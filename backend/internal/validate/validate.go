// Package validate holds the field rules the Lunex resource modules share.
//
// Each helper returns nil when the field is fine, so a caller can collect every
// problem in one pass and answer with a complete "fields" array instead of
// making the client fix one thing per round trip — the same contract
// internal/auth established in Phase 1.
package validate

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/jashveer/lifeos/backend/internal/auth"
	"github.com/jashveer/lifeos/backend/internal/optional"
)

type (
	// Error is a single rejected field.
	Error = auth.ValidationError
	// Errors is the collection returned to the transport layer.
	Errors = auth.ValidationErrors
)

// Field length limits. They are generous enough not to be felt in normal use
// and small enough that a single row cannot be used as blob storage.
const (
	MaxTitleLen       = 500
	MaxDescriptionLen = 10_000
	MaxContentLen     = 40_000
	MaxCategoryLen    = 100
	MaxTagLen         = 50
	MaxTags           = 25
	// A year of minutes: past that, the field is being misused.
	MaxEffortMinutes = 60 * 24 * 365
)

// Title trims, then requires a non-empty value within the length limit. It
// returns the trimmed value so the caller stores what it validated.
func Title(field, v string) (string, *Error) {
	v = strings.TrimSpace(v)
	switch {
	case v == "":
		return "", &Error{Field: field, Message: "is required"}
	case utf8.RuneCountInString(v) > MaxTitleLen:
		return "", &Error{Field: field, Message: fmt.Sprintf("must be at most %d characters", MaxTitleLen)}
	}
	return v, nil
}

// MaxLen checks an optional free-text field.
func MaxLen(field, v string, max int) *Error {
	if utf8.RuneCountInString(v) > max {
		return &Error{Field: field, Message: fmt.Sprintf("must be at most %d characters", max)}
	}
	return nil
}

// OneOf checks a closed set. The allowed values are listed in the message
// because they are part of the public contract, not a secret.
func OneOf(field, v string, allowed []string) *Error {
	for _, a := range allowed {
		if v == a {
			return nil
		}
	}
	return &Error{Field: field, Message: "must be one of: " + strings.Join(allowed, ", ")}
}

// Tags trims, drops blanks and de-duplicates while preserving the order the
// client sent. Storing a normalized list means a filter on "work" is not
// defeated by a stray " work".
func Tags(field string, tags []string) ([]string, *Error) {
	out := make([]string, 0, len(tags))
	seen := make(map[string]struct{}, len(tags))
	for _, t := range tags {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		if utf8.RuneCountInString(t) > MaxTagLen {
			return nil, &Error{Field: field, Message: fmt.Sprintf("each tag must be at most %d characters", MaxTagLen)}
		}
		if _, dup := seen[t]; dup {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	if len(out) > MaxTags {
		return nil, &Error{Field: field, Message: fmt.Sprintf("must have at most %d tags", MaxTags)}
	}
	return out, nil
}

// Minutes checks an effort estimate.
func Minutes(field string, v int) *Error {
	switch {
	case v < 0:
		return &Error{Field: field, Message: "must not be negative"}
	case v > MaxEffortMinutes:
		return &Error{Field: field, Message: fmt.Sprintf("must be at most %d minutes", MaxEffortMinutes)}
	}
	return nil
}

// Optional trims a free-text field and returns nil when nothing is left, so an
// empty string and an omitted value land in the database the same way: NULL.
func Optional(v string) *string {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	return &v
}

// TrimmedField turns a supplied free-text value into a patch instruction: an
// all-whitespace value clears the column rather than storing "   ".
func TrimmedField(v string) optional.Field[string] {
	if s := strings.TrimSpace(v); s != "" {
		return optional.Of(s)
	}
	return optional.Null[string]()
}
