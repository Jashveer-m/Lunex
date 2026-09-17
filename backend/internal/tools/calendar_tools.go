package tools

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/calendar"
	"github.com/jashveer/lifeos/backend/internal/httpx"
)

// eventRecord is an event as a tool reports it: what is on, when, and where.
type eventRecord struct {
	ID       string  `json:"id"`
	Title    string  `json:"title"`
	Start    string  `json:"start"`
	End      string  `json:"end"`
	AllDay   bool    `json:"all_day"`
	Location *string `json:"location"`
}

func toEventRecord(e calendar.Event) eventRecord {
	return eventRecord{
		ID: e.ID.String(), Title: e.Title,
		Start:  e.StartTime.UTC().Format(httpx.TimeFormat),
		End:    e.EndTime.UTC().Format(httpx.TimeFormat),
		AllDay: e.AllDay, Location: e.Location,
	}
}

var eventRecordSchema = object(map[string]Schema{
	"id":       uuidField("the event's id"),
	"title":    str("what the event is"),
	"start":    timestamp("when it starts"),
	"end":      timestamp("when it ends"),
	"all_day":  boolean("whether it takes the whole day"),
	"location": str("where it is, or null"),
}, "id", "title", "start", "end", "all_day", "location")

// The window a calendar read uses when the user named no dates.
//
// A default rather than a refusal, because "what is on my calendar?" is a
// question with an obvious answer and no dates in it. It is not the invented
// filter the hardening pass forbids for two reasons: it is the same window
// every time rather than a guess at what the user meant, and the summary the
// user and the model both see names the dates it used -- so an answer drawn
// from the wrong week says which week it was.
const (
	// DefaultCalendarDays is a look at the diary: the week ahead.
	DefaultCalendarDays = 7
	// NamedSearchDays is how far ahead a search for a *named* event looks. The
	// dentist appointment somebody is trying to remember is not usually in the
	// next seven days, and a search that says "nothing matched" because of the
	// window rather than because of the calendar is the failure this number
	// exists to avoid.
	NamedSearchDays = 90
	// DefaultEventMinutes is how long an event lasts when the user said when it
	// starts and not when it ends.
	DefaultEventMinutes = 60
)

// Argument aliases for the two grounded window arguments.
//
// They are declared on the Param rather than only read in prepare, because the
// router drops an ungrounded filter by *key*: a "when" the tool reads but the
// declaration does not mention is a window value that escapes the grounding
// check. See Param.Aliases and Router.dropUngroundedFilters.
var (
	startAliases = []string{"from", "when", "range", "period", "date", "day", "on", "after"}
	endAliases   = []string{"to", "until", "till", "through", "before"}
)

// --- search_calendar ------------------------------------------------------------

// searchCalendarInput is the resolved window, canonical (RFC 3339, UTC).
type searchCalendarInput struct {
	Start string `json:"start"`
	End   string `json:"end"`
	Query string `json:"query,omitempty"`
}

func searchCalendarTool(s Services) Tool {
	return define(Tool{
		Name: SearchCalendar,
		Description: "Look at the user's calendar: the events between two dates, optionally only those matching a word or phrase. " +
			"Use it for what is on today, this week, or on a particular day.",
		Permission: Read,
		Params: []Param{
			{Name: "start", Type: "string", Filter: true, Aliases: startAliases,
				Description: `the first day to look at, in the user's own words ("today", "monday", "next week") or as YYYY-MM-DD`},
			{Name: "end", Type: "string", Filter: true, Aliases: endAliases,
				Description: "the last day to look at, if the user gave a range"},
			{Name: "query", Type: "string", Description: "a word or phrase from the event, if the user named one"},
		},
		Output: object(map[string]Schema{
			"start":  timestamp("the first moment of the window searched"),
			"end":    timestamp("the first moment after it"),
			"count":  integer("how many events are listed"),
			"more":   boolean("whether more events fell in the window than are listed"),
			"events": listOf(eventRecordSchema),
		}, "start", "end", "count", "more", "events"),
	},
		func(_ context.Context, _ uuid.UUID, a Args) (searchCalendarInput, error) {
			in := searchCalendarInput{Query: a.String("query", "q", "search", "text", "keyword", "title", "event")}
			start, end, err := calendarWindow(SearchCalendar, a, s.Now(), in.Query != "")
			if err != nil {
				return in, err
			}
			in.Start, in.End = start.Format(time.RFC3339), end.Format(time.RFC3339)
			return in, nil
		},
		func(ctx context.Context, userID uuid.UUID, in searchCalendarInput) (Result, error) {
			start, end := storedTime(in.Start), storedTime(in.End)
			if start.IsZero() || end.IsZero() {
				return Result{}, fmt.Errorf("search calendar: the stored window is not a pair of timestamps")
			}
			found, more, err := search(in.Query, func(term string, limit int) ([]calendar.Event, error) {
				return s.Calendar.List(ctx, userID, calendar.Filter{
					Start: start, End: end, Query: term, Sort: calendar.DefaultSort, Limit: limit,
				})
			}, func(e calendar.Event) uuid.UUID { return e.ID })
			if err != nil {
				return Result{}, fmt.Errorf("search calendar: %w", err)
			}
			records := make([]eventRecord, 0, len(found))
			for _, e := range found {
				records = append(records, toEventRecord(e))
			}
			return Result{
				Output: map[string]any{
					"start": start.UTC().Format(httpx.TimeFormat), "end": end.UTC().Format(httpx.TimeFormat),
					"count": len(records), "more": more, "events": records,
				},
				Events: found, More: more,
			}, nil
		},
		func(in searchCalendarInput) string {
			out := "Look at the calendar"
			if in.Query != "" {
				out += " for " + quoted(in.Query)
			}
			return out + " " + windowPhrase(storedTime(in.Start), storedTime(in.End)) + "."
		},
	)
}

// calendarWindow resolves the window a calendar read runs over.
//
// Both ends may be absent -- the router drops one the user's message does not
// give -- and each absence has an answer that does not involve guessing: no
// dates at all is the default window from today, a start alone is that one day
// (or the stretch a phrase like "next week" names), and an end alone is from
// today to that day.
func calendarWindow(tool string, a Args, now time.Time, named bool) (start, end time.Time, err error) {
	rawStart := a.String(append([]string{"start", "start_time"}, startAliases...)...)
	rawEnd := a.String(append([]string{"end", "end_time"}, endAliases...)...)

	utc := now.UTC()
	today := time.Date(utc.Year(), utc.Month(), utc.Day(), 0, 0, 0, 0, time.UTC)
	ahead := DefaultCalendarDays
	if named {
		ahead = NamedSearchDays
	}

	switch {
	case rawStart == "" && rawEnd == "":
		return today, today.AddDate(0, 0, ahead), nil
	case rawEnd == "":
		// One phrase, which may name a stretch ("next week") or a day.
		start, end, err = ParseWindow(rawStart, now)
		if err != nil {
			return time.Time{}, time.Time{}, unreadableDate(tool, rawStart)
		}
		return start, end, nil
	case rawStart == "":
		m, err := ParseMoment(rawEnd, now)
		if err != nil {
			return time.Time{}, time.Time{}, unreadableDate(tool, rawEnd)
		}
		return today, endOfWindow(m), nil
	}

	from, err := ParseMoment(rawStart, now)
	if err != nil {
		return time.Time{}, time.Time{}, unreadableDate(tool, rawStart)
	}
	to, err := ParseMoment(rawEnd, now)
	if err != nil {
		return time.Time{}, time.Time{}, unreadableDate(tool, rawEnd)
	}
	start, end = from.Time, endOfWindow(to)
	if !end.After(start) {
		return time.Time{}, time.Time{}, invalid(tool,
			"the range ends before it starts: ask the user which dates they mean")
	}
	return start, end, nil
}

// endOfWindow turns the far end of a range into the exclusive bound of a
// half-open window. A date means the whole of that day -- "Monday to Friday"
// includes Friday's events, which is what anybody asking means and what an
// exclusive bound taken literally would drop.
func endOfWindow(m Moment) time.Time {
	if m.HasTime {
		return m.Time
	}
	return m.Day().AddDate(0, 0, 1)
}

func unreadableDate(tool, raw string) error {
	return invalid(tool, "could not read %s as a date: ask the user for a date such as 2026-09-30", quoted(raw))
}

// --- create_calendar_event --------------------------------------------------------

type createCalendarEventInput struct {
	Title       string `json:"title"`
	Start       string `json:"start"`
	End         string `json:"end"`
	AllDay      bool   `json:"all_day,omitempty"`
	Location    string `json:"location,omitempty"`
	Description string `json:"description,omitempty"`
}

func createCalendarEventTool(s Services) Tool {
	return define(Tool{
		Name:        CreateCalendarEvent,
		Description: "Propose a new calendar event. It is only added to the calendar after the user approves it.",
		Permission:  Write,
		Params: []Param{
			{Name: "title", Type: "string", Description: "what the event is", Required: true},
			{Name: "start", Type: "string", Required: true,
				Description: `when it starts, in the user's own words ("tomorrow at 3pm", "friday 09:30") or as YYYY-MM-DD`},
			{Name: "end", Type: "string", Description: "when it ends, if the user said"},
			{Name: "duration_minutes", Type: "string", Description: "how long it lasts in minutes, if the user said that instead"},
			{Name: "all_day", Type: "string", Description: "true if it takes the whole day"},
			{Name: "location", Type: "string", Description: "where it is, if the user said"},
			{Name: "description", Type: "string", Description: "more detail, if the user gave any"},
		},
		Output: object(map[string]Schema{"event": eventRecordSchema}, "event"),
	},
		func(_ context.Context, _ uuid.UUID, a Args) (createCalendarEventInput, error) {
			in := createCalendarEventInput{
				Title:       a.String("title", "name", "event", "summary"),
				Location:    a.String("location", "where", "place", "venue"),
				Description: a.String("description", "details", "notes"),
			}
			if in.Title == "" {
				return in, invalid(CreateCalendarEvent, "an event needs a title: ask the user what it is")
			}
			start, end, allDay, err := eventInterval(a, s.Now())
			if err != nil {
				return in, err
			}
			in.Start, in.End, in.AllDay = start.Format(time.RFC3339), end.Format(time.RFC3339), allDay

			// The service's own validation, run now so the user is never shown a
			// proposal that would fail on approval.
			v, err := calendar.ValidateCreate(in.toService())
			if err != nil {
				return in, fieldProblems(CreateCalendarEvent, err)
			}
			in.Title, in.Description, in.Location = v.Title, v.Description, v.Location
			return in, nil
		},
		func(ctx context.Context, userID uuid.UUID, in createCalendarEventInput) (Result, error) {
			e, err := s.Calendar.Create(ctx, userID, in.toService())
			if err != nil {
				return Result{}, err
			}
			return Result{Output: map[string]any{"event": toEventRecord(e)}, Events: []calendar.Event{e}}, nil
		},
		func(in createCalendarEventInput) string {
			return "Add " + quoted(in.Title) + " to the calendar " +
				intervalPhrase(storedTime(in.Start), storedTime(in.End), in.AllDay) +
				details("at", in.Location) + "."
		},
	)
}

func (in createCalendarEventInput) toService() calendar.CreateInput {
	out := calendar.CreateInput{
		Title: in.Title, Description: in.Description, Location: in.Location, AllDay: in.AllDay,
	}
	out.StartTime, out.EndTime = storedTime(in.Start), storedTime(in.End)
	return out
}

// eventInterval works out when a proposed event runs, from what the user
// actually said.
//
// The rule is the hardening pass's, applied to a time of day: a start with no
// time is an all-day event, not nine in the morning. A model that writes
// "tomorrow" for "book the dentist tomorrow" has said everything the user did,
// and the proposal shows an all-day Thursday -- which is true, editable, and
// not a meeting invented for a time nobody chose.
func eventInterval(a Args, now time.Time) (start, end time.Time, allDay bool, err error) {
	rawStart := a.String("start", "start_time", "when", "date", "time", "at", "on", "begins", "from")
	if rawStart == "" {
		return start, end, false, invalid(CreateCalendarEvent,
			"an event needs a start: ask the user when it is")
	}
	from, err := ParseMoment(rawStart, now)
	if err != nil {
		return start, end, false, unreadableDate(CreateCalendarEvent, rawStart)
	}

	stated, statedSet := a.Bool("all_day", "allday", "full_day", "whole_day")
	allDay = (statedSet && stated) || !from.HasTime

	rawEnd := a.String("end", "end_time", "until", "till", "finishes", "ends", "to")
	var toMoment *Moment
	if rawEnd != "" {
		// Resolved against the start rather than against today, so a bare
		// "4pm" lands on the day the event is on.
		m, err := ParseMoment(rawEnd, from.Time)
		if err != nil {
			return start, end, false, unreadableDate(CreateCalendarEvent, rawEnd)
		}
		toMoment = &m
		if m.HasTime {
			// An end stated as a time of day that is not after its start, with
			// no day of its own, is an event running past midnight -- the only
			// reading of "9pm to 1am" that is not an error.
			if !m.Time.After(from.Time) && !m.HasDate {
				m.Time = m.Time.AddDate(0, 0, 1)
				toMoment = &m
			}
			// A stated end time settles it: this is not an all-day event
			// unless the user also said that it was.
			allDay = statedSet && stated
		}
	}

	if allDay {
		start = from.Day()
		end = start.AddDate(0, 0, 1)
		if toMoment != nil {
			end = toMoment.Day().AddDate(0, 0, 1)
		}
		if !end.After(start) {
			return start, end, allDay, invalid(CreateCalendarEvent,
				"the event ends before it starts: ask the user which dates they mean")
		}
		return start, end, true, nil
	}

	start = from.Time
	switch {
	case toMoment != nil:
		// A date-only end means the whole of that day, the same inclusive
		// reading a calendar window's far end gets: "from Thursday 9am until
		// Sunday" includes Sunday.
		end = endOfWindow(*toMoment)
	default:
		end = start.Add(DefaultEventMinutes * time.Minute)
		if mins := durationMinutes(a); mins > 0 {
			end = start.Add(time.Duration(mins) * time.Minute)
		}
	}
	if end.Before(start) {
		return start, end, false, invalid(CreateCalendarEvent,
			"the event ends before it starts: ask the user when it runs from and to")
	}
	return start, end, false, nil
}

// durationMinutes reads a stated length. Anything that is not a plain positive
// number of minutes is no duration at all, and the default applies -- a
// proposal that says an hour is honest about being a default; one that says
// four days because "4" was read out of "4 people" is not.
func durationMinutes(a Args) int {
	raw := a.String("duration_minutes", "duration", "minutes", "length")
	if raw == "" {
		return 0
	}
	fields := strings.Fields(strings.ToLower(raw))
	if len(fields) == 0 {
		return 0
	}
	n := 0
	if _, err := fmt.Sscanf(fields[0], "%d", &n); err != nil || n <= 0 {
		return 0
	}
	unit := ""
	if len(fields) > 1 {
		unit = fields[1]
	}
	switch {
	case strings.HasPrefix(unit, "hour"), unit == "h", unit == "hr", unit == "hrs":
		n *= 60
	case strings.HasPrefix(unit, "day"):
		n *= 60 * 24
	}
	if n > 60*24*366 {
		return 0
	}
	return n
}

// --- rendering ------------------------------------------------------------------

// storedTime reads a canonical timestamp back. The canonical form was written
// by this package, so a failure here is a row that was not -- and is answered
// with the zero time, which every caller checks.
func storedTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

// formatMoment renders a resolved timestamp for a summary: "Fri 11 Sep 2026
// 14:00", or just the day when there is no time of day to show.
func formatMoment(t time.Time, withTime bool) string {
	if !withTime {
		return formatDay(t)
	}
	return t.UTC().Format("Mon 2 Jan 2006 15:04")
}

// windowPhrase renders the range a calendar read covers, in the inclusive
// terms a person reads it in: a window ending at midnight is named by the day
// before, because [Mon, Tue) is Monday, not "Monday to Tuesday".
func windowPhrase(start, end time.Time) string {
	if start.IsZero() || end.IsZero() {
		return "(over a window that could not be read)"
	}
	last := end.Add(-time.Nanosecond)
	if sameDay(start, last) {
		return "on " + formatDay(start)
	}
	return "from " + formatDay(start) + " to " + formatDay(last)
}

// intervalPhrase renders when a proposed event runs.
func intervalPhrase(start, end time.Time, allDay bool) string {
	if start.IsZero() || end.IsZero() {
		return "(at a time that could not be read)"
	}
	if allDay {
		last := end.Add(-time.Nanosecond)
		if sameDay(start, last) {
			return "all day on " + formatDay(start)
		}
		return "all day from " + formatDay(start) + " to " + formatDay(last)
	}
	if sameDay(start, end) {
		return "on " + formatMoment(start, true) + "–" + end.UTC().Format("15:04")
	}
	return "from " + formatMoment(start, true) + " to " + formatMoment(end, true)
}

func sameDay(a, b time.Time) bool {
	return a.UTC().Format(time.DateOnly) == b.UTC().Format(time.DateOnly)
}
