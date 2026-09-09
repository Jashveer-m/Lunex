package notes

import (
	"github.com/jashveer/lifeos/backend/internal/validate"
)

// ValidateCreate checks and normalizes a new note.
func ValidateCreate(in CreateInput) (CreateInput, error) {
	var errs validate.Errors

	title, err := validate.Title("title", in.Title)
	if err != nil {
		errs = append(errs, *err)
	}
	in.Title = title

	if e := validate.MaxLen("content", in.Content, validate.MaxContentLen); e != nil {
		errs = append(errs, *e)
	}

	tags, tagErr := validate.Tags("tags", in.Tags)
	if tagErr != nil {
		errs = append(errs, *tagErr)
	}
	in.Tags = tags

	return in, errs.OrNil()
}

// ValidatePatch turns a PATCH body into column instructions. content and tags
// are NOT NULL columns, so an explicit null means "empty", not "NULL".
func ValidatePatch(in UpdateInput) (Patch, error) {
	var errs validate.Errors
	var p Patch

	if v, ok := in.Title.Get(); ok {
		title, err := validate.Title("title", v)
		if err != nil {
			errs = append(errs, *err)
		} else {
			p.Title = &title
		}
	} else if in.Title.Cleared() {
		errs = append(errs, validate.Error{Field: "title", Message: "is required"})
	}

	if v, ok := in.Content.Get(); ok {
		if e := validate.MaxLen("content", v, validate.MaxContentLen); e != nil {
			errs = append(errs, *e)
		} else {
			p.Content = &v
		}
	} else if in.Content.Cleared() {
		empty := ""
		p.Content = &empty
	}

	if v, ok := in.Tags.Get(); ok {
		tags, err := validate.Tags("tags", v)
		if err != nil {
			errs = append(errs, *err)
		} else {
			p.Tags = &tags
		}
	} else if in.Tags.Cleared() {
		empty := []string{}
		p.Tags = &empty
	}

	return p, errs.OrNil()
}

// ValidateFilter clamps the list query; an unknown sort is a client error.
func ValidateFilter(f Filter) (Filter, error) {
	var errs validate.Errors

	if f.Sort == "" {
		f.Sort = DefaultSort
	}
	if _, ok := Sorts[f.Sort]; !ok {
		errs = append(errs, validate.Error{Field: "sort", Message: "is not a sortable field"})
	}
	if e := validate.MaxLen("tag", f.Tag, validate.MaxTagLen); e != nil {
		errs = append(errs, *e)
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
