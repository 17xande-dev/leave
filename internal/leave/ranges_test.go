package leave

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func assertRangeInvariants(t *testing.T, f *fixture) {
	t.Helper()
	var orphans int
	if err := f.db.QueryRow(`SELECT count(*) FROM request_ranges rr WHERE NOT EXISTS (SELECT 1 FROM requests r WHERE r.id=rr.request_id)`).Scan(&orphans); err != nil || orphans != 0 {
		t.Fatalf("orphan ranges=%d %v", orphans, err)
	}
	rows, err := f.db.Query(`SELECT r.id,r.start_date,r.end_date,rr.start_date,rr.end_date FROM requests r LEFT JOIN request_ranges rr ON rr.request_id=r.id ORDER BY r.id,rr.start_date`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var lastID int64
	var lastEnd, spanEnd string
	count := 0
	for rows.Next() {
		var id int64
		var start, end string
		var rs, re sql.NullString
		if err := rows.Scan(&id, &start, &end, &rs, &re); err != nil {
			t.Fatal(err)
		}
		if !rs.Valid || !re.Valid {
			t.Fatalf("request %d missing range", id)
		}
		if id != lastID {
			if lastID != 0 && lastEnd != spanEnd {
				t.Fatalf("request %d span end %s != %s", lastID, spanEnd, lastEnd)
			}
			if rs.String != start {
				t.Fatalf("request %d span start %s != %s", id, start, rs.String)
			}
		} else {
			d, _ := ParseDate(lastEnd)
			if rs.String <= fmtDate(d.AddDate(0, 0, 1)) {
				t.Fatalf("request %d overlapping/adjacent ranges", id)
			}
		}
		if re.String < rs.String {
			t.Fatalf("request %d reversed range", id)
		}
		lastID, lastEnd, spanEnd = id, re.String, end
		count++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if count == 0 {
		t.Fatal("no requests checked")
	}
	if lastEnd != spanEnd {
		t.Fatalf("request %d span end %s != %s", lastID, spanEnd, lastEnd)
	}
}

func TestRangeInvariantsEveryWriter(t *testing.T) {
	f := newFixture(t)
	var triggers int
	if err := f.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='trigger' AND name IN ('requests_default_range','requests_sync_range')`).Scan(&triggers); err != nil || triggers != 0 {
		t.Fatalf("transitional triggers=%d %v", triggers, err)
	}
	r, err := f.svc.Submit(f.ctx, f.people["staff"], Input{TypeID: f.types["annual"].ID, Ranges: []Range{{"2026-10-12", "2026-10-13"}, {"2026-10-15", "2026-10-16"}}})
	if err != nil {
		t.Fatal(err)
	}
	assertRangeInvariants(t, f)
	r, err = f.svc.Edit(f.ctx, f.people["staff"], r.ID, Input{TypeID: r.TypeID, Start: "2026-10-14", End: "2026-10-15"})
	if err != nil {
		t.Fatal(err)
	}
	assertRangeInvariants(t, f)
	r, err = f.svc.Edit(f.ctx, f.people["staff"], r.ID, Input{TypeID: r.TypeID, Ranges: []Range{{"2026-10-14", "2026-10-15"}, {"2026-10-19", "2026-10-20"}}})
	if err != nil {
		t.Fatal(err)
	}
	assertRangeInvariants(t, f)
	r, err = f.svc.Edit(f.ctx, f.people["staff"], r.ID, Input{TypeID: r.TypeID, Start: "2026-10-14", End: "2026-10-15"})
	if err != nil {
		t.Fatal(err)
	}
	assertRangeInvariants(t, f)
	if _, err = f.svc.Decide(f.ctx, f.people["head"], r.ID, true, ""); err != nil {
		t.Fatal(err)
	}
	assertRangeInvariants(t, f)
	if err = f.svc.SetRangeEventID(f.ctx, r.ID, r.Start, "original-event"); err != nil {
		t.Fatal(err)
	}
	c, err := f.svc.RequestCancellation(f.ctx, f.people["staff"], r.ID, "cancel")
	if err != nil {
		t.Fatal(err)
	}
	assertRangeInvariants(t, f)
	if !reflect.DeepEqual(c.Ranges, r.Ranges) {
		t.Fatalf("cancellation ranges=%v want %v", c.Ranges, r.Ranges)
	}
	cr, err := f.svc.CalendarRanges(f.ctx, c.ID)
	if err != nil || len(cr) != 1 || cr[0].EventID != "" {
		t.Fatalf("copied event id: %v %v", cr, err)
	}
	if _, err = f.svc.RecordLeave(f.ctx, f.people["hr"], f.people["staff"].ID, Input{TypeID: r.TypeID, Ranges: []Range{{"2026-09-01", "2026-09-02"}, {"2026-09-07", "2026-09-08"}}, Note: "log"}); err != nil {
		t.Fatal(err)
	}
	assertRangeInvariants(t, f)
	if _, err = f.svc.ImportLeave(f.ctx, f.people["hr"], f.people["staff"].ID, Input{TypeID: r.TypeID, Start: "2026-11-02", End: "2026-11-03", Note: "log"}, "import"); err != nil {
		t.Fatal(err)
	}
	assertRangeInvariants(t, f)
	f.december(false)
	assertRangeInvariants(t, f)
	draft := f.drafts("staff")[0]
	if _, err = f.svc.SubmitDraft(f.ctx, f.people["staff"], draft.ID, ""); err != nil {
		t.Fatal(err)
	}
	assertRangeInvariants(t, f)
	// HR's ordinary draft path, separate from shutdown's mandatory drafts.
	n, err := f.negative("staff", "annual", "2026-11-09", "2026-11-30", "need more")
	if err != nil {
		t.Fatal(err)
	}
	if n.Status != StatusAwaitingHR {
		t.Fatalf("status=%s", n.Status)
	}
	assertRangeInvariants(t, f)
	if _, err = f.svc.GrantNegative(f.ctx, f.people["hr"], n.ID, "agreed"); err != nil {
		t.Fatal(err)
	}
	assertRangeInvariants(t, f)
	if _, err = f.svc.SubmitDraft(f.ctx, f.people["staff"], n.ID, ""); err != nil {
		t.Fatal(err)
	}
	assertRangeInvariants(t, f)
}

func TestRangeInvariantsStatusWriters(t *testing.T) {
	for _, path := range []string{"cancel", "decline", "withdraw", "unrecord", "unimport", "halves", "refuse-negative", "delete-shutdown"} {
		t.Run(path, func(t *testing.T) {
			f := newHalfFixture(t)
			in := Input{TypeID: f.types["annual"].ID, Ranges: []Range{{"2026-11-02", "2026-11-03"}, {"2026-11-05", "2026-11-06"}}, Note: "log"}
			var r Request
			var err error
			switch path {
			case "unrecord":
				in.Ranges = []Range{{"2026-09-01", "2026-09-02"}, {"2026-09-07", "2026-09-08"}}
				r, err = f.svc.RecordLeave(f.ctx, f.people["hr"], f.people["staff"].ID, in)
			case "unimport", "halves":
				r, err = f.svc.ImportLeave(f.ctx, f.people["hr"], f.people["staff"].ID, in, ImportedNotePrefix+"1)")
			case "refuse-negative":
				f.exhaustAnnual()
				in.Negative = true
				r, err = f.svc.Submit(f.ctx, f.people["staff"], in)
			default:
				r, err = f.svc.Submit(f.ctx, f.people["staff"], in)
			}
			if err != nil {
				t.Fatal(err)
			}
			assertRangeInvariants(t, f)
			switch path {
			case "cancel":
				_, err = f.svc.Decide(f.ctx, f.people["head"], r.ID, true, "")
				if err != nil {
					t.Fatal(err)
				}
				assertRangeInvariants(t, f)
				r, err = f.svc.RequestCancellation(f.ctx, f.people["staff"], r.ID, "cancel")
				if err != nil {
					t.Fatal(err)
				}
				assertRangeInvariants(t, f)
				_, err = f.svc.Decide(f.ctx, f.people["head"], r.ID, true, "")
			case "decline":
				_, err = f.svc.Decide(f.ctx, f.people["head"], r.ID, false, "no")
			case "withdraw":
				err = f.svc.Withdraw(f.ctx, f.people["staff"], r.ID)
			case "unrecord":
				_, err = f.svc.Unrecord(f.ctx, f.people["hr"], r.ID)
			case "unimport":
				_, err = f.svc.UnimportLeave(f.ctx, f.people["hr"], r.ID)
			case "halves":
				_, err = f.svc.SetHalves(f.ctx, f.people["hr"], r.ID, true, true)
			case "refuse-negative":
				_, err = f.svc.RefuseNegative(f.ctx, f.people["hr"], r.ID, "no")
			case "delete-shutdown":
				id := f.december(false)
				assertRangeInvariants(t, f)
				err = f.svc.DeleteShutdown(f.ctx, f.people["hr"], id)
			}
			if err != nil {
				t.Fatal(err)
			}
			assertRangeInvariants(t, f)
			var orphans int
			if err := f.db.QueryRow(`SELECT count(*) FROM request_ranges rr WHERE NOT EXISTS (SELECT 1 FROM requests r WHERE r.id=rr.request_id)`).Scan(&orphans); err != nil || orphans != 0 {
				t.Fatalf("orphan ranges=%d %v", orphans, err)
			}
		})
	}
}

func TestCalendarRangesDoesNotTakeWriteLock(t *testing.T) {
	f := newFixture(t)
	r := f.mustSubmit("staff", "annual", "2026-11-02", "2026-11-03")
	conn, err := f.db.Conn(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(f.ctx, `BEGIN IMMEDIATE`); err != nil {
		t.Fatal(err)
	}
	defer conn.ExecContext(f.ctx, `ROLLBACK`)
	ctx, cancel := context.WithTimeout(f.ctx, time.Second)
	defer cancel()
	got, err := f.svc.CalendarRanges(ctx, r.ID)
	if err != nil || len(got) != 1 || got[0].Range != r.Ranges[0] {
		t.Fatalf("read under write lock: %v %v", got, err)
	}
	if _, err := f.svc.CalendarRanges(ctx, -1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown request: %v", err)
	}
}

func TestSubmitDraftRangeRewriteAtomic(t *testing.T) {
	f := newFixture(t)
	f.december(false)
	d := f.drafts("staff")[0]
	// SubmitDraft deliberately rewrites stored ranges. A failed rewrite must
	// not commit a status change while leaving the old bookkeeping behind.
	if _, err := f.db.Exec(`CREATE TRIGGER refuse_range_rewrite BEFORE DELETE ON request_ranges BEGIN SELECT RAISE(ABORT,'range rewrite failed'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.SubmitDraft(f.ctx, f.people["staff"], d.ID, ""); err == nil || !strings.Contains(err.Error(), "range rewrite failed") {
		t.Fatalf("rewrite failure ignored: %v", err)
	}
	got, err := f.svc.Get(f.ctx, d.ID)
	if err != nil || got.Status != StatusDraft || !reflect.DeepEqual(got.Ranges, d.Ranges) {
		t.Fatalf("failed rewrite changed draft: %+v %v", got, err)
	}
	assertRangeInvariants(t, f)
}

func TestInsertRangesNormalises(t *testing.T) {
	f := newFixture(t)
	var id int64
	err := f.svc.withTx(f.ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec(`INSERT INTO requests(user_id,type_id,kind,start_date,end_date,days_x100,cycle_start,status,created_at) VALUES(?,?,'leave','2026-11-01','2026-11-30',100,'2026-01-01','pending','')`, f.people["staff"].ID, f.types["annual"].ID)
		if err != nil {
			return err
		}
		id, _ = res.LastInsertId()
		var n int
		if err := tx.QueryRow(`SELECT count(*) FROM request_ranges WHERE request_id=?`, id).Scan(&n); err != nil {
			return err
		}
		if n != 0 {
			t.Fatalf("trigger rows=%d", n)
		}
		if _, err := f.svc.get(f.ctx, tx, id); err == nil || !strings.Contains(err.Error(), "has no stored ranges") {
			t.Fatalf("range-less read: %v", err)
		}
		return insertRanges(tx, id, []Range{{"2026-11-12", "2026-11-13"}, {"2026-11-03", "2026-11-04"}, {"2026-11-02", "2026-11-03"}, {"2026-11-05", "2026-11-06"}})
	})
	if err != nil {
		t.Fatal(err)
	}
	r, err := f.svc.Get(f.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	want := []Range{{"2026-11-02", "2026-11-06"}, {"2026-11-12", "2026-11-13"}}
	if !reflect.DeepEqual(r.Ranges, want) {
		t.Fatalf("ranges=%v want %v", r.Ranges, want)
	}
	assertRangeInvariants(t, f)
	if err := f.svc.withTx(f.ctx, func(tx *sql.Tx) error { return insertRanges(tx, id, nil) }); err == nil {
		t.Fatal("empty ranges accepted")
	}
	r, err = f.svc.Get(f.ctx, id)
	if err != nil || !reflect.DeepEqual(r.Ranges, want) {
		t.Fatalf("failed replacement lost data: %v %v", r, err)
	}
}

func TestInputRangeShorthand(t *testing.T) {
	f := newFixture(t)
	in := Input{TypeID: f.types["annual"].ID, Ranges: []Range{{"2026-11-02", "2026-11-03"}}}
	r, err := f.svc.Submit(f.ctx, f.people["staff"], in)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(r.Ranges, in.Ranges) || r.Start != "2026-11-02" || r.End != "2026-11-03" {
		t.Fatalf("request=%+v", r)
	}
	in.Start = "2026-11-02"
	if _, err = f.svc.Preview(f.ctx, f.people["staff"], in); !fieldErr(err, "start") {
		t.Fatalf("both inputs: %v", err)
	}
}

func TestSingleRangeFingerprintUnchanged(t *testing.T) {
	r := Request{ID: 42, Kind: KindLeave, TypeID: 7, Start: "2026-11-16", End: "2026-11-18", Days: 250, HalfStart: true, Note: "family"}
	const want = "fe3c9475b7c5c5aa853ca38f"
	if got := r.Fingerprint(); got != want {
		t.Fatalf("fingerprint=%s want %s", got, want)
	}
	r.Ranges = []Range{{r.Start, r.End}}
	if got := r.Fingerprint(); got != want {
		t.Fatalf("loaded single range changed fingerprint=%s", got)
	}
}

func TestShutdownDeletionRequiresExplicitRanges(t *testing.T) {
	for _, oldBinary := range []bool{false, true} {
		t.Run(map[bool]string{false: "service", true: "old-binary"}[oldBinary], func(t *testing.T) {
			f := newFixture(t)
			id := f.december(false)
			var before int
			if err := f.db.QueryRow(`SELECT count(*) FROM request_ranges`).Scan(&before); err != nil {
				t.Fatal(err)
			}
			if before == 0 {
				t.Fatal("no shutdown ranges created")
			}
			if oldBinary {
				// Pre-range binaries are no longer supported: a raw delete
				// must refuse, not silently destroy the range bookkeeping.
				if _, err := f.db.Exec(`DELETE FROM requests WHERE shutdown_part_id IN (SELECT id FROM shutdown_parts WHERE shutdown_id=?)`, id); err == nil || !strings.Contains(err.Error(), "FOREIGN KEY") {
					t.Fatalf("raw deletion: %v", err)
				}
				assertRangeInvariants(t, f)
				return
			} else if err := f.svc.DeleteShutdown(f.ctx, f.people["hr"], id); err != nil {
				t.Fatal(err)
			}
			var remaining int
			if err := f.db.QueryRow(`SELECT count(*) FROM request_ranges`).Scan(&remaining); err != nil {
				t.Fatal(err)
			}
			if remaining != 0 {
				t.Fatalf("shutdown left %d ranges", remaining)
			}
		})
	}
}

func TestEditRangesWithoutTransitionalTriggers(t *testing.T) {
	f := newFixture(t)
	r := f.mustSubmit("staff", "annual", "2026-11-02", "2026-11-03")
	// Exercise both directions: one range to several, then several to one.
	want := []Range{{"2026-11-04", "2026-11-05"}, {"2026-11-10", "2026-11-11"}}
	r, err := f.svc.Edit(f.ctx, f.people["staff"], r.ID, Input{TypeID: r.TypeID, Ranges: want})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(r.Ranges, want) {
		t.Fatalf("edited ranges=%v want %v", r.Ranges, want)
	}
	assertRangeInvariants(t, f)
	// Keeping the span while changing its gaps is still an edit.
	want = []Range{{"2026-11-04", "2026-11-06"}, {"2026-11-10", "2026-11-11"}}
	r, err = f.svc.Edit(f.ctx, f.people["staff"], r.ID, Input{TypeID: r.TypeID, Ranges: want})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(r.Ranges, want) {
		t.Fatalf("same-span edited ranges=%v want %v", r.Ranges, want)
	}
	assertRangeInvariants(t, f)
	want = []Range{{"2026-11-12", "2026-11-13"}}
	r, err = f.svc.Edit(f.ctx, f.people["staff"], r.ID, Input{TypeID: r.TypeID, Ranges: want})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(r.Ranges, want) {
		t.Fatalf("edited ranges=%v want %v", r.Ranges, want)
	}
	assertRangeInvariants(t, f)
}

func TestSubmitDraftPreservesStoredRanges(t *testing.T) {
	f := newFixture(t)
	want := []Range{{"2026-12-08", "2026-12-09"}, {"2026-12-11", "2026-12-11"}}
	f.exhaustAnnual()
	draft, err := f.svc.Submit(f.ctx, f.people["staff"], Input{TypeID: f.types["annual"].ID, Ranges: want, Negative: true, Note: "needed"})
	if err != nil {
		t.Fatal(err)
	}
	assertRangeInvariants(t, f)
	if _, err := f.svc.GrantNegative(f.ctx, f.people["hr"], draft.ID, "agreed"); err != nil {
		t.Fatal(err)
	}
	r, err := f.svc.SubmitDraft(f.ctx, f.people["staff"], draft.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(r.Ranges, want) {
		t.Fatalf("submitted draft ranges=%v want %v", r.Ranges, want)
	}
	if r.Days != WholeDays(3) {
		t.Fatalf("draft re-quoted its gap as leave: days=%s", r.Days)
	}
	assertRangeInvariants(t, f)
}
