package leave

import (
	"encoding/json"
	"math"
	"testing"
)

func TestDaysString(t *testing.T) {
	for _, c := range []struct {
		d    Days
		want string
	}{
		{1500, "15"}, {150, "1.5"}, {1033, "10.33"}, {-144, "-1.44"}, {50, "0.5"},
		{5, "0.05"}, {-50, "-0.5"}, {0, "0"}, {100, "1"}, {-100, "-1"}, {-5, "-0.05"},
		{math.MinInt64, "-92233720368547758.08"},
	} {
		if got := c.d.String(); got != c.want {
			t.Errorf("Days(%d).String() = %q, want %q", int64(c.d), got, c.want)
		}
	}
}

func TestParseDays(t *testing.T) {
	for _, c := range []struct {
		in   string
		want Days
	}{
		{"15", 1500}, {"1.5", 150}, {"10.33", 1033}, {"-1.44", -144}, {"+2", 200},
		{".5", 50}, {"2.", 200}, {" 3 ", 300}, {"0", 0}, {"-0.05", -5}, {"0.05", 5}, {"1.50", 150},
	} {
		got, err := ParseDays(c.in)
		if err != nil || got != c.want {
			t.Errorf("ParseDays(%q) = %d, %v; want %d", c.in, int64(got), err, int64(c.want))
		}
	}
	for _, in := range []string{
		"", " ", "-", "+", ".", "1,5", "1e2", "1.234", "abc", "1 000", "--1", "1.-5",
		"99999999999999999999", "92233720368547759", "0x10", "1.5.2",
	} {
		if got, err := ParseDays(in); err == nil {
			t.Errorf("ParseDays(%q) = %d, want an error", in, int64(got))
		}
	}
}

func TestDaysRoundTrip(t *testing.T) {
	for _, d := range []Days{0, 1, -1, 5, 50, 99, 100, 101, 1033, -144, 123456789, -987654321} {
		got, err := ParseDays(d.String())
		if err != nil || got != d {
			t.Errorf("round trip of %d went through %q to %d, %v", int64(d), d.String(), int64(got), err)
		}
	}
}

func TestDaysJSON(t *testing.T) {
	b, err := json.Marshal(struct {
		A Days  `json:"a"`
		B *Days `json:"b"`
	}{A: 150, B: nil})
	if err != nil || string(b) != `{"a":1.5,"b":null}` {
		t.Fatalf("marshal = %s, %v", b, err)
	}
	var out struct {
		A Days  `json:"a"`
		B *Days `json:"b"`
		C Days  `json:"c"`
	}
	if err := json.Unmarshal([]byte(`{"a": 10.33, "b": -1.44, "c": "2.5"}`), &out); err != nil {
		t.Fatal(err)
	}
	if out.A != 1033 || out.B == nil || *out.B != -144 || out.C != 250 {
		t.Fatalf("unmarshal = %+v", out)
	}
	for _, bad := range []string{`{"a": 1.234}`, `{"a": 1e2}`, `{"a": "x"}`, `{"a": true}`, `{"a": ""}`} {
		var v struct{ A Days }
		if err := json.Unmarshal([]byte(bad), &v); err == nil {
			t.Errorf("unmarshal %s: want an error, got %+v", bad, v)
		}
	}
}

func TestDaysLabel(t *testing.T) {
	for _, c := range []struct {
		d    Days
		unit string
		want string
	}{
		{100, UnitWorkdays, "1 working day"}, {150, UnitWorkdays, "1.5 working days"},
		{50, UnitWorkdays, "0.5 working days"}, {0, UnitWorkdays, "0 working days"},
		{100, UnitCalendar, "1 day"}, {1200, UnitCalendar, "12 days"},
		{100, UnitWeekend, "1 weekend"}, {200, UnitWeekend, "2 weekends"}, {-100, UnitWeekend, "-1 weekend"},
	} {
		if got := c.d.Label(c.unit); got != c.want {
			t.Errorf("Days(%d).Label(%s) = %q, want %q", int64(c.d), c.unit, got, c.want)
		}
	}
}
