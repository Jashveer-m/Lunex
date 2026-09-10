// Package notes owns the note aggregate: title, free-text content and tags.
package notes

import (
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/optional"
)

// ErrNotFound covers both "no such note" and "that note is somebody else's",
// which is what keeps the API from confirming foreign ids.
var ErrNotFound = errors.New("note not found")

// Note mirrors a row of the notes table.
type Note struct {
	ID        uuid.UUID
	UserID    uuid.UUID
	Title     string
	Content   string
	Tags      []string
	CreatedAt time.Time
	UpdatedAt time.Time
}

type CreateInput struct {
	Title   string
	Content string
	Tags    []string
}

type UpdateInput struct {
	Title   optional.Field[string]
	Content optional.Field[string]
	Tags    optional.Field[[]string]
}

// Patch is the validated, column-level form of UpdateInput. Every notes column
// a client can set is NOT NULL, so a plain pointer says all there is to say.
type Patch struct {
	Title   *string
	Content *string
	Tags    *[]string
}

func (p Patch) Empty() bool { return p.Title == nil && p.Content == nil && p.Tags == nil }

// Filter is the query behind GET /notes.
type Filter struct {
	Tag string
	// Query keeps the notes whose title or content contains it,
	// case-insensitively and literally; see tasks.Filter.Query.
	Query  string
	Sort   string
	Limit  int
	Offset int
}

// Sorts is the fixed allow-list of ORDER BY fragments.
var Sorts = map[string]string{
	"created_at":  "created_at ASC",
	"-created_at": "created_at DESC",
	"updated_at":  "updated_at ASC",
	"-updated_at": "updated_at DESC",
	"title":       "lower(title) ASC",
	"-title":      "lower(title) DESC",
}

const DefaultSort = "-created_at"

const (
	DefaultLimit = 50
	MaxLimit     = 200
)
