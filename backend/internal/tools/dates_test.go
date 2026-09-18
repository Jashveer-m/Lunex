package tools

import (
	"testing"
	"time"
)

// testNow is Thursday 10 September 2026, 15:30 UTC. Every relative phrase
// below is resolved against it, which is the point of doing the arithmetic in
// Go: the answer to "next friday" is a fact, not a sample.
func TestParseDate(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"2026-09-30", "2026-09-30"},
		{"2026-09-30T17:00:00Z", "2026-09-30T17:00:00Z"},
		{"today", "2026-09-10"},
		{"Tonight", "2026-09-10"},
		{"tomorrow", "2026-09-11"},
		{"by tomorrow", "2026-09-11"},
		{"the day after tomorrow", "2026-09-12"},
		// A weekday is the next one strictly after today, with or without
		// "next" -- so "thursday" on a Thursday is a week away.
		{"friday", "2026-09-11"},
		{"next friday", "2026-09-11"},
		{"on Mon", "2026-09-14"},
		{"thursday", "2026-09-17"},
		{"in 3 days", "2026-09-13"},
		{"in 2 weeks", "2026-09-24"},
		{"in 1 month", "2026-10-10"},
		{"next week", "2026-09-17"},
		{"next month", "2026-10-10"},
		{"end of the week", "2026-09-13"},
		{"end of the month", "2026-09-30"},
		{"end of year", "2026-12-31"},
		{"30 September", "2026-09-30"},
		{"September 30", "2026-09-30"},
		{"sept 30th, 2027", "2027-09-30"},
		{"30th of September", "2026-09-30"},
		// Without a year, the next such date that is not already past.
		{"5 January", "2027-01-05"},
		{"10 September", "2026-09-10"},
	} {
		t.Run(tc.in, func(t *testing.T) {
			got, err := ParseDate(tc.in, testNow)
			if err != nil {
				t.Fatalf("ParseDate(%q): %v", tc.in, err)
			}
			layout := time.DateOnly
			if len(tc.want) > len(time.DateOnly) {
				layout = time.RFC3339
			}
			if s := got.Format(layout); s != tc.want {
				t.Fatalf("ParseDate(%q) = %s, want %s", tc.in, s, tc.want)
			}
			if got.Location() != time.UTC {
				t.Fatalf("ParseDate(%q) is not UTC", tc.in)
			}
		})
	}
}

// The calendar day is the UTC one whatever zone the clock is read in. On a
// server in IST, 01:00 on Friday 11 September is still Thursday 10 September in
// UTC -- the date the chat prompt states -- and before the fix "tomorrow"
// resolved against the local Friday and landed a day late.
func TestParseDateUsesTheUTCDayWhateverTheServerZone(t *testing.T) {
	ist := time.FixedZone("IST", 5*60*60+30*60)
	now := time.Date(2026, time.September, 11, 1, 0, 0, 0, ist)
	if now.UTC().Day() != 10 {
		t.Fatalf("test clock is wrong: %v", now.UTC())
	}
	for in, want := range map[string]string{
		"today":       "2026-09-10",
		"tomorrow":    "2026-09-11",
		"next friday": "2026-09-11",
		"in 3 days":   "2026-09-13",
	} {
		got, err := ParseDate(in, now)
		if err != nil {
			t.Fatalf("ParseDate(%q): %v", in, err)
		}
		if s := got.Format(time.DateOnly); s != want {
			t.Errorf("ParseDate(%q) at %v = %s, want %s", in, now, s, want)
		}
	}
}

// Anything it cannot read is an error, never a guess -- including a date that
// time.Date would quietly roll over into the next month.
func TestParseDateRefusesToGuess(t *testing.T) {
	for _, in := range []string{
		"next year", "someday", "soon", "31 September", "30/09/2026", "the 40th", "in 0 days",
		"in 99999 days", "",
	} {
		if got, err := ParseDate(in, testNow); err == nil {
			t.Fatalf("ParseDate(%q) = %v, want an error", in, got)
		}
	}
}

// The past. Nothing looked backwards before Phase 9 -- a deadline is ahead and
// a calendar mostly is -- and a ledger is read backwards: "I paid the rent
// yesterday" is the ordinary way an expense gets mentioned.
func TestParseDateReadsThePast(t *testing.T) {
	for phrase, want := range map[string]string{
		"yesterday":                "2026-09-09",
		"last night":               "2026-09-09",
		"the day before yesterday": "2026-09-08",
		"3 days ago":               "2026-09-07",
		"2 weeks ago":              "2026-08-27",
		"1 month ago":              "2026-08-10",
		"a year ago":               "",
	} {
		got, err := ParseDate(phrase, testNow)
		if want == "" {
			if err == nil {
				t.Fatalf("ParseDate(%q) = %v, want a refusal rather than a guess", phrase, got)
			}
			continue
		}
		if err != nil || got.Format(time.DateOnly) != want {
			t.Fatalf("ParseDate(%q) = %v, %v; want %s", phrase, got, err, want)
		}
	}
}

// The same for a stretch of the past, which is what "how much did I spend last
// month" needs. The windows are half-open, as everywhere else here.
func TestParseWindowReadsThePast(t *testing.T) {
	for phrase, want := range map[string][2]string{
		"last month":    {"2026-08-01", "2026-09-01"},
		"last year":     {"2025-01-01", "2026-01-01"},
		"last 7 days":   {"2026-09-04", "2026-09-11"},
		"past 3 months": {"2026-06-11", "2026-09-11"},
	} {
		start, end, err := ParseWindow(phrase, testNow)
		if err != nil {
			t.Fatalf("ParseWindow(%q): %v", phrase, err)
		}
		got := [2]string{start.Format(time.DateOnly), end.Format(time.DateOnly)}
		if got != want {
			t.Fatalf("ParseWindow(%q) = %v, want %v", phrase, got, want)
		}
	}
}

// Moving back a month must clamp to the end of the month it lands in, not
// normalize forward the way time.AddDate does. The 31st of March minus one
// month is the 31st of February, and AddDate makes that the 3rd of March -- so
// "how much did I spend in the past month", asked on the 31st, would leave out
// the first three days of the period it named.
func TestMonthsBeforeClampsToTheEndOfTheMonth(t *testing.T) {
	for _, tc := range []struct {
		from string
		n    int
		want string
	}{
		{"2026-03-31", 1, "2026-02-28"},
		{"2026-05-31", 3, "2026-02-28"},
		{"2026-01-31", 2, "2025-11-30"},
		{"2028-03-29", 1, "2028-02-29"}, // a leap year
		{"2026-09-10", 1, "2026-08-10"}, // the ordinary case is unchanged
		{"2026-09-10", 12, "2025-09-10"},
	} {
		from, err := time.Parse(time.DateOnly, tc.from)
		if err != nil {
			t.Fatal(err)
		}
		if got := MonthsBefore(from, tc.n).Format(time.DateOnly); got != tc.want {
			t.Fatalf("MonthsBefore(%s, %d) = %s, want %s", tc.from, tc.n, got, tc.want)
		}
	}

	// And through the two parsers that use it.
	endOfMarch := time.Date(2026, 3, 31, 12, 0, 0, 0, time.UTC)
	start, end, err := ParseWindow("past 1 month", endOfMarch)
	if err != nil {
		t.Fatal(err)
	}
	if got := start.Format(time.DateOnly); got != "2026-03-01" {
		t.Fatalf(`ParseWindow("past 1 month") on the 31st starts %s, want 2026-03-01`, got)
	}
	if got := end.Format(time.DateOnly); got != "2026-04-01" {
		t.Fatalf(`it ends %s, want the 1st of April`, got)
	}
	got, err := ParseDate("1 month ago", endOfMarch)
	if err != nil || got.Format(time.DateOnly) != "2026-02-28" {
		t.Fatalf(`ParseDate("1 month ago") on the 31st = %v, %v; want 2026-02-28`, got, err)
	}
}
