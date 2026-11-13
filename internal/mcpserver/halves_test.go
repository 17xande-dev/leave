package mcpserver

import "testing"

func TestSetHalfDaysTool(t *testing.T) {
	f := newFixture(t)
	staff, hr := f.as("staff1"), f.as("hr")
	r := f.ok(staff, "apply_for_leave", map[string]any{"type": "annual", "start": "2026-10-12", "end": "2026-10-14"})
	fp, _ := f.ok(hr, "get_request", map[string]any{"id": id(r)}).Data["fingerprint"].(string)
	if fp == "" {
		t.Fatal("missing fingerprint")
	}
	changed := f.ok(hr, "set_half_days", map[string]any{"id": id(r), "half_start": true, "half_end": false, "seen": fp})
	if get(changed.Data, "request", "days") != float64(2.5) || get(changed.Data, "request", "half_start") != true {
		t.Fatalf("change: %+v", changed.Data)
	}
	f.refused(hr, "set_half_days", map[string]any{"id": id(r), "half_start": false, "half_end": true, "seen": fp}, "This request changed since you opened it. Check it again.")
	f.refused(hr, "set_half_days", map[string]any{"id": id(r), "half_start": true, "half_end": false}, "unchanged")
	cleared := f.ok(hr, "set_half_days", map[string]any{"id": id(r), "half_start": false, "half_end": false})
	if get(cleared.Data, "request", "days") != float64(3) {
		t.Fatalf("clear: %+v", cleared.Data)
	}
	f.makeAdmin("staff1")
	f.refused(staff, "set_half_days", map[string]any{"id": id(r), "half_start": true, "half_end": false}, "Another admin changes half days on your own leave")
}
