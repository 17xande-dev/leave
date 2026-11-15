package mcpserver

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func TestRangesThroughMCP(t *testing.T) {
	f := newFixture(t)
	staff, hr := f.as("staff1"), f.as("hr")
	ranges := []DateRange{{"2026-10-12", "2026-10-13"}, {"2026-10-15", "2026-10-16"}}
	args := map[string]any{"type": "annual", "ranges": ranges}
	p := f.ok(staff, "preview_request", args)
	if p.Data["ok"] != true || p.Data["days"] != float64(4) || p.Data["start"] != "2026-10-12" || p.Data["end"] != "2026-10-16" {
		t.Fatalf("preview ranges = %v", p.Data)
	}
	b, _ := json.Marshal(p.Data["ranges"])
	var previewRanges []DateRange
	if err := json.Unmarshal(b, &previewRanges); err != nil || !slices.Equal(previewRanges, ranges) {
		t.Fatalf("preview ranges = %s, want %v (%v)", b, ranges, err)
	}
	r := f.ok(staff, "apply_for_leave", args)
	assertRanges := func(r result, want []DateRange) {
		t.Helper()
		got, _ := json.Marshal(get(r.Data, "request", "ranges"))
		var decoded []DateRange
		if err := json.Unmarshal(got, &decoded); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(decoded, want) {
			t.Fatalf("ranges = %s, want %v", got, want)
		}
	}
	assertRanges(r, ranges)
	assertRanges(f.ok(staff, "get_request", map[string]any{"id": id(r)}), ranges)
	changed := []DateRange{{"2026-10-12", "2026-10-12"}, {"2026-10-15", "2026-10-16"}}
	e := f.ok(staff, "edit_request", map[string]any{"id": id(r), "type": "annual", "ranges": changed})
	assertRanges(e, changed)
	if get(e.Data, "request", "days") != float64(3) {
		t.Fatalf("edit = %v", e.Data)
	}
	// The shorthand still defaults end to start and returns its one actual range.
	single := f.ok(staff, "edit_request", map[string]any{"id": id(r), "type": "annual", "start": "2026-10-12"})
	assertRanges(single, []DateRange{{"2026-10-12", "2026-10-12"}})
	assertRanges(f.ok(staff, "apply_for_leave", map[string]any{"type": "annual", "start": "2026-10-19"}), []DateRange{{"2026-10-19", "2026-10-19"}})

	recorded := []DateRange{{"2026-06-01", "2026-06-02"}, {"2026-07-06", "2026-07-07"}}
	rec := f.ok(hr, "record_leave", map[string]any{"person": "staff2", "type": "annual", "ranges": recorded, "note": "HR log", "half_start": true, "half_end": true})
	assertRanges(rec, recorded)
	if get(rec.Data, "request", "days") != float64(3) || get(rec.Data, "request", "half_start") != true || get(rec.Data, "request", "half_end") != true {
		t.Fatalf("record ranges with endpoint halves = %v", rec.Data)
	}
	assertRanges(f.ok(hr, "record_leave", map[string]any{"person": "staff2", "type": "annual", "start": "2026-08-03", "note": "HR log"}), []DateRange{{"2026-08-03", "2026-08-03"}})

	for _, tool := range []string{"preview_request", "apply_for_leave", "edit_request", "record_leave"} {
		cs := staff
		mixed := map[string]any{"type": "annual", "start": "2026-10-12", "ranges": ranges}
		if tool == "edit_request" {
			mixed["id"] = id(r)
		}
		if tool == "record_leave" {
			cs = hr
			mixed["person"], mixed["note"] = "staff2", "HR log"
		}
		f.refused(cs, tool, mixed, "Use either ranges or first and last day, not both.")
		delete(mixed, "start")
		mixed["end"] = "2026-10-16"
		f.refused(cs, tool, mixed, "Use either ranges or first and last day, not both.")
	}
}

func TestMCPRangeLimitsAndAwayGaps(t *testing.T) {
	f := newFixture(t)
	staff, hr := f.as("staff1"), f.as("hr")
	rid := id(f.ok(staff, "apply_for_leave", map[string]any{"type": "annual", "start": "2026-10-05"}))
	cases := []struct {
		ranges []DateRange
		msg    string
	}{
		{[]DateRange{{"2026-10-12", "2026-10-12"}, {"2026-10-14", "2026-10-14"}, {"2026-10-16", "2026-10-16"}, {"2026-10-19", "2026-10-19"}}, "at most 3"},
		{[]DateRange{{"2026-10-12", "2026-10-12"}, {"2026-11-13", "2026-11-13"}}, "31"},
	}
	for _, tc := range cases {
		args := map[string]any{"type": "annual", "ranges": tc.ranges}
		p := f.ok(staff, "preview_request", args)
		if p.Data["ok"] != false || !strings.Contains(p.Text, tc.msg) {
			t.Fatalf("preview = %v / %s, want %q", p.Data, p.Text, tc.msg)
		}
		f.refused(staff, "apply_for_leave", args, tc.msg)
		args["id"] = rid
		f.refused(staff, "edit_request", args, tc.msg)
	}
	f.refused(hr, "record_leave", map[string]any{"person": "staff2", "type": "annual", "note": "HR log", "ranges": []DateRange{{"2026-06-01", "2026-06-01"}, {"2026-06-03", "2026-06-03"}, {"2026-06-05", "2026-06-05"}, {"2026-06-08", "2026-06-08"}}}, "at most 3")
	f.refused(staff, "apply_for_leave", map[string]any{"type": "maternity", "ranges": []DateRange{{"2026-10-12", "2026-10-13"}, {"2026-10-15", "2026-10-16"}}}, "one continuous stretch")
	f.ok(staff, "apply_for_leave", map[string]any{"type": "annual", "ranges": []DateRange{{"2026-10-12", "2026-10-13"}, {"2026-10-15", "2026-10-16"}}})
	gap := f.ok(hr, "who_is_away", map[string]any{"from": "2026-10-14", "to": "2026-10-14"})
	if len(gap.Data["departments"].([]any)) != 0 {
		t.Fatalf("gap reported away = %v", gap.Data)
	}
	away := f.ok(hr, "who_is_away", map[string]any{"from": "2026-10-12", "to": "2026-10-16"})
	depts := away.Data["departments"].([]any)
	if len(depts) != 1 {
		t.Fatalf("away = %v", away.Data)
	}
	people := depts[0].(map[string]any)["people"].([]any)
	if len(people) != 2 || people[0].(map[string]any)["end"] != "2026-10-13" || people[1].(map[string]any)["start"] != "2026-10-15" {
		t.Fatalf("away ranges = %v", people)
	}
}

func TestMCPRangeSchemasAndDescriptions(t *testing.T) {
	f := newFixture(t)
	listed, err := f.as("staff1").ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, tool := range listed.Tools {
		switch tool.Name {
		case "preview_request", "apply_for_leave", "edit_request", "record_leave":
			seen++
			b, err := json.Marshal(tool.InputSchema)
			if err != nil {
				t.Fatal(err)
			}
			var schema struct {
				Required   []string                   `json:"required"`
				Properties map[string]json.RawMessage `json:"properties"`
			}
			if err := json.Unmarshal(b, &schema); err != nil {
				t.Fatal(err)
			}
			for _, field := range schema.Required {
				if field == "start" || field == "end" || field == "ranges" {
					t.Errorf("%s requires %s", tool.Name, field)
				}
			}
			if schema.Properties["ranges"] == nil || !strings.Contains(string(schema.Properties["ranges"]), `"start"`) || !strings.Contains(string(schema.Properties["ranges"]), `"end"`) {
				t.Errorf("%s ranges schema = %s", tool.Name, schema.Properties["ranges"])
			}
			for _, phrase := range []string{"workdays types only", "max 3", "31", "first day of the first range", "last day of the last range", "never both"} {
				if !strings.Contains(tool.Description, phrase) {
					t.Errorf("%s description lacks %q", tool.Name, phrase)
				}
			}
		}
	}
	if seen != 4 {
		t.Fatalf("checked %d tools, want 4", seen)
	}
}
