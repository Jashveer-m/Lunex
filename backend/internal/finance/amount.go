package finance

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Amount is a sum of money in hundredths of a unit -- 1234 is 12.34.
//
// It is an integer and never a float, and that is the one non-negotiable
// decision in this module. A float64 cannot hold 0.1, so a column of them does
// not add up to what a person adding the same numbers on paper gets, and the
// whole point of the summary endpoint is to produce a total somebody will
// compare against their bank. int64 hundredths is exactly the scale
// numeric(12,2) stores, so the value in Go and the value in Postgres are the
// same number rather than two roundings of one.
//
// Hundredths, rather than the minor unit of whatever currency it is, because
// the column has two decimal places and nothing here knows what a currency's
// minor unit is. A zero-decimal currency stores whole hundreds and reads back
// as it was written.
type Amount int64

// MaxAmount is what numeric(12,2) holds: ten digits before the point. The
// CHECK constraint rejects more, so validating it here is what turns a
// database error into a field error.
const MaxAmount Amount = 999_999_999_999

// ErrNotAnAmount is a string that is not a decimal number of money.
var ErrNotAnAmount = errors.New("not an amount")

// ParseAmount reads a decimal string: "12", "12.3", "12.34", "1,234.50".
//
// Grouping commas and a leading currency symbol are accepted because this is
// also what a person types, and rejecting "₹1,200" in favour of a field error
// helps nobody. More than two decimal places is refused rather than rounded: a
// caller that means 12.345 means something this column cannot store, and
// silently making it 12.35 is the kind of quiet edit a ledger must not do.
//
// What it does *not* enforce is how large one expense may be. That is
// MaxAmount, checked by ValidateCreate, and it is deliberately not checked here
// -- this function also reads back the *sums* the summary query produces, and
// two expenses of MaxAmount add up to more than one is allowed to be. The only
// limit here is what an int64 of hundredths can hold, which is seven orders of
// magnitude beyond any personal ledger.
func ParseAmount(s string) (Amount, error) {
	raw := strings.TrimSpace(s)
	// At most one sign, and it comes before the symbol: "-$5", not "--5".
	s = raw
	neg := strings.HasPrefix(s, "-")
	if neg || strings.HasPrefix(s, "+") {
		s = s[1:]
	}
	s = strings.TrimLeft(s, "$£€₹¥")

	// Spaces and underscores go first, so "1, 200" is read as the grouped
	// number it is rather than refused by the check below.
	s = strings.Map(func(r rune) rune {
		if r == '_' || r == ' ' {
			return -1
		}
		return r
	}, s)
	// A comma is a grouping separator here, never a decimal point. That is a
	// choice, and the ambiguous case is refused rather than guessed: "12,34" is
	// twelve euros thirty-four to a good part of the world and twelve thousand
	// three hundred and forty to the rest, and stripping the comma silently
	// multiplies it by a hundred. So a comma is only accepted where a grouping
	// comma can be -- followed by exactly three digits.
	if err := groupingOnly(s); err != nil {
		return 0, err
	}
	s = strings.ReplaceAll(s, ",", "")
	if s == "" {
		return 0, fmt.Errorf("%w: %q", ErrNotAnAmount, raw)
	}

	whole, frac, hasFrac := strings.Cut(s, ".")
	if whole == "" {
		whole = "0"
	}
	if !allDigits(whole) || (hasFrac && !allDigits(frac)) {
		return 0, fmt.Errorf("%w: %q", ErrNotAnAmount, raw)
	}
	switch {
	case len(frac) > 2:
		return 0, fmt.Errorf("%w: %q has more than two decimal places", ErrNotAnAmount, raw)
	case len(frac) == 1:
		frac += "0"
	case len(frac) == 0:
		frac = "00"
	}
	// Overflow is ParseInt's answer rather than a digit count of our own, so
	// that a leading zero is a leading zero and a sum wider than one expense is
	// allowed to be still reads back.
	n, err := strconv.ParseInt(strings.TrimLeft(whole, "0")+frac, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %q is not a number this can hold", ErrNotAnAmount, raw)
	}
	if neg {
		n = -n
	}
	return Amount(n), nil
}

// groupingOnly rejects a comma that is not a thousands separator. See
// ParseAmount.
func groupingOnly(s string) error {
	for i := strings.IndexByte(s, ','); i >= 0; i = strings.IndexByte(s, ',') {
		rest := s[i+1:]
		run := 0
		for run < len(rest) && rest[run] >= '0' && rest[run] <= '9' {
			run++
		}
		if run != 3 {
			return fmt.Errorf("%w: %q -- use a full stop for the decimal point", ErrNotAnAmount, s)
		}
		s = rest
	}
	return nil
}

func allDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

// String is the canonical decimal form, always with two places: "12.34".
// It is what goes to Postgres and what comes back in JSON.
func (a Amount) String() string {
	sign := ""
	n := int64(a)
	if n < 0 {
		sign, n = "-", -n
	}
	return fmt.Sprintf("%s%d.%02d", sign, n/100, n%100)
}

// MarshalJSON writes the amount as a JSON *number* with two decimal places.
//
// Unquoted, because a client charting spending should not have to parse a
// string; exact, because the digits are written out rather than passed through
// a float64 on the way. A client that decodes it into a float of its own has
// made that choice itself.
func (a Amount) MarshalJSON() ([]byte, error) { return []byte(a.String()), nil }

// UnmarshalJSON accepts a JSON number or a string. The number is read from its
// source text rather than through a float64, so 0.1 is a tenth here and not
// 0.1000000000000000055.
func (a *Amount) UnmarshalJSON(data []byte) error {
	s := strings.TrimSpace(string(data))
	if s == "null" {
		return nil
	}
	if len(s) >= 2 && s[0] == '"' {
		var str string
		if err := json.Unmarshal(data, &str); err != nil {
			return err
		}
		s = str
	}
	v, err := ParseAmount(s)
	if err != nil {
		return err
	}
	*a = v
	return nil
}
