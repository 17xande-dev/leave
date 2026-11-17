package handler

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/17xande-dev/leave/internal/leave"
)

func TestStretchYearsInTeamAndPreview(t *testing.T) {
	pages, partials, err := parsePages()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		ranges  []leave.Range
		team    []string
		preview string
	}{
		{"same year", []leave.Range{{Start: "2026-10-12", End: "2026-10-13"}, {Start: "2026-10-15", End: "2026-10-16"}},
			[]string{"12–13 Oct", "15–16 Oct"}, "Mon 12 – Tue 13 Oct, Thu 15 – Fri 16 Oct"},
		{"crossing stretch", []leave.Range{{Start: "2026-12-28", End: "2027-01-03"}, {Start: "2027-01-05", End: "2027-01-05"}},
			[]string{"28 Dec 2026 – 3 Jan 2027", "5 Jan 2027"}, "Mon 28 Dec 2026 – Sun 3 Jan 2027, Tue 5 Jan 2027"},
		{"separate years", []leave.Range{{Start: "2026-12-28", End: "2026-12-29"}, {Start: "2027-01-04", End: "2027-01-05"}},
			[]string{"28 Dec 2026 – 29 Dec 2026", "4 Jan 2027 – 5 Jan 2027"}, "Mon 28 Dec 2026 – Tue 29 Dec 2026, Mon 4 Jan 2027 – Tue 5 Jan 2027"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := leave.Request{Start: tc.ranges[0].Start, End: tc.ranges[len(tc.ranges)-1].End, Ranges: tc.ranges}
			var team, preview bytes.Buffer
			if err := pages["team.html"].ExecuteTemplate(&team, "content", view{Data: teamData{Reports: []leave.Request{r}}}); err != nil {
				t.Fatal(err)
			}
			for _, want := range tc.team {
				if !strings.Contains(team.String(), "<span>"+want+"</span>") {
					t.Errorf("team lacks %q: %s", want, team.String())
				}
			}
			q := quoteData{Quote: &leave.Quote{Days: 100, Ranges: tc.ranges}}
			if err := partials.ExecuteTemplate(&preview, "quote", q); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(preview.String(), "stretches: "+tc.preview+"</strong>") {
				t.Errorf("preview lacks %q: %s", tc.preview, preview.String())
			}
		})
	}
}

func TestAwayMorePunctuation(t *testing.T) {
	pages, partials, err := parsePages()
	if err != nil {
		t.Fatal(err)
	}
	for _, count := range []int{1, 2} {
		for _, more := range []int{0, 2} {
			t.Run(fmt.Sprintf("%d shown %d more", count, more), func(t *testing.T) {
				away := []awayView{{Name: "Alex", Ranges: []awayRange{{Start: "2026-10-12", End: "2026-10-12"}, {Start: "2027-01-04", End: "2027-01-04"}}}}
				last := "Alex (12 Oct 2026, 4 Jan 2027)"
				if count == 2 {
					away = append(away, awayView{Name: "Sam", Ranges: []awayRange{{Start: "2026-10-13", End: "2026-10-13"}}})
					last = "Sam (13 Oct)"
				}
				for _, queue := range []bool{false, true} {
					var buf bytes.Buffer
					want := last
					end := "</p>"
					if queue {
						end = "</small>"
					}
					if more > 0 {
						if count > 1 {
							if queue {
								want += ";"
							} else {
								want += ","
							}
						}
						want += " and 2 more"
					}
					if queue {
						err = pages["approvals.html"].ExecuteTemplate(&buf, "content", view{Data: queueData{Queue: []queueRow{{Away: away, AwayMore: more}}}})
					} else {
						err = partials.ExecuteTemplate(&buf, "quote", quoteData{Away: away, AwayMore: more})
					}
					if err != nil {
						t.Fatal(err)
					}
					if !strings.Contains(buf.String(), want+end) {
						t.Errorf("queue=%v: want %q in %s", queue, want+end, buf.String())
					}
				}
			})
		}
	}
}
