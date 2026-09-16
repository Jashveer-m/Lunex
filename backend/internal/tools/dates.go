package tools

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ParseDate reads a deadline the way a model is likely to have written it, and
// resolves it against now.
//
// The routing prompt tells the model to copy a relative date exactly as the
// user wrote it rather than to work one out, and this is why: date arithmetic
// is exactly the thing a 3B model gets confidently wrong ("next Friday" from a
// Thursday lands on the wrong week about as often as the right one), and it is
// a thing Go does exactly. So the model's job is to find the phrase and this
// function's job is to know what it means.
//
// Accepted: an RFC 3339 timestamp (kept as written), a calendar date
// (2026-09-30, "30 September", "September 30, 2026", "30th Sep"), today,
// tonight, tomorrow, the day after tomorrow, a weekday with or without
// this/next/on/by ("friday", "next fri"), "in N days/weeks/months", next
// week/month, and the end of the week/month/year.
//
// A weekday is always the next one strictly after today, and "next friday"
// means the same as "friday": the two readings of "next" split people about
// evenly, and picking the nearer one means a wrong guess is a deadline that
// arrives early rather than one that is missed. The proposal shows the
// resolved date, and the user approves or rejects exactly that.
//
// A date without a time is midnight UTC on that day -- the same convention the
// chat prompt renders a deadline in. Anything else is an error, and the caller
// turns it into a request for a date it can read rather than guessing.
func ParseDate(s string, now time.Time) (time.Time, error) {
	raw := s
	s = strings.ToLower(strings.TrimSpace(s))
	for _, prefix := range []string{"due ", "by ", "on ", "before ", "until ", "for "} {
		s = strings.TrimPrefix(s, prefix)
	}
	s = strings.TrimSpace(strings.TrimSuffix(s, "."))

	if t, err := time.Parse(time.RFC3339, strings.ToUpper(s)); err == nil {
		return t.UTC(), nil
	}
	if t, err := time.Parse(time.DateOnly, s); err == nil {
		return t, nil
	}

	// Today is today *in UTC* (docs/decisions.md, 35), which is the date the
	// chat prompt tells the model it is. Reading the calendar fields off a
	// local-time now -- which is what time.Now returns -- put "today" a day
	// ahead of the prompt's date on any server east of Greenwich for part of
	// every day (00:00-05:30 in IST), so "tomorrow" resolved to the day after
	// the one the assistant then named.
	now = now.UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	switch s {
	case "today", "tonight", "this evening", "end of day", "end of today", "eod":
		return today, nil
	case "tomorrow", "tmrw", "tomorrow morning", "tomorrow evening", "tomorrow night":
		return today.AddDate(0, 0, 1), nil
	case "the day after tomorrow", "day after tomorrow":
		return today.AddDate(0, 0, 2), nil
	case "next week", "in a week", "a week from now", "a week from today":
		return today.AddDate(0, 0, 7), nil
	case "next month", "in a month", "a month from now":
		return today.AddDate(0, 1, 0), nil
	case "end of the week", "end of week", "the end of the week", "this weekend", "the weekend", "weekend":
		return nextWeekday(today, time.Sunday, true), nil
	case "end of the month", "end of month", "the end of the month":
		return time.Date(today.Year(), today.Month()+1, 0, 0, 0, 0, 0, time.UTC), nil
	case "end of the year", "end of year", "the end of the year":
		return time.Date(today.Year(), time.December, 31, 0, 0, 0, 0, time.UTC), nil
	}

	if m := relativeDate.FindStringSubmatch(s); m != nil {
		n, err := strconv.Atoi(m[1])
		if err != nil || n <= 0 || n > 3650 {
			return time.Time{}, fmt.Errorf("could not read %q as a date", raw)
		}
		switch m[2] {
		case "day":
			return today.AddDate(0, 0, n), nil
		case "week":
			return today.AddDate(0, 0, 7*n), nil
		case "month":
			return today.AddDate(0, n, 0), nil
		}
	}

	words := strings.Fields(s)
	if len(words) >= 1 && len(words) <= 2 {
		day := words[len(words)-1]
		if len(words) == 1 || words[0] == "this" || words[0] == "next" || words[0] == "coming" {
			if wd, ok := weekdays[day]; ok {
				return nextWeekday(today, wd, false), nil
			}
		}
	}

	if t, ok := calendarDate(s, today); ok {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("could not read %q as a date", raw)
}

var relativeDate = regexp.MustCompile(`^in (\d+) (day|week|month)s?$`)

var weekdays = map[string]time.Weekday{
	"sunday": time.Sunday, "sun": time.Sunday,
	"monday": time.Monday, "mon": time.Monday,
	"tuesday": time.Tuesday, "tue": time.Tuesday, "tues": time.Tuesday,
	"wednesday": time.Wednesday, "wed": time.Wednesday,
	"thursday": time.Thursday, "thu": time.Thursday, "thur": time.Thursday, "thurs": time.Thursday,
	"friday": time.Friday, "fri": time.Friday,
	"saturday": time.Saturday, "sat": time.Saturday,
}

// nextWeekday is the first wd strictly after today -- or today itself, when
// includeToday is set and today is wd ("the end of the week" on a Sunday is
// today, not a week away).
func nextWeekday(today time.Time, wd time.Weekday, includeToday bool) time.Time {
	d := (int(wd) - int(today.Weekday()) + 7) % 7
	if d == 0 && !includeToday {
		d = 7
	}
	return today.AddDate(0, 0, d)
}

var months = map[string]time.Month{
	"january": time.January, "jan": time.January,
	"february": time.February, "feb": time.February,
	"march": time.March, "mar": time.March,
	"april": time.April, "apr": time.April,
	"may":  time.May,
	"june": time.June, "jun": time.June,
	"july": time.July, "jul": time.July,
	"august": time.August, "aug": time.August,
	"september": time.September, "sep": time.September, "sept": time.September,
	"october": time.October, "oct": time.October,
	"november": time.November, "nov": time.November,
	"december": time.December, "dec": time.December,
}

var ordinal = regexp.MustCompile(`^(\d{1,2})(st|nd|rd|th)?$`)

// calendarDate reads "30 september", "september 30", "sep 30 2026" and
// "30th sept, 2026". Without a year it is the next such date that is not in
// the past, so "30 September" in December means next September.
func calendarDate(s string, today time.Time) (time.Time, bool) {
	s = strings.NewReplacer(",", " ", "of ", " ").Replace(s)
	words := strings.Fields(s)
	if len(words) < 2 || len(words) > 3 {
		return time.Time{}, false
	}

	var day int
	var month time.Month
	year := 0
	for i, w := range words {
		if m, ok := months[w]; ok && month == 0 {
			month = m
			continue
		}
		if m := ordinal.FindStringSubmatch(w); m != nil && day == 0 {
			day, _ = strconv.Atoi(m[1])
			continue
		}
		if i == len(words)-1 && len(w) == 4 {
			if y, err := strconv.Atoi(w); err == nil && y >= 2000 && y <= 2100 {
				year = y
				continue
			}
		}
		return time.Time{}, false
	}
	if month == 0 || day < 1 || day > 31 {
		return time.Time{}, false
	}

	if year == 0 {
		year = today.Year()
		if time.Date(year, month, day, 0, 0, 0, 0, time.UTC).Before(today) {
			year++
		}
	}
	t := time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
	// time.Date normalizes 31 September to 1 October. A date that moved is a
	// date that does not exist, and is refused rather than silently shifted.
	if t.Day() != day || t.Month() != month {
		return time.Time{}, false
	}
	return t, true
}

// formatDay renders a resolved date the way a proposal shows it.
func formatDay(t time.Time) string { return t.UTC().Format("Mon 2 Jan 2006") }
