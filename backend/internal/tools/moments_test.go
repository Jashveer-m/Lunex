package tools

import (
	"testing"
	"time"
)

// testNow is Thursday 10 September 2026, 15:30 UTC. Every phrase below is
// resolved against it.
func TestParseMoment(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    string
		hasTime bool
		hasDate bool
	}{
		// A date and nothing else: no time of day was said, and none is made
		// up. That is what makes create_calendar_event file it as all-day.
		{"2026-09-30", "2026-09-30T00:00:00Z", false, true},
		{"tomorrow", "2026-09-11T00:00:00Z", false, true},
		{"friday", "2026-09-11T00:00:00Z", false, true},
		{"30 September", "2026-09-30T00:00:00Z", false, true},
		// "Tomorrow evening" is a time of day to a person and a two-hour error
		// bar to a calendar, so it stays a day here.
		{"tomorrow evening", "2026-09-11T00:00:00Z", false, true},

		// A date and a time, written every way a model writes it.
		{"tomorrow at 3pm", "2026-09-11T15:00:00Z", true, true},
		{"3pm tomorrow", "2026-09-11T15:00:00Z", true, true},
		{"tomorrow at 3:30pm", "2026-09-11T15:30:00Z", true, true},
		{"tomorrow 3.30 pm", "2026-09-11T15:30:00Z", true, true},
		{"friday 09:30", "2026-09-11T09:30:00Z", true, true},
		{"2026-09-30 14:00", "2026-09-30T14:00:00Z", true, true},
		{"monday at noon", "2026-09-14T12:00:00Z", true, true},
		{"tomorrow at midnight", "2026-09-11T00:00:00Z", true, true},
		{"12am tomorrow", "2026-09-11T00:00:00Z", true, true},
		{"12pm tomorrow", "2026-09-11T12:00:00Z", true, true},

		// A time and no day is today -- relative to whatever `now` is, which is
		// how an event's end lands on its start's day.
		{"4pm", "2026-09-10T16:00:00Z", true, false},
		{"16:45", "2026-09-10T16:45:00Z", true, false},

		// An RFC 3339 timestamp is taken as written: it is what the API stores.
		{"2026-09-30T17:00:00Z", "2026-09-30T17:00:00Z", true, true},
	} {
		t.Run(tc.in, func(t *testing.T) {
			got, err := ParseMoment(tc.in, testNow)
			if err != nil {
				t.Fatalf("ParseMoment(%q): %v", tc.in, err)
			}
			if s := got.Time.Format(time.RFC3339); s != tc.want {
				t.Fatalf("ParseMoment(%q) = %s, want %s", tc.in, s, tc.want)
			}
			if got.HasTime != tc.hasTime || got.HasDate != tc.hasDate {
				t.Fatalf("ParseMoment(%q) time=%v date=%v, want time=%v date=%v",
					tc.in, got.HasTime, got.HasDate, tc.hasTime, tc.hasDate)
			}
			if got.Time.Location() != time.UTC {
				t.Fatalf("ParseMoment(%q) is not UTC", tc.in)
			}
		})
	}
}

// A bare number is not a time. "In 1 week" is a date the user gave, and
// reading the 1 as one o'clock would turn it into a time they did not.
func TestParseMomentRefusesWhatItCannotRead(t *testing.T) {
	for _, in := range []string{"sometime soon", "whenever", "the 32nd of Octember", "", "25:00"} {
		if got, err := ParseMoment(in, testNow); err == nil {
			t.Fatalf("ParseMoment(%q) = %v, want an error", in, got)
		}
	}
	for _, tc := range []struct{ in, want string }{
		{"in 1 week", "2026-09-17T00:00:00Z"},
		{"in 3 days", "2026-09-13T00:00:00Z"},
	} {
		got, err := ParseMoment(tc.in, testNow)
		if err != nil {
			t.Fatal(err)
		}
		if s := got.Time.Format(time.RFC3339); s != tc.want || got.HasTime {
			t.Fatalf("ParseMoment(%q) = %s (time=%v), want %s with no time of day", tc.in, s, got.HasTime, tc.want)
		}
	}
}

func TestParseWindow(t *testing.T) {
	for _, tc := range []struct{ in, start, end string }{
		{"today", "2026-09-10", "2026-09-11"},
		{"tomorrow", "2026-09-11", "2026-09-12"},
		{"yesterday", "2026-09-09", "2026-09-10"},
		// The week runs Monday to Monday; today is a Thursday.
		{"this week", "2026-09-07", "2026-09-14"},
		{"next week", "2026-09-14", "2026-09-21"},
		{"last week", "2026-08-31", "2026-09-07"},
		{"this weekend", "2026-09-12", "2026-09-14"},
		{"next weekend", "2026-09-19", "2026-09-21"},
		{"this month", "2026-09-01", "2026-10-01"},
		{"next month", "2026-10-01", "2026-11-01"},
		{"this year", "2026-01-01", "2027-01-01"},
		{"the next 3 days", "2026-09-10", "2026-09-13"},
		{"next 2 weeks", "2026-09-10", "2026-09-24"},
		// Anything else is the day it names, and only that day.
		{"friday", "2026-09-11", "2026-09-12"},
		{"2026-09-30", "2026-09-30", "2026-10-01"},
	} {
		t.Run(tc.in, func(t *testing.T) {
			start, end, err := ParseWindow(tc.in, testNow)
			if err != nil {
				t.Fatalf("ParseWindow(%q): %v", tc.in, err)
			}
			if s := start.Format(time.DateOnly); s != tc.start {
				t.Fatalf("ParseWindow(%q) starts %s, want %s", tc.in, s, tc.start)
			}
			if s := end.Format(time.DateOnly); s != tc.end {
				t.Fatalf("ParseWindow(%q) ends %s, want %s", tc.in, s, tc.end)
			}
			if !end.After(start) {
				t.Fatalf("ParseWindow(%q) is empty", tc.in)
			}
		})
	}

	if _, _, err := ParseWindow("at some point", testNow); err == nil {
		t.Fatal("an unreadable window was accepted")
	}
}
