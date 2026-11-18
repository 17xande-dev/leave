package mcpserver

import (
	"context"
	"strings"
	"testing"
)

// A correction states the whole request, so a field left out is cleared, not
// kept. The tool description says so, and the behaviour is pinned beside it.
func TestCorrectRequestOmittedFieldsClear(t *testing.T) {
	f := newFixture(t)
	staff, hr := f.as("staff1"), f.as("hr")
	tools, err := hr.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, tl := range tools.Tools {
		if tl.Name == "correct_request" {
			found = true
			if !strings.Contains(tl.Description, "omitting the note or the half-day flags clears them") {
				t.Errorf("description does not warn that omitted fields clear: %s", tl.Description)
			}
		}
	}
	if !found {
		t.Fatal("correct_request is not listed")
	}
	r := f.ok(staff, "apply_for_leave", map[string]any{"type": "annual", "start": "2026-10-12", "end": "2026-10-14", "note": "keep me?"})
	out := f.ok(hr, "correct_request", map[string]any{"id": id(r), "type": "annual", "start": "2026-10-12", "end": "2026-10-13", "reason": "shorter"})
	if n, _ := get(out.Data, "request", "note").(string); n != "" {
		t.Fatalf("an omitted note was kept: %q", n)
	}
}

func TestCorrectRequestTool(t *testing.T) {
	f := newFixture(t)
	staff, hr := f.as("staff1"), f.as("hr")
	r := f.ok(staff, "apply_for_leave", map[string]any{"type": "annual", "start": "2026-10-12", "end": "2026-10-14", "note": "n"})
	read := f.ok(hr, "get_request", map[string]any{"id": id(r)})
	fp, _ := read.Data["fingerprint"].(string)
	if read.Data["can_correct"] != true || read.Data["corrections"] != nil {
		t.Fatalf("before: %+v", read.Data)
	}
	if f.ok(staff, "get_request", map[string]any{"id": id(r)}).Data["can_correct"] != false {
		t.Fatal("the owner is told they can correct their own request")
	}
	f.refused(hr, "correct_request", map[string]any{"id": id(r), "type": "unpaid", "start": "2026-10-12", "end": "2026-10-14", "reason": "  "}, "Say why you are changing it")
	out := f.ok(hr, "correct_request", map[string]any{"id": id(r), "type": "compassionate", "start": "2026-10-13", "end": "2026-10-14",
		"note": "funeral", "reason": "It was a bereavement", "seen": fp})
	if get(out.Data, "request", "type") != "Compassionate leave" || get(out.Data, "request", "days") != float64(2) ||
		!strings.Contains(out.Text, "has been emailed") || get(out.Data, "request", "status") != "pending" {
		t.Fatalf("correct: %+v %s", out.Data, out.Text)
	}
	f.refused(hr, "correct_request", map[string]any{"id": id(r), "type": "unpaid", "start": "2026-10-13", "end": "2026-10-14", "note": "x", "reason": "again", "seen": fp},
		"This request changed since you read it")
	after := f.ok(staff, "get_request", map[string]any{"id": id(r)}).Data
	cs, _ := after["corrections"].([]any)
	if len(cs) != 1 {
		t.Fatalf("corrections: %+v", after["corrections"])
	}
	c := cs[0].(map[string]any)
	if c["by"] != "Hannah HR" || c["reason"] != "It was a bereavement" || !strings.Contains(c["before"].(string), "Annual leave, 2026-10-12 to 2026-10-14") ||
		!strings.Contains(c["after"].(string), "Compassionate leave, 2026-10-13 to 2026-10-14 (2 days)") {
		t.Fatalf("correction: %+v", c)
	}
	// Several stretches in, several out.
	two := f.ok(hr, "correct_request", map[string]any{"id": id(r), "type": "annual", "note": "n", "reason": "two stretches",
		"ranges": []map[string]any{{"start": "2026-10-12", "end": "2026-10-12"}, {"start": "2026-10-19", "end": "2026-10-19"}}})
	if rs, _ := get(two.Data, "request", "ranges").([]any); len(rs) != 2 {
		t.Fatalf("ranges: %+v", two.Data)
	}
	// Someone else's request only: an admin's own is corrected by another admin.
	f.makeAdmin("staff1")
	mine := f.ok(staff, "apply_for_leave", map[string]any{"type": "annual", "start": "2026-11-02", "end": "2026-11-03"})
	f.refused(staff, "correct_request", map[string]any{"id": id(mine), "type": "unpaid", "start": "2026-11-02", "end": "2026-11-03", "reason": "x"}, "Another admin corrects your own leave")
	if f.ok(staff, "get_request", map[string]any{"id": id(mine)}).Data["can_correct"] != false {
		t.Fatal("can_correct true for one's own request")
	}
	f.ok(hr, "correct_request", map[string]any{"id": id(mine), "type": "unpaid", "start": "2026-11-02", "end": "2026-11-03", "note": "n", "reason": "x"})
}
