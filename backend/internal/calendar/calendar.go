// Package calendar owns the event aggregate: what is on, when, and what it is
// for.
//
// It is the Phase 2 resource shape -- transport, service, repository, every
// query scoped to the owner -- with one difference that runs through all of it:
// an event is a half-open interval, and the only read the table has is "what
// overlaps this window". So there is no unbounded list. GET /calendar requires
// a range, the filter carries it, and the SQL is one index scan over
// (user_id, start_time). An "all my events" endpoint on a table that grows a
// row per meeting for years is a denial-of-service lever with no use case
// behind it; see docs/decisions.md.
//
// Recurrence is stored and not interpreted. RecurrenceRule is whatever string
// the client put there -- an RRULE, usually -- and nothing in this phase reads
// it: a recurring event is one row, and the repeats it describes do not exist
// as rows and are not returned by a range query. Computing them is a phase of
// its own (expansion, exceptions, time zones, the unbounded tail), and half of
// it would be worse than none.
package calendar

import (
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/optional"
)

// ErrNotFound covers "no such event", "that event is somebody else's", and a
// related task or goal that is not the caller's. One error for all of them is
// what keeps the API from confirming foreign ids.
var ErrNotFound = errors.New("event not found")

// Event mirrors a row of the calendar_events table.
//
// The interval is half-open: an event occupies [StartTime, EndTime), so the
// 09:00-10:00 meeting and the 10:00-11:00 one do not overlap. A zero-length
// event is allowed -- the CHECK is `>=` -- because a reminder at a moment is a
// thing people put in calendars.
type Event struct {
	ID          uuid.UUID
	UserID      uuid.UUID
	Title       string
	Description *string
	StartTime   time.Time
	EndTime     time.Time
	// AllDay says how to display the interval, not what it is. An all-day
	// event still carries both ends (midnight to midnight the next day, by
	// convention); the flag tells a client to show "Thursday" rather than
	// "00:00-00:00".
	AllDay         bool
	Location       *string
	RecurrenceRule *string
	RelatedTaskID  *uuid.UUID
	RelatedGoalID  *uuid.UUID
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// Duration is how long the event lasts.
func (e Event) Duration() time.Duration { return e.EndTime.Sub(e.StartTime) }

// CreateInput is a validated new event. Every field is already normalized by
// the time the repository sees it.
type CreateInput struct {
	Title          string
	Description    string
	StartTime      time.Time
	EndTime        time.Time
	AllDay         bool
	Location       string
	RecurrenceRule string
	RelatedTaskID  *uuid.UUID
	RelatedGoalID  *uuid.UUID
}

// UpdateInput is a PATCH body. An unset field is left alone; a field set to
// null clears the column.
type UpdateInput struct {
	Title          optional.Field[string]
	Description    optional.Field[string]
	StartTime      optional.Field[time.Time]
	EndTime        optional.Field[time.Time]
	AllDay         optional.Field[bool]
	Location       optional.Field[string]
	RecurrenceRule optional.Field[string]
	RelatedTaskID  optional.Field[uuid.UUID]
	RelatedGoalID  optional.Field[uuid.UUID]
}

// Patch is the normalized form of UpdateInput handed to the repository.
type Patch struct {
	Title          *string
	Description    optional.Field[string]
	StartTime      *time.Time
	EndTime        *time.Time
	AllDay         *bool
	Location       optional.Field[string]
	RecurrenceRule optional.Field[string]
	RelatedTaskID  optional.Field[uuid.UUID]
	RelatedGoalID  optional.Field[uuid.UUID]
}

// Empty reports whether the patch would touch no columns at all.
func (p Patch) Empty() bool {
	return p.Title == nil && !p.Description.Set && p.StartTime == nil && p.EndTime == nil &&
		p.AllDay == nil && !p.Location.Set && !p.RecurrenceRule.Set &&
		!p.RelatedTaskID.Set && !p.RelatedGoalID.Set
}

// MovesTheInterval reports whether the patch changes either end. It is what
// tells the service it has to re-check `end >= start` against the stored row
// rather than against the patch alone: moving only the start can invert an
// interval whose end the client never mentioned.
func (p Patch) MovesTheInterval() bool { return p.StartTime != nil || p.EndTime != nil }

// Filter is the query behind GET /calendar.
//
// Start and End are required and are the whole of the query: an event is
// returned when it overlaps [Start, End). Overlap rather than containment,
// because "what is on on Thursday" must include the conference that started on
// Tuesday -- a query that only matched events *starting* inside the window
// would hide exactly the events most worth seeing.
type Filter struct {
	Start time.Time
	End   time.Time
	// Query keeps the events whose title, description or location contains it,
	// case-insensitively and literally -- `%` and `_` are characters, not
	// wildcards. Same contract as tasks.Filter.Query.
	Query  string
	Sort   string
	Limit  int
	Offset int
}

// Sorts maps the public `sort` values onto SQL. As everywhere else, keeping it
// a fixed map is what makes ORDER BY safe to interpolate.
var Sorts = map[string]string{
	"start_time":  "start_time ASC",
	"-start_time": "start_time DESC",
	"end_time":    "end_time ASC",
	"-end_time":   "end_time DESC",
	"created_at":  "created_at ASC",
	"-created_at": "created_at DESC",
	"updated_at":  "updated_at ASC",
	"-updated_at": "updated_at DESC",
	"title":       "lower(title) ASC",
	"-title":      "lower(title) DESC",
}

// DefaultSort is chronological. Unlike every other resource in this codebase,
// the natural order of a calendar is not "newest first": a list of a week's
// events reads forwards.
const DefaultSort = "start_time"

// Paging bounds. A range query is already bounded by its window, but a window
// can be a decade, so the cap is not optional.
const (
	DefaultLimit = 100
	MaxLimit     = 500
)

// MaxWindow is the widest range one query may ask for. Ten years is far past
// any calendar view and still small enough that the worst case is one person's
// events rather than one person's decade of them, repeatedly.
const MaxWindow = 10 * 366 * 24 * time.Hour

// Field limits particular to this module. The shared ones (title, description,
// query) live in internal/validate.
const (
	// MaxLocationLen fits a postal address or a meeting-room URL.
	MaxLocationLen = 500
	// MaxRecurrenceRuleLen fits an RRULE with a long EXDATE list. The column is
	// opaque to this phase, so the limit is the only thing said about it.
	MaxRecurrenceRuleLen = 2_000
)
