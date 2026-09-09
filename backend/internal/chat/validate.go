package chat

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/jashveer/lifeos/backend/internal/validate"
)

// ValidateTitle normalizes the optional title on conversation creation. An
// omitted or all-whitespace title is not an error: it means "name it later",
// and the first message supplies one.
func ValidateTitle(title string) (string, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return DefaultTitle, nil
	}
	if utf8.RuneCountInString(title) > validate.MaxTitleLen {
		return "", validate.Errors{{
			Field:   "title",
			Message: fmt.Sprintf("must be at most %d characters", validate.MaxTitleLen),
		}}
	}
	return title, nil
}

// ValidateMessage checks the text of an outgoing user message.
func ValidateMessage(content string) (string, error) {
	content = strings.TrimSpace(content)
	switch {
	case content == "":
		return "", validate.Errors{{Field: "content", Message: "is required"}}
	case utf8.RuneCountInString(content) > MaxMessageLen:
		return "", validate.Errors{{
			Field:   "content",
			Message: fmt.Sprintf("must be at most %d characters", MaxMessageLen),
		}}
	}
	return content, nil
}

// ValidateFilter clamps the list query; an unknown sort is a client error
// rather than a silent fallback.
func ValidateFilter(f Filter) (Filter, error) {
	var errs validate.Errors

	if f.Sort == "" {
		f.Sort = DefaultSort
	}
	if _, ok := Sorts[f.Sort]; !ok {
		errs = append(errs, validate.Error{Field: "sort", Message: "is not a sortable field"})
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
