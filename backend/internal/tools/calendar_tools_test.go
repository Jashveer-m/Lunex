package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// testNow is Thursday 10 September 2026, 15:30 UTC (fakes_test.go), so
// "tomorrow" is Friday the 11th and "this week" is Mon 7 -- Mon 14.

// --- create_calendar_event --------------------------------------------------------

// The call as the model writes it, turned into the interval the user approves:
// the relative date and the time of day resolved by Go rather than by the
// model, and an end that follows from what was said.
func TestCreateCalendarEventResolvesWhenItIs(t *testing.T) {
	w := newWorld()
	for name, tc := range map[string]struct {
		args    Args
		start   string
		end     string
		allDay  bool
		summary string
	}{
		"a time of day": {
			Args{"title": "Dentist", "start": "tomorrow at 3pm"},
			"2026-09-11T15:00:00Z", "2026-09-11T16:00:00Z", false,
			`Add "Dentist" to the calendar on Fri 11 Sep 2026 15:00–16:00.`,
		},
		"both ends": {
			Args{"title": "Standup", "start": "friday 09:30", "end": "09:45"},
			"2026-09-11T09:30:00Z", "2026-09-11T09:45:00Z", false,
			`Add "Standup" to the calendar on Fri 11 Sep 2026 09:30–09:45.`,
		},
		"a duration": {
			Args{"title": "Call", "start": "2026-09-30 14:00", "duration_minutes": "30"},
			"2026-09-30T14:00:00Z", "2026-09-30T14:30:00Z", false,
			`Add "Call" to the calendar on Wed 30 Sep 2026 14:00–14:30.`,
		},
		"a duration in hours": {
			Args{"title": "Workshop", "start": "2026-09-30 09:00", "duration": "2 hours"},
			"2026-09-30T09:00:00Z", "2026-09-30T11:00:00Z", false,
			`Add "Workshop" to the calendar on Wed 30 Sep 2026 09:00–11:00.`,
		},
		"past midnight": {
			Args{"title": "Sky watch", "start": "friday 9pm", "end": "1am"},
			"2026-09-11T21:00:00Z", "2026-09-12T01:00:00Z", false,
			`Add "Sky watch" to the calendar from Fri 11 Sep 2026 21:00 to Sat 12 Sep 2026 01:00.`,
		},
		// No time of day was stated, so none is invented: it is an all-day
		// event, which is true and editable.
		"no time given": {
			Args{"title": "Conference", "start": "2026-10-01"},
			"2026-10-01T00:00:00Z", "2026-10-02T00:00:00Z", true,
			`Add "Conference" to the calendar all day on Thu 1 Oct 2026.`,
		},
		// A date-only end of a timed event is the whole of that day.
		"a timed start and a day for an end": {
			Args{"title": "Conference", "start": "2026-10-01 09:00", "end": "2026-10-03"},
			"2026-10-01T09:00:00Z", "2026-10-04T00:00:00Z", false,
			`Add "Conference" to the calendar from Thu 1 Oct 2026 09:00 to Sun 4 Oct 2026 00:00.`,
		},
		"a span of days": {
			Args{"title": "Conference", "start": "2026-10-01", "end": "2026-10-03"},
			"2026-10-01T00:00:00Z", "2026-10-04T00:00:00Z", true,
			`Add "Conference" to the calendar all day from Thu 1 Oct 2026 to Sat 3 Oct 2026.`,
		},
		"all day stated outright": {
			Args{"title": "Leave", "start": "tomorrow", "all_day": true},
			"2026-09-11T00:00:00Z", "2026-09-12T00:00:00Z", true,
			`Add "Leave" to the calendar all day on Fri 11 Sep 2026.`,
		},
		"with a place": {
			Args{"title": "Lunch", "start": "tomorrow at noon", "location": "the canteen"},
			"2026-09-11T12:00:00Z", "2026-09-11T13:00:00Z", false,
			`Add "Lunch" to the calendar on Fri 11 Sep 2026 12:00–13:00 (at the canteen).`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			call := prepare(t, w, CreateCalendarEvent, tc.args)
			in := input(t, call)
			if in["start"] != tc.start || in["end"] != tc.end {
				t.Fatalf("interval = %v to %v, want %v to %v", in["start"], in["end"], tc.start, tc.end)
			}
			if allDay, _ := in["all_day"].(bool); allDay != tc.allDay {
				t.Fatalf("all_day = %v, want %v", in["all_day"], tc.allDay)
			}
			if call.Permission != Write {
				t.Fatalf("permission = %v", call.Permission)
			}
			if call.Summary != tc.summary {
				t.Fatalf("summary  = %q\nwant     = %q", call.Summary, tc.summary)
			}
		})
	}
}

// Strict about content: each of these is a proposal the user must never be
// shown, because it would fail on approval or means nothing.
func TestCreateCalendarEventDeclinesWhatItCannotPropose(t *testing.T) {
	w := newWorld()
	for name, tc := range map[string]struct {
		args Args
		want string
	}{
		"no title": {Args{"start": "tomorrow"}, "needs a title"},
		"no start": {Args{"title": "Dentist"}, "needs a start"},
		"an unreadable start": {Args{"title": "x", "start": "sometime soon"},
			"could not read"},
		"an unreadable end": {Args{"title": "x", "start": "tomorrow at 3pm", "end": "whenever"},
			"could not read"},
		"backwards days": {Args{"title": "x", "start": "2026-10-05", "end": "2026-10-01"},
			"ends before it starts"},
	} {
		t.Run(name, func(t *testing.T) {
			if reason := declined(t, w, CreateCalendarEvent, tc.args); !strings.Contains(reason, tc.want) {
				t.Fatalf("reason = %q, want it to mention %q", reason, tc.want)
			}
		})
	}
	if n := w.totalWrites(); n != 0 {
		t.Fatalf("a declined proposal wrote %d times", n)
	}
}

// Approving is what writes, and it writes the interval the proposal showed.
func TestAnApprovedEventIsCreatedAsProposed(t *testing.T) {
	w := newWorld()
	call := prepare(t, w, CreateCalendarEvent, Args{"title": "Dentist", "start": "tomorrow at 3pm"})
	if n := w.totalWrites(); n != 0 {
		t.Fatalf("preparing wrote %d times", n)
	}
	id := w.ledger.add(w.user, call, "proposed")
	exec, err := w.reg.RunApproved(context.Background(), w.user, id)
	if err != nil || exec.Err != nil {
		t.Fatal(err, exec.Err)
	}
	if len(w.calendar.creates) != 1 {
		t.Fatalf("creates = %+v", w.calendar.creates)
	}
	created := w.calendar.creates[0]
	if !created.StartTime.Equal(time.Date(2026, 9, 11, 15, 0, 0, 0, time.UTC)) ||
		!created.EndTime.Equal(time.Date(2026, 9, 11, 16, 0, 0, 0, time.UTC)) {
		t.Fatalf("created %v to %v", created.StartTime, created.EndTime)
	}
	if len(exec.Result.Events) != 1 || exec.Result.Count() != 1 {
		t.Fatalf("result = %+v", exec.Result)
	}
}

// --- search_calendar --------------------------------------------------------------

func TestSearchCalendarResolvesTheWindow(t *testing.T) {
	w := newWorld()
	for name, tc := range map[string]struct {
		args    Args
		start   string
		end     string
		summary string
	}{
		// No dates at all is the default window, and the summary says which
		// days it looked at -- the default is disclosed, never silent.
		"nothing given": {
			Args{}, "2026-09-10T00:00:00Z", "2026-09-17T00:00:00Z",
			"Look at the calendar from Thu 10 Sep 2026 to Wed 16 Sep 2026.",
		},
		"a single day": {
			Args{"start": "tomorrow"}, "2026-09-11T00:00:00Z", "2026-09-12T00:00:00Z",
			"Look at the calendar on Fri 11 Sep 2026.",
		},
		"a named stretch": {
			Args{"start": "next week"}, "2026-09-14T00:00:00Z", "2026-09-21T00:00:00Z",
			"Look at the calendar from Mon 14 Sep 2026 to Sun 20 Sep 2026.",
		},
		"a weekend": {
			Args{"when": "this weekend"}, "2026-09-12T00:00:00Z", "2026-09-14T00:00:00Z",
			"Look at the calendar from Sat 12 Sep 2026 to Sun 13 Sep 2026.",
		},
		// The far end is inclusive of its day: "Monday to Friday" is a working
		// week, not four days.
		"a range": {
			Args{"start": "2026-09-14", "end": "2026-09-18"}, "2026-09-14T00:00:00Z", "2026-09-19T00:00:00Z",
			"Look at the calendar from Mon 14 Sep 2026 to Fri 18 Sep 2026.",
		},
		"an end alone": {
			Args{"until": "2026-09-12"}, "2026-09-10T00:00:00Z", "2026-09-13T00:00:00Z",
			"Look at the calendar from Thu 10 Sep 2026 to Sat 12 Sep 2026.",
		},
		// A named event is worth looking further ahead for than a look at the
		// diary is.
		"a named event": {
			Args{"query": "dentist"}, "2026-09-10T00:00:00Z", "2026-12-09T00:00:00Z",
			`Look at the calendar for "dentist" from Thu 10 Sep 2026 to Tue 8 Dec 2026.`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			call := prepare(t, w, SearchCalendar, tc.args)
			in := input(t, call)
			if in["start"] != tc.start || in["end"] != tc.end {
				t.Fatalf("window = %v to %v, want %v to %v", in["start"], in["end"], tc.start, tc.end)
			}
			if call.Permission != Read {
				t.Fatalf("permission = %v", call.Permission)
			}
			if call.Summary != tc.summary {
				t.Fatalf("summary  = %q\nwant     = %q", call.Summary, tc.summary)
			}
		})
	}

	if reason := declined(t, w, SearchCalendar, Args{"start": "the 32nd of Octember"}); !strings.Contains(reason, "could not read") {
		t.Fatalf("reason = %q", reason)
	}
	if reason := declined(t, w, SearchCalendar, Args{"start": "2026-09-18", "end": "2026-09-14"}); !strings.Contains(reason, "ends before it starts") {
		t.Fatalf("reason = %q", reason)
	}
}

func TestSearchCalendarReturnsTheEventsInTheWindow(t *testing.T) {
	w := newWorld()
	tomorrow := time.Date(2026, 9, 11, 9, 0, 0, 0, time.UTC)
	standup := w.calendar.seed(w.user, "Standup", tomorrow, time.Hour)
	w.calendar.seed(w.user, "Retro next month", tomorrow.AddDate(0, 1, 0), time.Hour)
	w.calendar.seed(uuid.New(), "Somebody else's standup", tomorrow, time.Hour)

	res, err := w.reg.RunRead(context.Background(), w.user,
		prepare(t, w, SearchCalendar, Args{"start": "tomorrow"}))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Events) != 1 || res.Events[0].ID != standup.ID {
		t.Fatalf("found %+v, want only tomorrow's own event", res.Events)
	}
	// The recorded output names the window as well as what was in it, so the
	// action log says which days "nothing on" was about.
	out, _ := json.Marshal(res.Output)
	for _, want := range []string{`"count":1`, `"more":false`, `"start":"2026-09-11`, `"end":"2026-09-12`} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("output = %s, want it to contain %s", out, want)
		}
	}

	// And a window with nothing in it finds nothing rather than widening
	// itself.
	res, err = w.reg.RunRead(context.Background(), w.user,
		prepare(t, w, SearchCalendar, Args{"start": "2026-09-14", "end": "2026-09-18"}))
	if err != nil {
		t.Fatal(err)
	}
	if res.Count() != 0 {
		t.Fatalf("a quiet week returned %+v", res.Events)
	}
}

func TestSearchCalendarNeverCrossesUsers(t *testing.T) {
	w := newWorld()
	w.calendar.seed(uuid.New(), "Their standup", time.Date(2026, 9, 11, 9, 0, 0, 0, time.UTC), time.Hour)
	res, err := w.reg.RunRead(context.Background(), w.user, prepare(t, w, SearchCalendar, Args{"start": "tomorrow"}))
	if err != nil {
		t.Fatal(err)
	}
	if res.Count() != 0 {
		t.Fatalf("search_calendar found another user's events: %+v", res.Events)
	}
}
