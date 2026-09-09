// Package tasks owns the task aggregate: the rows themselves, their subtask
// parentage and their dependency edges.
package tasks

import (
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/optional"
)

var (
	// ErrNotFound is returned when no task matches the lookup *for that user*.
	// A task belonging to somebody else is indistinguishable from one that does
	// not exist, which is what keeps the API from confirming foreign ids.
	ErrNotFound = errors.New("task not found")
	// ErrDependencyCycle is returned when an edge would make a task
	// transitively depend on itself.
	ErrDependencyCycle = errors.New("dependency would create a cycle")
)

// The closed sets behind the priority and status columns. They are enforced
// here rather than as CHECK constraints so adding a value later is a code
// change, not a migration — see docs/decisions.md.
var (
	Priorities = []string{"low", "medium", "high"}
	Statuses   = []string{"pending", "in_progress", "completed"}
)

const (
	DefaultPriority = "medium"
	DefaultStatus   = "pending"
)

// Task mirrors a row of the tasks table, plus the dependency ids loaded
// alongside it.
type Task struct {
	ID                     uuid.UUID
	UserID                 uuid.UUID
	Title                  string
	Description            *string
	Priority               string
	Status                 string
	Category               *string
	Tags                   []string
	Deadline               *time.Time
	ParentTaskID           *uuid.UUID
	EstimatedEffortMinutes *int
	ActualEffortMinutes    *int
	DependsOn              []uuid.UUID
	CreatedAt              time.Time
	UpdatedAt              time.Time
}

// CreateInput is a validated new task. Every field is already normalized by the
// time the repository sees it.
type CreateInput struct {
	Title                  string
	Description            string
	Priority               string
	Status                 string
	Category               string
	Tags                   []string
	Deadline               *time.Time
	ParentTaskID           *uuid.UUID
	EstimatedEffortMinutes *int
	ActualEffortMinutes    *int
}

// UpdateInput is a PATCH body. An unset field is left alone; a field set to
// null clears the column.
type UpdateInput struct {
	Title                  optional.Field[string]
	Description            optional.Field[string]
	Priority               optional.Field[string]
	Status                 optional.Field[string]
	Category               optional.Field[string]
	Tags                   optional.Field[[]string]
	Deadline               optional.Field[time.Time]
	ParentTaskID           optional.Field[uuid.UUID]
	EstimatedEffortMinutes optional.Field[int]
	ActualEffortMinutes    optional.Field[int]
}

// Patch is the normalized form of UpdateInput handed to the repository: the
// column-level instructions, with validation already applied.
type Patch struct {
	Title                  *string
	Description            optional.Field[string]
	Priority               *string
	Status                 *string
	Category               optional.Field[string]
	Tags                   *[]string
	Deadline               optional.Field[time.Time]
	ParentTaskID           optional.Field[uuid.UUID]
	EstimatedEffortMinutes optional.Field[int]
	ActualEffortMinutes    optional.Field[int]
}

// Empty reports whether the patch would touch no columns at all.
func (p Patch) Empty() bool {
	return p.Title == nil && !p.Description.Set && p.Priority == nil && p.Status == nil &&
		!p.Category.Set && p.Tags == nil && !p.Deadline.Set && !p.ParentTaskID.Set &&
		!p.EstimatedEffortMinutes.Set && !p.ActualEffortMinutes.Set
}

// Filter is the query behind GET /tasks.
type Filter struct {
	Status   string
	Category string
	Tag      string
	Sort     string
	Limit    int
	Offset   int
}

// Sorts maps the public `sort` values onto SQL. Keeping it a fixed map is what
// makes ORDER BY safe to interpolate: nothing the client sends reaches the
// query, only a value looked up here.
//
// NULLS LAST on the date sorts keeps undated rows out of the way of the ones a
// user asked to see by date.
var Sorts = map[string]string{
	"created_at":  "created_at ASC",
	"-created_at": "created_at DESC",
	"updated_at":  "updated_at ASC",
	"-updated_at": "updated_at DESC",
	"deadline":    "deadline ASC NULLS LAST",
	"-deadline":   "deadline DESC NULLS LAST",
	"title":       "lower(title) ASC",
	"-title":      "lower(title) DESC",
	"priority":    "CASE priority WHEN 'high' THEN 3 WHEN 'medium' THEN 2 WHEN 'low' THEN 1 ELSE 0 END ASC",
	"-priority":   "CASE priority WHEN 'high' THEN 3 WHEN 'medium' THEN 2 WHEN 'low' THEN 1 ELSE 0 END DESC",
}

// DefaultSort is newest first, which is what an unsorted list of anything
// user-created should look like.
const DefaultSort = "-created_at"

// Paging bounds. An unbounded list endpoint is a denial-of-service lever on a
// table that grows without limit, so the cap is not optional.
const (
	DefaultLimit = 50
	MaxLimit     = 200
)
