package leave

import (
	"database/sql"
	"reflect"
	"testing"
)

func assertRangeInvariants(t *testing.T, f *fixture) {
	t.Helper()
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
	r := f.mustSubmit("staff", "annual", "2026-10-12", "2026-10-13")
	assertRangeInvariants(t, f)
	var err error
	r, err = f.svc.Edit(f.ctx, f.people["staff"], r.ID, Input{TypeID: r.TypeID, Start: "2026-10-14", End: "2026-10-15"})
	if err != nil {
		t.Fatal(err)
	}
	assertRangeInvariants(t, f)
	if _, err = f.svc.Decide(f.ctx, f.people["head"], r.ID, true, ""); err != nil {
		t.Fatal(err)
	}
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
	if _, err = f.svc.RecordLeave(f.ctx, f.people["hr"], f.people["staff"].ID, Input{TypeID: r.TypeID, Start: "2026-09-01", End: "2026-09-02", Note: "log"}); err != nil {
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
	if _, err = f.svc.SubmitDraft(f.ctx, f.people["staff"], n.ID, ""); err != nil {
		t.Fatal(err)
	}
	assertRangeInvariants(t, f)
}

func TestInsertRangesReplacesTriggerAndNormalises(t *testing.T) {
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
		if n != 1 {
			t.Fatalf("trigger rows=%d", n)
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

func TestShutdownDeletionCascadesRangesForOldAndNewBinaries(t *testing.T) {
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
				// This is the unchanged old DeleteShutdown request deletion.
				if _, err := f.db.Exec(`DELETE FROM requests WHERE shutdown_part_id IN (SELECT id FROM shutdown_parts WHERE shutdown_id=?)`, id); err != nil {
					t.Fatal(err)
				}
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

func TestEditRangesWithTransitionalTriggers(t *testing.T) {
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
	f.december(false)
	draft := f.drafts("staff")[0]
	want := []Range{{"2026-12-08", "2026-12-09"}, {"2026-12-11", "2026-12-11"}}
	if err := f.svc.withTx(f.ctx, func(tx *sql.Tx) error { return insertRanges(tx, draft.ID, want) }); err != nil {
		t.Fatal(err)
	}
	r, err := f.svc.SubmitDraft(f.ctx, f.people["staff"], draft.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(r.Ranges, want) {
		t.Fatalf("submitted draft ranges=%v want %v", r.Ranges, want)
	}
	assertRangeInvariants(t, f)
}
