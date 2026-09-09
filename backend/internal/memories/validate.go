package memories

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/jashveer/lifeos/backend/internal/validate"
)

// ValidatePatch turns a PATCH body into column instructions.
//
// Both columns are NOT NULL, so an explicit null is a client error rather than
// a clear: a memory with no text is not a memory, and a memory that is neither
// enabled nor disabled does not exist.
func ValidatePatch(in UpdateInput) (Patch, error) {
	var errs validate.Errors
	var p Patch

	if v, ok := in.Content.Get(); ok {
		content := strings.TrimSpace(v)
		switch {
		case content == "":
			errs = append(errs, validate.Error{Field: "content", Message: "is required"})
		case utf8.RuneCountInString(content) > MaxContentLen:
			errs = append(errs, validate.Error{
				Field:   "content",
				Message: fmt.Sprintf("must be at most %d characters", MaxContentLen),
			})
		default:
			p.Content = &content
		}
	} else if in.Content.Cleared() {
		errs = append(errs, validate.Error{Field: "content", Message: "is required"})
	}

	if v, ok := in.Enabled.Get(); ok {
		p.Enabled = &v
	} else if in.Enabled.Cleared() {
		errs = append(errs, validate.Error{Field: "enabled", Message: "must be true or false"})
	}

	return p, errs.OrNil()
}

// ValidateFilter clamps the list query; an unknown sort or type is a client
// error rather than a silent fallback.
func ValidateFilter(f Filter) (Filter, error) {
	var errs validate.Errors

	if f.Sort == "" {
		f.Sort = DefaultSort
	}
	if _, ok := Sorts[f.Sort]; !ok {
		errs = append(errs, validate.Error{Field: "sort", Message: "is not a sortable field"})
	}
	if f.Type != "" {
		if e := validate.OneOf("type", f.Type, Types); e != nil {
			errs = append(errs, *e)
		}
	}
	switch {
	case f.Limit < 0:
		errs = append(errs, validate.Error{Field: "limit", Message: "must not be negative"})
	case f.Limit == 0:
		f.Limit = DefaultLimit
	case f.Limit > MaxLimit:
		f.Limit = MaxLimit
	}
	if f.Offset < 0 {
		errs = append(errs, validate.Error{Field: "offset", Message: "must not be negative"})
	}
	return f, errs.OrNil()
}

// ValidateSearch checks and fills in a retrieval request. It mirrors
// documents.ValidateSearch: the caller that only sets Query gets the defaults,
// and the floor is left where the caller put it -- including at zero, which is
// how a caller says "return the nearest k whatever they are".
func ValidateSearch(q SearchQuery) (SearchQuery, error) {
	var errs validate.Errors

	q.Query = strings.TrimSpace(q.Query)
	switch {
	case q.Query == "":
		errs = append(errs, validate.Error{Field: "query", Message: "is required"})
	case utf8.RuneCountInString(q.Query) > MaxQueryLen:
		errs = append(errs, validate.Error{
			Field:   "query",
			Message: fmt.Sprintf("must be at most %d characters", MaxQueryLen),
		})
	}
	switch {
	case q.Limit < 0:
		errs = append(errs, validate.Error{Field: "limit", Message: "must not be negative"})
	case q.Limit == 0:
		q.Limit = DefaultSearchLimit
	case q.Limit > MaxSearchLimit:
		q.Limit = MaxSearchLimit
	}
	if q.MinSimilarity < 0 || q.MinSimilarity > 1 {
		errs = append(errs, validate.Error{Field: "min_similarity", Message: "must be between 0 and 1"})
	}
	return q, errs.OrNil()
}
