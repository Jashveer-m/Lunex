package tools

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// A deadline is a day; a calendar event is a moment and a window. This file is
// the second half of ParseDate: the same posture -- the model finds the phrase
// and Go works out what it means -- applied to a time of day and to a range.
//
// The rule that shapes all of it is the hardening pass's: a value the user did
// not give is not invented. So a time of day is read only when one is actually
// stated ("3pm", "15:00", "noon"), and "tomorrow" with no time produces a
// moment that says it has no time -- which create_calendar_event turns into an
// all-day event rather than into a guess at nine in the morning. "Tomorrow
// evening" is a time of day to a person and a two-hour error bar to a
// calendar, so it is not one here either; the event is all-day and the user can
// say when.

// Moment is a resolved point in time, and what was actually said about it.
type Moment struct {
	Time time.Time
	// HasTime reports that a time of day was stated, rather than defaulted to
	// midnight by a bare date.
	HasTime bool
	// HasDate reports that a day was stated. A bare "3pm" has none, and the
	// caller decides which day that means -- for an event's end, the day its
	// start is on.
	HasDate bool
}

// Day is the midnight-UTC start of the day the moment falls on.
func (m Moment) Day() time.Time {
	t := m.Time.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// ParseMoment reads a date, a time of day, or both, and resolves it against
// now.
//
// Everything ParseDate accepts is accepted here, with an optional time of day
// in front of it or behind it: "tomorrow at 3pm", "3pm tomorrow", "friday
// 09:30", "2026-09-30 14:00", "noon". A bare time of day is that time *today*
// -- relative to whatever `now` is, which is how an event's end is resolved
// onto its start's day.
//
// An RFC 3339 timestamp is taken as written, and counts as both a date and a
// time: it is what the API itself would store.
func ParseMoment(s string, now time.Time) (Moment, error) {
	raw := strings.TrimSpace(s)
	if raw == "" {
		return Moment{}, fmt.Errorf("could not read %q as a date or time", s)
	}
	if t, err := time.Parse(time.RFC3339, strings.ToUpper(raw)); err == nil {
		return Moment{Time: t.UTC(), HasTime: true, HasDate: true}, nil
	}

	lower := strings.ToLower(raw)
	rest, clock, found := splitTimeOfDay(lower)
	if !found {
		day, err := ParseDate(raw, now)
		if err != nil {
			return Moment{}, err
		}
		return Moment{Time: day, HasDate: true}, nil
	}

	// A time of day and nothing else: today, in the sense `now` gives.
	if rest == "" {
		today := now.UTC()
		return Moment{
			Time:    time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, time.UTC).Add(clock),
			HasTime: true,
		}, nil
	}
	day, err := ParseDate(rest, now)
	if err != nil {
		return Moment{}, fmt.Errorf("could not read %q as a date and time", raw)
	}
	return Moment{Time: day.Add(clock), HasTime: true, HasDate: true}, nil
}

// timeOfDay matches the clock times people write: 3pm, 3:30 pm, 3.30pm, 15:00,
// and the two words that name a time exactly.
//
// A bare number is deliberately not one of them. "Lunch at 1" is a time and
// "in 1 week" is not, and a pattern that reads both the same way turns a date
// the user did give into a time they did not.
var timeOfDay = regexp.MustCompile(`\b(?:(\d{1,2})(?:[:.](\d{2}))?\s*(am|pm)|(\d{1,2}):(\d{2})|(noon|midday|midnight))\b`)

// connectors are the words left behind when a time of day is cut out of a
// phrase: "tomorrow at 3pm" leaves "tomorrow at".
var connectors = regexp.MustCompile(`\b(at|from|around|about|by|on|starting|start|until|till|to|@)\b`)

// splitTimeOfDay cuts the time of day out of a phrase, returning the rest of
// the phrase and the offset into the day. found is false when the phrase names
// no time at all.
func splitTimeOfDay(s string) (rest string, clock time.Duration, found bool) {
	m := timeOfDay.FindStringSubmatchIndex(s)
	if m == nil {
		return s, 0, false
	}
	g := func(i int) string {
		if m[2*i] < 0 {
			return ""
		}
		return s[m[2*i]:m[2*i+1]]
	}

	var hour, minute int
	switch {
	case g(3) != "": // 12-hour, with am/pm
		hour, _ = strconv.Atoi(g(1))
		if g(2) != "" {
			minute, _ = strconv.Atoi(g(2))
		}
		if hour > 12 || minute > 59 {
			return s, 0, false
		}
		hour %= 12
		if g(3) == "pm" {
			hour += 12
		}
	case g(4) != "": // 24-hour
		hour, _ = strconv.Atoi(g(4))
		minute, _ = strconv.Atoi(g(5))
		if hour > 23 || minute > 59 {
			return s, 0, false
		}
	default: // noon, midday, midnight
		if g(6) != "midnight" {
			hour = 12
		}
	}

	cut := strings.TrimSpace(s[:m[0]] + " " + s[m[1]:])
	cut = strings.TrimSpace(connectors.ReplaceAllString(cut, " "))
	return strings.Join(strings.Fields(cut), " "), time.Duration(hour)*time.Hour + time.Duration(minute)*time.Minute, true
}

// ParseWindow reads a phrase naming a stretch of time and returns it as the
// half-open range [start, end) the calendar is queried with.
//
// The named periods are the ones people ask about -- today, tomorrow, this
// week, next month -- and anything else is read as a single day (or, with a
// time of day, the twenty-four hours from it): "what is on on Friday" is a
// question about Friday, not about Friday onwards.
//
// A week runs Monday to Monday. That is a choice, and it is the one the ISO
// calendar makes; a user whose week starts on Sunday sees a window shifted by a
// day, which the tool's summary states in dates so it is visible rather than
// assumed.
func ParseWindow(s string, now time.Time) (start, end time.Time, err error) {
	phrase := strings.ToLower(strings.TrimSpace(s))
	for _, prefix := range []string{"for ", "in ", "during ", "over ", "the "} {
		phrase = strings.TrimPrefix(phrase, prefix)
	}
	phrase = strings.TrimSpace(strings.TrimSuffix(phrase, "."))

	utc := now.UTC()
	today := time.Date(utc.Year(), utc.Month(), utc.Day(), 0, 0, 0, 0, time.UTC)
	// Monday of this week.
	monday := today.AddDate(0, 0, -((int(today.Weekday()) + 6) % 7))

	switch phrase {
	case "today", "the rest of today", "rest of today", "tonight", "this evening":
		return today, today.AddDate(0, 0, 1), nil
	case "tomorrow", "tmrw":
		return today.AddDate(0, 0, 1), today.AddDate(0, 0, 2), nil
	case "yesterday":
		return today.AddDate(0, 0, -1), today, nil
	case "this week", "the week", "week":
		return monday, monday.AddDate(0, 0, 7), nil
	case "next week":
		return monday.AddDate(0, 0, 7), monday.AddDate(0, 0, 14), nil
	case "last week", "previous week":
		return monday.AddDate(0, 0, -7), monday, nil
	case "this weekend", "weekend", "coming weekend":
		// Saturday and Sunday of the week we are in.
		return monday.AddDate(0, 0, 5), monday.AddDate(0, 0, 7), nil
	case "next weekend":
		return monday.AddDate(0, 0, 12), monday.AddDate(0, 0, 14), nil
	case "this month", "the month", "month":
		first := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, time.UTC)
		return first, first.AddDate(0, 1, 0), nil
	case "next month":
		first := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, 1, 0)
		return first, first.AddDate(0, 1, 0), nil
	case "last month", "previous month":
		first := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, -1, 0)
		return first, first.AddDate(0, 1, 0), nil
	case "this year", "the year", "year":
		first := time.Date(today.Year(), time.January, 1, 0, 0, 0, 0, time.UTC)
		return first, first.AddDate(1, 0, 0), nil
	case "next year":
		first := time.Date(today.Year()+1, time.January, 1, 0, 0, 0, 0, time.UTC)
		return first, first.AddDate(1, 0, 0), nil
	case "last year", "previous year":
		first := time.Date(today.Year()-1, time.January, 1, 0, 0, 0, 0, time.UTC)
		return first, first.AddDate(1, 0, 0), nil
	}

	// The backwards stretches, which arrived with Phase 9 for the same reason
	// "yesterday" did: a ledger is read backwards. The window ends tomorrow
	// rather than today because it is half-open and today's spending is part of
	// "the last 7 days".
	if m := lastNPeriods.FindStringSubmatch(phrase); m != nil {
		n, convErr := strconv.Atoi(m[1])
		if convErr != nil || n <= 0 || n > 366 {
			return time.Time{}, time.Time{}, fmt.Errorf("could not read %q as a stretch of time", s)
		}
		tomorrow := today.AddDate(0, 0, 1)
		switch m[2] {
		case "day":
			return today.AddDate(0, 0, -n+1), tomorrow, nil
		case "week":
			return today.AddDate(0, 0, -7*n+1), tomorrow, nil
		case "month":
			// Clamped, not normalized forward: see MonthsBefore. A window that
			// starts on the 4th of March because February has no 31st is a
			// total missing three days of the period that was asked for.
			return MonthsBefore(today, n).AddDate(0, 0, 1), tomorrow, nil
		}
	}

	if m := nextNPeriods.FindStringSubmatch(phrase); m != nil {
		n, convErr := strconv.Atoi(m[1])
		if convErr != nil || n <= 0 || n > 366 {
			return time.Time{}, time.Time{}, fmt.Errorf("could not read %q as a stretch of time", s)
		}
		switch m[2] {
		case "day":
			return today, today.AddDate(0, 0, n), nil
		case "week":
			return today, today.AddDate(0, 0, 7*n), nil
		case "month":
			return today, today.AddDate(0, n, 0), nil
		case "hour":
			return utc, utc.Add(time.Duration(n) * time.Hour), nil
		}
	}

	m, err := ParseMoment(s, now)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	if m.HasTime {
		return m.Time, m.Time.AddDate(0, 0, 1), nil
	}
	return m.Day(), m.Day().AddDate(0, 0, 1), nil
}

// lastNPeriods reads "the last 7 days", "past 3 months", "previous 2 weeks".
var lastNPeriods = regexp.MustCompile(`^(?:last|past|previous)\s+(\d+)\s*(day|week|month)s?$`)

// NamesABareCalendarDate reports whether a phrase names a day and a month and
// no year -- "5 September", "sept 30th".
//
// It exists because such a phrase is the one kind whose *year* is inferred, and
// the two modules that read dates infer it in opposite directions. See
// InThePast.
func NamesABareCalendarDate(s string) bool {
	s = strings.ToLower(strings.NewReplacer(",", " ", "-", " ", "/", " ").Replace(s))
	named := false
	for _, w := range strings.Fields(s) {
		if _, ok := months[w]; ok {
			named = true
			continue
		}
		// Any four-digit number is a year the phrase gave for itself.
		if len(w) == 4 {
			if y, err := strconv.Atoi(w); err == nil && y >= 2000 && y <= 2100 {
				return false
			}
		}
	}
	return named
}

// InThePast is a resolved day read the way a ledger reads one: backwards.
//
// calendarDate resolves a bare "5 September" to the *next* one, because it was
// written for deadlines and a deadline is ahead of you. An expense is behind
// you: money spent on "5 September", said in December, was spent this year, and
// resolving it forward records a purchase dated next year -- which no total for
// any period the user asks about will ever include.
//
// Only a bare calendar date is moved. "tomorrow" is also in the future and is
// left exactly where it is: it is not an inferred year, it is what the user
// said, and the caller decides whether to accept it.
func InThePast(raw string, day, now time.Time) time.Time {
	if !NamesABareCalendarDate(raw) {
		return day
	}
	today := time.Date(now.UTC().Year(), now.UTC().Month(), now.UTC().Day(), 0, 0, 0, 0, time.UTC)
	if day.After(today) {
		return day.AddDate(-1, 0, 0)
	}
	return day
}

// nextNPeriods reads "the next 3 days", "next 2 weeks", "48 hours".
var nextNPeriods = regexp.MustCompile(`^(?:next |coming |)(\d+)\s*(hour|day|week|month)s?$`)
