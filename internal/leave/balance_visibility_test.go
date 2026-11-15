package leave

import (
	"reflect"
	"testing"
)

func TestSeedBalanceVisibilityAndSaveType(t *testing.T) {
	// The fixture migrates an empty database before seeding, so the migration's
	// UPDATE cannot be responsible for these defaults.
	f := newFixture(t)
	hidden := map[string]bool{"compassionate": true, "unpaid": true, "parental": true, "maternity": true}
	types, err := f.svc.Types(f.ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(types) != 10 {
		t.Fatalf("seeded %d types, want 10", len(types))
	}
	for _, ty := range types {
		if ty.ShowBalance == hidden[ty.Code] {
			t.Errorf("%s ShowBalance = %v", ty.Code, ty.ShowBalance)
		}
	}

	// Exercise both INSERT values and both UPDATE directions, rather than
	// relying on the seed alone to prove the new column is persisted.
	for _, shown := range []bool{false, true} {
		ty := Type{Code: "custom-hidden", Name: "Custom", CycleMonths: 12, Unit: UnitWorkdays, Active: true, ShowBalance: shown}
		if shown {
			ty.Code = "custom-shown"
		}
		id, err := f.svc.SaveType(f.ctx, ty)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []bool{shown, !shown, shown} {
			got, err := f.svc.Type(f.ctx, id)
			if err != nil || got.ShowBalance != want {
				t.Fatalf("type %d ShowBalance = %v, want %v: %v", id, got.ShowBalance, want, err)
			}
			got.ShowBalance = !want
			if _, err := f.svc.SaveType(f.ctx, got); err != nil {
				t.Fatal(err)
			}
		}
	}
	parental := f.types["parental"]
	parental.ShowBalance = true
	if _, err := f.svc.SaveType(f.ctx, parental); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.SeedTypes(f.ctx); err != nil {
		t.Fatal(err)
	}
	got, err := f.svc.Type(f.ctx, parental.ID)
	if err != nil || !got.ShowBalance {
		t.Fatalf("seed overwrote admin visibility: %+v, %v", got, err)
	}
}

func TestVisibleBalancesKeepsFullApplyAndAdminSource(t *testing.T) {
	f := newFixture(t)
	bs, err := f.svc.Balances(f.ctx, f.people["staff"].ID)
	if err != nil {
		t.Fatal(err)
	}
	codes := func(bs []Balance) []string {
		var out []string
		for _, b := range bs {
			out = append(out, b.Type.Code)
		}
		return out
	}
	want := []string{"annual", "sick", "family", "maternity", "parental", "compassionate", "unpaid"}
	if got := codes(bs); !reflect.DeepEqual(got, want) {
		t.Fatalf("full balances = %v, want %v", got, want)
	}
	visible := VisibleBalances(bs)
	if got := codes(visible); !reflect.DeepEqual(got, []string{"annual", "sick", "family"}) {
		t.Fatalf("visible balances = %v", got)
	}
	visible[0].Type.Name = "Changed copy"
	if bs[0].Type.Name == "Changed copy" {
		t.Fatal("VisibleBalances reused the input backing slice")
	}
	if got := codes(bs); !reflect.DeepEqual(got, want) {
		t.Fatalf("filter changed full balances: %v", got)
	}
	f.setSex("staff", "m")
	bs, err = f.svc.Balances(f.ctx, f.people["staff"].ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := codes(bs); !reflect.DeepEqual(got, []string{"annual", "sick", "family", "parental", "compassionate", "unpaid"}) {
		t.Fatalf("male full balances = %v", got)
	}
	if len(VisibleBalances(nil)) != 0 {
		t.Fatal("nil balances did not filter to empty")
	}
}
