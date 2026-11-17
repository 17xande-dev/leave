package handler

import (
	"bytes"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/17xande-dev/leave/internal/leave"
	"github.com/xuri/excelize/v2"
)

func (h *Handler) AdminPayrollExport(w http.ResponseWriter, r *http.Request) {
	v := r.URL.Query().Get("include_disabled")
	if v != "" && v != "1" {
		http.Error(w, "Choose whether to include disabled accounts.", http.StatusBadRequest)
		return
	}
	rep, err := h.leave.Payroll(r.Context(), v == "1")
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	u := currentUser(r)
	buf, err := payrollWorkbook(rep, rep.GeneratedAt, u.Name)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	asAt := rep.AsAt.Format(time.DateOnly)
	slog.Info("payroll export", "by_id", u.ID, "by", u.Username, "as_at", asAt,
		"people", len(rep.People), "include_disabled", rep.IncludeDisabled,
		"bytes", buf.Len(), "unparsed_openings", rep.UnparsedOpenings)
	w.Header().Set("Content-Type", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="leave-payroll-%s.xlsx"`, asAt))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.Itoa(buf.Len()))
	_, _ = buf.WriteTo(w)
}

var payrollAnnualHeaders = []string{"Name", "Username", "Email", "Department", "Status", "Allowance",
	"Adjustments", "Entitlement", "Approved", "Approved not yet started", "Pending", "Available",
	"Balance to date", "Payroll opening balance", "Change since opening"}
var payrollAllHeaders = []string{"Name", "Username", "Department", "Status", "Leave type", "Unit",
	"Cycle start", "Cycle end", "Allowance", "Adjustments", "Approved", "Pending", "Available"}

// A typed string cell is never evaluated by Excel, unlike CSV. No visible
// apostrophe is added; excelize's public Style has no quotePrefix.
func setText(f *excelize.File, sheet, cell, s string) error {
	return f.SetCellStr(sheet, cell, s)
}

// payrollCells keeps the first write error: nothing is sent until the complete
// workbook has been built successfully.
type payrollCells struct {
	f                              *excelize.File
	err                            error
	number, date, datetime, header int
}

func (w *payrollCells) check(err error) {
	if w.err == nil {
		w.err = err
	}
}
func payrollCell(col, row int) string {
	cell, _ := excelize.CoordinatesToCellName(col, row)
	return cell
}
func (w *payrollCells) text(sheet string, col, row int, s string) {
	w.check(setText(w.f, sheet, payrollCell(col, row), s))
}
func (w *payrollCells) days(sheet string, col, row int, d *leave.Days) {
	if d == nil {
		return
	}
	cell := payrollCell(col, row)
	w.check(w.f.SetCellFloat(sheet, cell, float64(*d)/100, 2, 64))
	w.check(w.f.SetCellStyle(sheet, cell, cell, w.number))
}
func (w *payrollCells) day(sheet string, col, row int, t time.Time, clock bool) {
	style := w.date
	hour, minute, second := 0, 0, 0
	if clock {
		style = w.datetime
		hour, minute, second = t.Clock()
	}
	// Excel serial dates carry civil time, not a timezone. Apply our style
	// after SetCellValue, which installs excelize's default date style.
	t = time.Date(t.Year(), t.Month(), t.Day(), hour, minute, second, 0, time.UTC)
	cell := payrollCell(col, row)
	w.check(w.f.SetCellValue(sheet, cell, t))
	w.check(w.f.SetCellStyle(sheet, cell, cell, style))
}
func (w *payrollCells) dateString(sheet string, col, row int, s string) {
	t, err := leave.ParseDate(s)
	w.check(err)
	if err == nil {
		w.day(sheet, col, row, t, false)
	}
}
func (w *payrollCells) dataSheet(sheet string, headers []string, rows int, widths []float64) {
	for i, h := range headers {
		w.text(sheet, i+1, 1, h)
		col, _ := excelize.ColumnNumberToName(i + 1)
		w.check(w.f.SetColWidth(sheet, col, col, widths[i]))
	}
	last := payrollCell(len(headers), rows)
	w.check(w.f.SetCellStyle(sheet, "A1", payrollCell(len(headers), 1), w.header))
	w.check(w.f.SetPanes(sheet, &excelize.Panes{Freeze: true, YSplit: 1, TopLeftCell: "A2", ActivePane: "bottomLeft"}))
	w.check(w.f.AutoFilter(sheet, "A1:"+last, nil))
}

func payrollWorkbook(rep leave.PayrollReport, generatedAt time.Time, byName string) (*bytes.Buffer, error) {
	f := excelize.NewFile()
	defer f.Close()
	w := &payrollCells{f: f}
	w.check(f.SetSheetName("Sheet1", "Annual leave"))
	_, err := f.NewSheet("All types")
	w.check(err)
	_, err = f.NewSheet("About")
	w.check(err)
	f.SetActiveSheet(0)
	for _, spec := range []struct {
		target *int
		format string
	}{
		{&w.number, "0.00;[Red]-0.00"}, {&w.date, "yyyy-mm-dd"}, {&w.datetime, "yyyy-mm-dd hh:mm"},
	} {
		*spec.target, err = f.NewStyle(&excelize.Style{CustomNumFmt: &spec.format})
		w.check(err)
	}
	w.header, err = f.NewStyle(&excelize.Style{Font: &excelize.Font{Bold: true}})
	w.check(err)
	w.check(f.SetDocProps(&excelize.DocProperties{Title: "Leave balances for payroll", Creator: "Leave", Created: generatedAt.Format(time.RFC3339)}))
	annualRow, allRow := 2, 2
	for _, p := range rep.People {
		u := p.User
		status := "Active"
		if u.Disabled {
			status = "Disabled"
		}
		if rep.Annual != nil {
			for i, s := range []string{u.Name, u.Username, u.Email, u.Department, status} {
				w.text("Annual leave", i+1, annualRow, s)
			}
			if fig, ok := p.Figures[rep.Annual.ID]; ok {
				var change *leave.Days
				if available := fig.Available(); available != nil && p.Opening != nil {
					d := *available - *p.Opening
					change = &d
				}
				for i, d := range []*leave.Days{fig.Allowance, &fig.Adjustments, fig.Entitlement(), &fig.Approved,
					&fig.ApprovedFuture, &fig.Pending, fig.Available(), fig.ToDate(), p.Opening, change} {
					w.days("Annual leave", i+6, annualRow, d)
				}
			}
			annualRow++
		}
		for _, pt := range rep.Types {
			fig, ok := p.Figures[pt.Type.ID]
			if !ok {
				continue
			}
			unit := "days"
			switch pt.Type.Unit {
			case leave.UnitCalendar:
				unit = "calendar days"
			case leave.UnitWeekend:
				unit = "weekends"
			}
			for i, s := range []string{u.Name, u.Username, u.Department, status, pt.Type.Name, unit} {
				w.text("All types", i+1, allRow, s)
			}
			w.dateString("All types", 7, allRow, pt.CycleStart)
			w.dateString("All types", 8, allRow, pt.CycleEnd)
			for i, d := range []*leave.Days{fig.Allowance, &fig.Adjustments, &fig.Approved, &fig.Pending, fig.Available()} {
				w.days("All types", i+9, allRow, d)
			}
			allRow++
		}
	}
	w.dataSheet("Annual leave", payrollAnnualHeaders, annualRow-1, []float64{28, 20, 32, 28, 12, 12, 14, 14, 12, 26, 12, 12, 18, 24, 22})
	w.dataSheet("All types", payrollAllHeaders, allRow-1, []float64{28, 20, 28, 12, 30, 18, 14, 14, 12, 14, 12, 12, 12})
	included := "Enabled accounts"
	if rep.IncludeDisabled {
		included = "Enabled and disabled accounts"
	}
	for i, pair := range [][2]string{{"Report", "Leave balances for payroll"}, {"As at", ""}, {"Generated at", ""},
		{"Generated by", byName}, {"People included", included}} {
		w.text("About", 1, i+1, pair[0])
		w.text("About", 2, i+1, pair[1])
	}
	w.day("About", 2, 2, rep.AsAt, false)
	w.day("About", 2, 3, generatedAt, true)
	row := 6
	if rep.Annual == nil {
		w.text("About", 1, row, "Annual leave")
		w.text("About", 2, row, "No annual leave type exists.")
		row++
	}
	for _, pt := range rep.Types {
		w.text("About", 1, row, pt.Type.Name+" cycle")
		w.text("About", 2, row, pt.CycleStart+" to "+pt.CycleEnd)
		row++
	}
	row++
	for _, pair := range [][2]string{
		{"Available", "What the person can still book: entitlement less approved and pending leave, including leave booked for later."},
		{"Balance to date", "Entitlement less approved leave that has already started. Ignores pending and future leave; a request starting on or before the as-at date counts whole."},
		{"Payroll opening balance", "Payroll's October 2026 figure the app's balances were set to. Blank where no correction was needed."},
		{"Change since opening", "Available less payroll opening balance: what the app has deducted or added since."},
		{"Day counts", "The app counts each person's work week (with their long weekend) less South African public holidays, so its day counts can differ from payroll's."},
	} {
		w.text("About", 1, row, pair[0])
		w.text("About", 2, row, pair[1])
		row++
	}
	w.check(f.SetColWidth("About", "A", "A", 28))
	w.check(f.SetColWidth("About", "B", "B", 100))
	if w.err != nil {
		return nil, w.err
	}
	return f.WriteToBuffer()
}
