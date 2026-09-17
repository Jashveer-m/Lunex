package chat

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/actions"
	"github.com/jashveer/lifeos/backend/internal/ai"
	"github.com/jashveer/lifeos/backend/internal/calendar"
	"github.com/jashveer/lifeos/backend/internal/tools"
)

// The harness clock is Wednesday 9 September 2026, 12:00 UTC.
var (
	today    = time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC)
	thisPM   = today.Add(16 * time.Hour)
	tomorrow = today.AddDate(0, 0, 1)
)

// withEvent puts one event on the harness user's calendar.
func (h *harness) withEvent(title string, start time.Time, d time.Duration) calendar.Event {
	e := calendar.Event{
		ID: uuid.New(), UserID: h.user, Title: title, StartTime: start, EndTime: start.Add(d),
	}
	h.calendar.byUser[h.user] = append(h.calendar.byUser[h.user], e)
	return e
}

// --- the heuristic ------------------------------------------------------------

// The property the phase's chat half exists for: what is on today reaches the
// model as background, so "what does my day look like" is answerable at all.
func TestTodaysEventsAreRetrievedAsContext(t *testing.T) {
	h := newHarness(t, &ai.Mock{ReplyFunc: func(msgs []ai.Message) string {
		if strings.Contains(ai.PromptText(msgs), "Dentist") {
			return "You have the dentist at 16:00 today [S1]."
		}
		return "I could not find anything about that."
	}})
	event := h.withEvent("Dentist", thisPM, time.Hour)

	turn, _, err := h.send(t, "What does my day look like?")
	if err != nil {
		t.Fatal(err)
	}

	// The window: from the top of today, so an event that began this morning is
	// still in front of the model this afternoon.
	if len(h.calendar.filters) != 1 {
		t.Fatalf("the calendar was read %d times, want once", len(h.calendar.filters))
	}
	f := h.calendar.filters[0]
	if !f.Start.Equal(today) {
		t.Fatalf("the window starts %v, want the top of today", f.Start)
	}
	if want := h.svc.now().UTC().Add(EventWindow); !f.End.Equal(want) {
		t.Fatalf("the window ends %v, want %v", f.End, want)
	}
	if f.Limit != MaxEvents {
		t.Fatalf("limit = %d, want %d", f.Limit, MaxEvents)
	}
	if h.calendar.callers[0] != h.user {
		t.Fatalf("the calendar was read as %v, not the caller", h.calendar.callers[0])
	}

	// The event reached the prompt with its times written out, and is recorded
	// as the source the answer cited.
	prompt := ai.PromptText(h.provider.LastPrompt())
	for _, want := range []string{`event: "Dentist"`, "Wed 9 Sep 2026 16:00 UTC"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt is missing %q:\n%s", want, prompt)
		}
	}
	if len(turn.Assistant.Sources) != 1 {
		t.Fatalf("recorded %d sources, want the one event: %+v", len(turn.Assistant.Sources), turn.Assistant.Sources)
	}
	got := turn.Assistant.Sources[0]
	switch {
	case got.Type != SourceEvent:
		t.Fatalf("type = %q, want event", got.Type)
	case got.ID != event.ID:
		t.Fatalf("id = %v, want the event", got.ID)
	case got.Title != "Dentist":
		t.Fatalf("title = %q", got.Title)
	case !got.Cited:
		t.Fatalf("the answer cited %s but the record says otherwise: %+v", got.Label, got)
	case got.Similarity != nil:
		t.Fatalf("an event carries a similarity score: %+v", got)
	}
}

// The window is short on purpose: next month's event is not background to
// every question.
func TestAnEventBeyondTheWindowIsNotRetrieved(t *testing.T) {
	h := newHarness(t, nil)
	h.withEvent("Conference", today.AddDate(0, 1, 0), 24*time.Hour)
	h.withEvent("Standup", tomorrow.Add(9*time.Hour), time.Hour)

	turn, _, err := h.send(t, "What does my day look like?")
	if err != nil {
		t.Fatal(err)
	}
	var titles []string
	for _, s := range turn.Assistant.Sources {
		if s.Type == SourceEvent {
			titles = append(titles, s.Title)
		}
	}
	if strings.Join(titles, ",") != "Standup" {
		t.Fatalf("retrieved %v, want only what is inside the window", titles)
	}
}

// An all-day event says so rather than claiming to start at midnight, and a
// recurring one says that only the occurrence shown is recorded -- the repeats
// are not rows, and the model must not describe one as scheduled.
func TestEventSummariesSayWhatTheyAre(t *testing.T) {
	rule := "FREQ=WEEKLY;BYDAY=MO"
	where := "Room 2"
	for name, tc := range map[string]struct {
		event calendar.Event
		want  []string
	}{
		"an all-day event": {
			calendar.Event{Title: "Leave", StartTime: today, EndTime: today.AddDate(0, 0, 1), AllDay: true},
			[]string{"all day on Wed 9 Sep 2026"},
		},
		"an all-day event over several days": {
			calendar.Event{Title: "Leave", StartTime: today, EndTime: today.AddDate(0, 0, 3), AllDay: true},
			[]string{"all day from Wed 9 Sep 2026 to Fri 11 Sep 2026"},
		},
		"a timed event": {
			calendar.Event{Title: "Standup", StartTime: thisPM, EndTime: thisPM.Add(time.Hour), Location: &where},
			[]string{"starts Wed 9 Sep 2026 16:00 UTC", "ends Wed 9 Sep 2026 17:00 UTC", "at Room 2"},
		},
		"a repeating event": {
			calendar.Event{Title: "Standup", StartTime: thisPM, EndTime: thisPM.Add(time.Hour), RecurrenceRule: &rule},
			[]string{"repeats (only this occurrence is recorded)"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := eventSummary(tc.event)
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Fatalf("summary = %q, want it to contain %q", got, want)
				}
			}
		})
	}
}

// A calendar that errors fails the turn. Answering "you have nothing on" when
// the query failed is a false statement about the user's data, which is what
// the grounding rule exists to prevent.
func TestACalendarFailureFailsTheTurn(t *testing.T) {
	h := newHarness(t, nil)
	h.calendar.err = errors.New("connection refused")

	if _, _, err := h.send(t, "What does my day look like?"); err == nil {
		t.Fatal("the turn survived a failed calendar read")
	}
	if len(h.store.stored(h.conv.ID)) != 0 {
		t.Fatal("a failed turn was persisted")
	}
}

// No calendar wired is an assistant that retrieves what Phase 7 did.
func TestNoCalendarIsANoOp(t *testing.T) {
	h := newHarness(t, nil)
	h.svc.calendar = nil
	turn, _, err := h.send(t, "What does my day look like?")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range turn.Assistant.Sources {
		if s.Type == SourceEvent {
			t.Fatalf("an event was retrieved with no calendar wired: %+v", s)
		}
	}
}

// --- the tools ------------------------------------------------------------------

const proposeDentist = `{"tool": "create_calendar_event", "arguments": {"title": "Dentist", "start": "tomorrow at 3pm"}}`

// The approval rule, for the calendar: asking the assistant to schedule
// something proposes it and nothing else. The calendar service is never called.
func TestARequestToScheduleSomethingIsProposedNotExecuted(t *testing.T) {
	h := newToolHarness(t, routingReply(proposeDentist, func(string) string {
		return "I have prepared that event for you to approve."
	}))

	turn, sink, err := h.send(t, "Book the dentist for tomorrow at 3pm")
	if err != nil {
		t.Fatal(err)
	}
	if n := h.calendar.writes(); n != 0 {
		t.Fatalf("the chat turn wrote to the calendar %d times", n)
	}

	recorded := h.store.recordedActions()
	if len(recorded) != 1 {
		t.Fatalf("recorded %d actions, want 1", len(recorded))
	}
	a := recorded[0]
	switch {
	case a.Status != actions.StatusProposed || a.Permission != tools.Write || a.ToolName != tools.CreateCalendarEvent:
		t.Fatalf("recorded %+v, want a proposed create_calendar_event", a)
	case a.UserID != h.user || a.ConversationID == nil || *a.ConversationID != h.conv.ID:
		t.Fatalf("the proposal is not the caller's, in this conversation: %+v", a)
	case a.Result != nil:
		t.Fatalf("a proposal has a result: %+v", a)
	}

	var in map[string]any
	if err := json.Unmarshal(a.Input, &in); err != nil {
		t.Fatal(err)
	}
	// The tool harness's clock is 10 September 2026, so "tomorrow at 3pm" is
	// the 11th at 15:00 -- resolved by Go, stored canonically, and shown to the
	// user as the day and time they are approving.
	if in["start"] != "2026-09-11T15:00:00Z" || in["end"] != "2026-09-11T16:00:00Z" {
		t.Fatalf("stored input = %v", in)
	}
	if len(turn.Actions) != 1 || !strings.Contains(turn.Actions[0].Summary, "Fri 11 Sep 2026 15:00") {
		t.Fatalf("the user is asked to approve %q", turn.Actions[0].Summary)
	}
	if len(sink.Announced) != 1 {
		t.Fatalf("announced %d actions", len(sink.Announced))
	}

	// And the model was told it is a proposal, in the words that stop it
	// claiming the event is in the diary.
	block := h.answerPrompt()
	for _, want := range []string{"prepared a change that has not been made yet", "Never say that it is done"} {
		if !strings.Contains(block, want) {
			t.Fatalf("the ACTIONS section is missing %q:\n%s", want, block)
		}
	}
}

// A calendar read runs during the turn, and what it found becomes the turn's
// first sources -- marked with the tool that found them.
func TestACalendarSearchRunsAndItsEventsAreSources(t *testing.T) {
	h := newToolHarness(t, routingReply(
		`{"tool": "search_calendar", "arguments": {"start": "tomorrow"}}`,
		func(prompt string) string {
			if strings.Contains(prompt, "Standup") {
				return "You have the standup at 09:00 [S1]."
			}
			return "Nothing matched."
		}))
	// The tool harness's clock is 10 September 2026, 12:00 UTC.
	standup := calendar.Event{
		ID: uuid.New(), UserID: h.user, Title: "Standup",
		StartTime: time.Date(2026, 9, 11, 9, 0, 0, 0, time.UTC),
		EndTime:   time.Date(2026, 9, 11, 9, 15, 0, 0, time.UTC),
	}
	h.calendar.events.byUser[h.user] = append(h.calendar.events.byUser[h.user], standup)

	turn, _, err := h.send(t, "What's on my calendar tomorrow?")
	if err != nil {
		t.Fatal(err)
	}
	if n := h.calendar.writes(); n != 0 {
		t.Fatalf("a read wrote to the calendar %d times", n)
	}

	first := turn.Assistant.Sources[0]
	switch {
	case first.Type != SourceEvent || first.ID != standup.ID:
		t.Fatalf("the first source is %+v, want the event the search found", first)
	case first.Tool != tools.SearchCalendar:
		t.Fatalf("tool = %q, want search_calendar", first.Tool)
	case !first.Cited:
		t.Fatalf("the answer cited the event but the record says otherwise: %+v", first)
	}

	// The read is recorded as an executed action, with the window it used in
	// its result -- so the log says which days "nothing on" was about.
	recorded := h.store.recordedActions()
	if len(recorded) != 1 || recorded[0].Status != actions.StatusExecuted || recorded[0].Permission != tools.Read {
		t.Fatalf("recorded %+v, want one executed read", recorded)
	}
	if !strings.Contains(string(recorded[0].Result), "2026-09-11") {
		t.Fatalf("the recorded result does not say which window it read: %s", recorded[0].Result)
	}
}

// The turn never reaches a write tool, whatever the model says -- the same
// property Phase 7 pins for tasks, checked here for the tool that is new.
func TestTheChatTurnCannotReachTheCalendarWriteTool(t *testing.T) {
	h := newToolHarness(t, routingReply(proposeDentist, func(string) string { return "Prepared." }))
	if _, _, err := h.send(t, "Book the dentist for tomorrow at 3pm"); err != nil {
		t.Fatal(err)
	}
	a := h.store.recordedActions()[0]

	// The registry the chat turn holds has no ledger at all, so even the one
	// method that runs a write refuses.
	if _, err := h.reg.RunApproved(context.Background(), h.user, a.ID); !errors.Is(err, tools.ErrNoLedger) {
		t.Fatalf("RunApproved = %v, want ErrNoLedger", err)
	}
	if n := h.calendar.writes(); n != 0 {
		t.Fatalf("the calendar was written to %d times", n)
	}
}
