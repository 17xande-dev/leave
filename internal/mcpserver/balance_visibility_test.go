package mcpserver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/17xande-dev/leave/internal/dbtest"
	"github.com/17xande-dev/leave/internal/directory"
	"github.com/17xande-dev/leave/internal/leave"
	"github.com/17xande-dev/leave/internal/users"
)

// Exercise the real tool functions and their JSON without a listening socket;
// the existing HTTP fixture tests transport and bearer-token handling separately.
func TestBalanceVisibilityToolResults(t *testing.T) {
	ctx := context.Background()
	db := dbtest.New(t)
	us := users.NewStore(db)
	svc := leave.New(db, us, leave.Config{Location: time.UTC})
	svc.Now = func() time.Time { return time.Date(2026, 10, 5, 8, 0, 0, 0, time.UTC) }
	if err := svc.SeedTypes(ctx); err != nil {
		t.Fatal(err)
	}
	staff, err := us.Upsert(ctx, directory.User{ID: "staff", DN: "CN=Staff", Username: "staff", Name: "Staff", Email: "staff@example.test"})
	if err != nil {
		t.Fatal(err)
	}
	hr, err := us.Upsert(ctx, directory.User{ID: "hr", DN: "CN=HR", Username: "hr", Name: "HR", Email: "hr@example.test", Admin: true})
	if err != nil {
		t.Fatal(err)
	}
	d := &deps{leave: svc, users: us}
	for _, code := range []string{"compassionate", "unpaid"} {
		ty, err := d.resolveType(ctx, code, staff)
		if err != nil {
			t.Fatal(err)
		}
		want := leave.MsgAdminOnlyType(ty)
		for _, u := range []users.User{staff, hr} {
			preview, _, err := d.previewRequest(ctx, u, RequestIn{Type: code, Start: "2026-10-12", End: "2026-10-12", Note: "reason"})
			if err != nil || preview.OK || len(preview.Problems) != 1 || preview.Problems[0].Field != "type" || preview.Problems[0].Message != want {
				t.Fatalf("admin-only preview: %+v %v", preview, err)
			}
			if _, _, err := d.apply(ctx, u, ApplyIn{Type: code, Start: "2026-10-12", End: "2026-10-12", Note: "reason"}); err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("admin-only apply: %v", err)
			}
		}
	}
	if _, err := d.input(ctx, staff, RequestIn{Type: "unknown"}); err == nil || strings.Contains(err.Error(), "compassionate") || strings.Contains(err.Error(), "unpaid") || !strings.Contains(err.Error(), "parental") {
		t.Fatalf("applicant hint: %v", err)
	}
	if _, err := d.resolveType(ctx, "unknown", staff); err == nil || !strings.Contains(err.Error(), "compassionate") {
		t.Fatalf("admin adjustment hint lost types: %v", err)
	}
	record, _, err := d.recordLeave(ctx, hr, RecordLeaveIn{Person: "staff", Type: "unpaid", Start: "2026-10-02", End: "2026-10-02", Note: "HR record"})
	if err != nil || record.Request.Status != leave.StatusApproved {
		t.Fatalf("admin-only record: %+v %v", record, err)
	}
	bs, _, err := d.myBalances(ctx, staff, none{})
	if err != nil {
		t.Fatal(err)
	}
	if len(bs.Balances) != 3 || len(bs.OtherTypes) != 2 {
		t.Fatalf("my_balances = %+v", bs)
	}
	hidden := map[string]bool{"compassionate": true, "unpaid": true, "parental": true, "maternity": true}
	for _, b := range bs.Balances {
		if hidden[b.Code] {
			t.Errorf("hidden %s has a visible balance", b.Code)
		}
	}
	delete(hidden, "compassionate")
	delete(hidden, "unpaid")
	for _, other := range bs.OtherTypes {
		if !hidden[other.Code] || other.Name == "" || other.Unit == "" {
			t.Errorf("bad other_types entry: %+v", other)
		}
		delete(hidden, other.Code)
	}
	if len(hidden) != 0 {
		t.Errorf("missing hidden types: %v", hidden)
	}
	encoded, err := json.Marshal(bs.OtherTypes)
	if err != nil || strings.Contains(string(encoded), "available") || strings.Contains(string(encoded), "allowance") {
		t.Fatalf("other_types leaked a balance: %s, %v", encoded, err)
	}

	for _, code := range []string{"parental", "annual"} {
		preview, _, err := d.previewRequest(ctx, staff, RequestIn{Type: code, Start: "2026-10-12", End: "2026-10-13"})
		if err != nil || !preview.OK || preview.Days != leave.WholeDays(2) {
			t.Fatalf("%s preview = %+v, %v", code, preview, err)
		}
		encoded, err := json.Marshal(preview)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Contains(string(encoded), "available_after"); got != (code == "annual") {
			t.Errorf("%s available_after presence: %s", code, encoded)
		}
		if got := strings.Contains(string(encoded), "available_before"); got != (code == "annual") {
			t.Errorf("%s available_before presence: %s", code, encoded)
		}
	}
	refusal, _, err := d.previewRequest(ctx, staff, RequestIn{Type: "parental", Start: "2026-10-12", End: "2026-10-22"})
	if err != nil || refusal.OK || len(refusal.Problems) == 0 || !strings.Contains(refusal.Problems[0].Message, "you have 10 left") {
		t.Fatalf("parental over-allowance refusal = %+v, %v", refusal, err)
	}
	applied, _, err := d.apply(ctx, staff, ApplyIn{Type: "parental", Start: "2026-10-12", End: "2026-10-13"})
	if err != nil || applied.Request.Type != "Parental leave" || applied.Request.Status != leave.StatusPending {
		t.Fatalf("parental apply = %+v, %v", applied, err)
	}
	encoded, err = json.Marshal(applied)
	if err != nil || strings.Contains(string(encoded), "available_after") {
		t.Fatalf("apply exposed available_after: %s, %v", encoded, err)
	}
	admin, _, err := d.personBalances(ctx, hr, PersonBalancesIn{Person: "staff"})
	if err != nil || len(admin.Balances) != 7 {
		t.Fatalf("person_balances = %+v, %v", admin, err)
	}
	var parental *BalanceInfo
	for i := range admin.Balances {
		if admin.Balances[i].Code == "parental" {
			parental = &admin.Balances[i]
		}
	}
	if parental == nil || parental.Available == nil || *parental.Available != leave.WholeDays(8) {
		t.Fatalf("admin parental balance = %+v", parental)
	}
	if err := us.SetSex(ctx, staff.ID, "m"); err != nil {
		t.Fatal(err)
	}
	bs, _, err = d.myBalances(ctx, staff, none{})
	if err != nil || len(bs.OtherTypes) != 1 {
		t.Fatalf("male my_balances = %+v, %v", bs, err)
	}
	for _, other := range bs.OtherTypes {
		if other.Code == "maternity" {
			t.Fatal("male other_types lists maternity")
		}
	}
	// Admin changes are data: turning the flag back on restores the balance
	// and removes the corresponding discoverability-only entry.
	types, err := svc.Types(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, ty := range types {
		if ty.Code == "parental" {
			ty.ShowBalance = true
			if _, err := svc.SaveType(ctx, ty); err != nil {
				t.Fatal(err)
			}
		}
	}
	bs, _, err = d.myBalances(ctx, staff, none{})
	if err != nil || len(bs.Balances) != 4 || len(bs.OtherTypes) != 0 {
		t.Fatalf("shown parental my_balances = %+v, %v", bs, err)
	}
}

func TestBalanceVisibilityToolSchemas(t *testing.T) {
	schema, err := json.Marshal(mustSchema[BalancesOut]())
	if err != nil || !strings.Contains(string(schema), "leave types you can also apply for; their balances are kept by HR") {
		t.Fatalf("balances schema = %s, %v", schema, err)
	}
	for _, schema := range []any{mustSchema[RequestIn](), mustSchema[ApplyIn](), mustSchema[EditIn]()} {
		encoded, err := json.Marshal(schema)
		if err != nil || !strings.Contains(string(encoded), "as listed by my_balances (balances or other_types)") {
			t.Errorf("request type schema = %s, %v", encoded, err)
		}
	}
}
