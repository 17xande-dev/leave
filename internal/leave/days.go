package leave

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Days is a quantity of leave in hundredths of a day (or of a weekend, for the
// weekend unit): 150 is a day and a half. Payroll balances are fractional and
// leave is taken in half days, and floating point cannot add them exactly, so
// every days figure is an integer count of hundredths in storage and in
// arithmetic. Only String and ParseDays know about the decimal point; never
// show the raw integer.
type Days int64

// WholeDays is n whole days.
func WholeDays(n int) Days { return Days(n) * 100 }

// String is the shortest exact decimal: no trailing zeros, no thousands
// separator ("15", "1.5", "10.33", "-1.44", "0.05").
func (d Days) String() string {
	neg := d < 0
	// Unsigned magnitude, so the most negative int64 does not overflow.
	u := uint64(d)
	if neg {
		u = -u
	}
	s := strconv.FormatUint(u/100, 10)
	if frac := u % 100; frac != 0 {
		s += fmt.Sprintf(".%02d", frac)
		s = strings.TrimRight(s, "0")
	}
	if neg {
		s = "-" + s
	}
	return s
}

// IsOne reports exactly one (or minus one): the only figure that takes a
// singular noun ("1 day", but "1.5 days" and "0.5 days").
func (d Days) IsOne() bool { return d == 100 || d == -100 }

// Plural picks the singular word at exactly one and the plural otherwise:
// the one rule for "1 day" but "1.5 days" and "0.5 days".
func (d Days) Plural(one, many string) string {
	if d.IsOne() {
		return one
	}
	return many
}

// Noun is the unit's name for this figure: "working day(s)" for the workdays
// unit, "day(s)" for calendar, "weekend(s)".
func (d Days) Noun(unit string) string {
	switch unit {
	case UnitWeekend:
		if d.IsOne() {
			return "weekend"
		}
		return "weekends"
	case UnitCalendar:
		if d.IsOne() {
			return "day"
		}
		return "days"
	}
	if d.IsOne() {
		return "working day"
	}
	return "working days"
}

// Label is the figure with its unit: "1 working day", "1.5 working days",
// "2 weekends".
func (d Days) Label(unit string) string { return d.String() + " " + d.Noun(unit) }

// MaxDays is the sanity cap on one adjustment or one allowance (1000 days): far
// beyond any real figure, so a slipped decimal point is refused rather than saved.
const MaxDays = Days(100000)

// MsgDaysTooMany is the refusal for a figure beyond MaxDays.
const MsgDaysTooMany = "That is more than 1000 days; check the figure."

// ErrDays is the error ParseDays returns for text that is not a days figure.
var ErrDays = errors.New("enter a number of days with at most two decimals, like 2 or 1.5")

// ParseDays reads a decimal such as "1.5", "-1.44", ".5" or "2." exactly, never
// through a float. It accepts an optional sign, digits and at most two
// decimals; commas, exponents, thousands separators, an empty string and
// anything that overflows are refused.
func ParseDays(s string) (Days, error) {
	s = strings.TrimSpace(s)
	neg := false
	if s != "" && (s[0] == '-' || s[0] == '+') {
		neg = s[0] == '-'
		s = s[1:]
	}
	whole, frac, _ := strings.Cut(s, ".")
	if whole == "" && frac == "" {
		return 0, ErrDays
	}
	if len(frac) > 2 || !allDigits(whole) || !allDigits(frac) {
		return 0, ErrDays
	}
	for len(frac) < 2 {
		frac += "0"
	}
	var n int64
	if whole != "" {
		w, err := strconv.ParseInt(whole, 10, 64)
		if err != nil || w > (1<<63-1-99)/100 {
			return 0, ErrDays
		}
		n = w * 100
	}
	f, _ := strconv.ParseInt(frac, 10, 64)
	n += f
	if neg {
		n = -n
	}
	return Days(n), nil
}

func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// MarshalJSON writes a JSON number with String's text (1.5, not 150).
func (d Days) MarshalJSON() ([]byte, error) { return []byte(d.String()), nil }

// UnmarshalJSON accepts a JSON number, or a string holding one, through the
// same exact decimal parse as ParseDays.
func (d *Days) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) > 0 && b[0] == '"' {
		// Decoded as a JSON string first, so an escape ("\u0031.5") means what
		// it says rather than reaching ParseDays as raw text.
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return ErrDays
		}
		v, err := ParseDays(s)
		if err != nil {
			return err
		}
		*d = v
		return nil
	}
	v, err := ParseDays(string(b))
	if err != nil {
		return err
	}
	*d = v
	return nil
}
