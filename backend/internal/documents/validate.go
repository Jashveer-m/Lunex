package documents

import (
	"fmt"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/jashveer/lifeos/backend/internal/validate"
)

// MaxUploadBytes is the default ceiling on an uploaded file. It is checked
// before the bytes are read, not after, so an oversized upload costs a rejected
// request rather than a parsed one. config can override it.
const MaxUploadBytes int64 = 10 << 20 // 10 MiB

// ValidateFilename normalizes the client-supplied name.
//
// Browsers and some clients send a full path in the multipart header; only the
// base name is kept. The value is stored and echoed back, never used to open a
// file, but stripping the path also removes the whole class of "../" mistakes
// a later phase would inherit when object storage arrives.
func ValidateFilename(name string) (string, *validate.Error) {
	// Windows clients send backslashes, which filepath.Base leaves alone on
	// Unix, so both separators are normalized first.
	name = strings.ReplaceAll(name, "\\", "/")
	name = strings.TrimSpace(filepath.Base(strings.TrimSpace(name)))

	switch {
	case name == "" || name == "." || name == "/":
		return "", &validate.Error{Field: "file", Message: "must have a filename"}
	case utf8.RuneCountInString(name) > MaxFilenameLen:
		return "", &validate.Error{Field: "file", Message: fmt.Sprintf("filename must be at most %d characters", MaxFilenameLen)}
	case strings.ContainsRune(name, 0):
		return "", &validate.Error{Field: "file", Message: "filename contains an invalid character"}
	}
	return name, nil
}

// ValidateSearch checks and defaults a retrieval request. It is exported
// because Phase 6 calls Service.Search directly and gets the same defaults the
// HTTP endpoint does.
func ValidateSearch(q SearchQuery) (SearchQuery, error) {
	var errs validate.Errors

	q.Query = strings.TrimSpace(q.Query)
	switch {
	case q.Query == "":
		errs = append(errs, validate.Error{Field: "query", Message: "is required"})
	case utf8.RuneCountInString(q.Query) > MaxQueryLen:
		errs = append(errs, validate.Error{Field: "query", Message: fmt.Sprintf("must be at most %d characters", MaxQueryLen)})
	}

	switch {
	case q.Limit < 0:
		errs = append(errs, validate.Error{Field: "limit", Message: "must not be negative"})
	case q.Limit == 0:
		q.Limit = DefaultSearchLimit
	case q.Limit > MaxSearchLimit:
		q.Limit = MaxSearchLimit
	}

	// Cosine similarity runs from -1 to 1. A negative floor is allowed but
	// meaningless; anything outside the range is a client mistake.
	if q.MinSimilarity < -1 || q.MinSimilarity > 1 {
		errs = append(errs, validate.Error{Field: "min_similarity", Message: "must be between -1 and 1"})
	}
	if len(q.DocumentIDs) > MaxLimit {
		errs = append(errs, validate.Error{Field: "document_ids", Message: fmt.Sprintf("must have at most %d ids", MaxLimit)})
	}

	return q, errs.OrNil()
}

// ValidateFilter clamps the list query; an unknown sort or status is a client
// error rather than a silent fallback.
func ValidateFilter(f Filter) (Filter, error) {
	var errs validate.Errors

	if f.Sort == "" {
		f.Sort = DefaultSort
	}
	if _, ok := Sorts[f.Sort]; !ok {
		errs = append(errs, validate.Error{Field: "sort", Message: "is not a sortable field"})
	}
	if f.Status != "" {
		if e := validate.OneOf("status", f.Status, Statuses); e != nil {
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
