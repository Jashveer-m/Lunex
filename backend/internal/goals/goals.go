// Package goals owns the goal aggregate and the milestones hanging off it.
package goals

import (
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/optional"
)

var (
	// ErrNotFound covers both "no such goal" and "that goal is somebody
	// else's". Keeping them one error is what stops the API confirming
	// foreign ids.
	ErrNotFound = errors.New("goal not found")
	// ErrMilestoneNotFound is the same idea one level down: the milestone
	// does not exist, or its goal is not the caller's.
	ErrMilestoneNotFound = errors.New("milestone not found")
)

// The closed sets behind the type and status columns; enforced in the service
// rather than as CHECK constraints — see docs/decisions.md.
var (
	Types    = []string{"short_term", "long_term", "career", "education", "financial", "personal", "project"}
	Statuses = []string{"active", "completed", "abandoned"}
)

const DefaultStatus = "active"

// Goal mirrors a row of the goals table with its milestones attached.
type Goal struct {
	ID          uuid.UUID
	UserID      uuid.UUID
	Title       string
	Description *string
	Type        string
	Status      string
	Deadline    *time.Time
	Milestones  []Milestone
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Milestone mirrors a row of goal_milestones. It has no updated_at column, so
// unlike the other Phase 2 tables it carries no set_updated_at trigger.
type Milestone struct {
	ID         uuid.UUID
	GoalID     uuid.UUID
	Title      string
	TargetDate *time.Time
	Completed  bool
	CreatedAt  time.Time
}

type CreateInput struct {
	Title       string
	Description string
	Type        string
	Status      string
	Deadline    *time.Time
}

type UpdateInput struct {
	Title       optional.Field[string]
	Description optional.Field[string]
	Type        optional.Field[string]
	Status      optional.Field[string]
	Deadline    optional.Field[time.Time]
}

// Patch is the validated, column-level form of UpdateInput.
type Patch struct {
	Title       *string
	Description optional.Field[string]
	Type        *string
	Status      *string
	Deadline    optional.Field[time.Time]
}

func (p Patch) Empty() bool {
	return p.Title == nil && !p.Description.Set && p.Type == nil && p.Status == nil && !p.Deadline.Set
}

type MilestoneInput struct {
	Title      string
	TargetDate *time.Time
}

type MilestoneUpdateInput struct {
	Title      optional.Field[string]
	TargetDate optional.Field[time.Time]
	Completed  optional.Field[bool]
}

// MilestonePatch is the validated form of MilestoneUpdateInput.
type MilestonePatch struct {
	Title      *string
	TargetDate optional.Field[time.Time]
	Completed  *bool
}

func (p MilestonePatch) Empty() bool {
	return p.Title == nil && !p.TargetDate.Set && p.Completed == nil
}

// Filter is the query behind GET /goals.
type Filter struct {
	Status string
	Type   string
	// Query keeps the goals whose title or description contains it,
	// case-insensitively and literally; see tasks.Filter.Query.
	Query  string
	Sort   string
	Limit  int
	Offset int
}

// Sorts is the fixed allow-list of ORDER BY fragments; nothing from the request
// ever reaches the query text.
var Sorts = map[string]string{
	"created_at":  "created_at ASC",
	"-created_at": "created_at DESC",
	"updated_at":  "updated_at ASC",
	"-updated_at": "updated_at DESC",
	"deadline":    "deadline ASC NULLS LAST",
	"-deadline":   "deadline DESC NULLS LAST",
	"title":       "lower(title) ASC",
	"-title":      "lower(title) DESC",
}

const DefaultSort = "-created_at"

const (
	DefaultLimit = 50
	MaxLimit     = 200
	// A goal with more milestones than this is a project, not a goal.
	MaxMilestones = 100
)
