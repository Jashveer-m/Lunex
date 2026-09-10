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
