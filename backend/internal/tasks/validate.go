package tasks

import (
	"strings"

	"github.com/jashveer/lifeos/backend/internal/optional"
	"github.com/jashveer/lifeos/backend/internal/validate"
)

// ValidateCreate checks and normalizes a new task, reporting every problem at
// once. The returned input is what should be persisted.
func ValidateCreate(in CreateInput) (CreateInput, error) {
	var errs validate.Errors

	title, err := validate.Title("title", in.Title)
	if err != nil {
		errs = append(errs, *err)
	}
	in.Title = title

	if e := validate.MaxLen("description", in.Description, validate.MaxDescriptionLen); e != nil {
		errs = append(errs, *e)
	}
	if e := validate.MaxLen("category", in.Category, validate.MaxCategoryLen); e != nil {
		errs = append(errs, *e)
	}

	// Defaults match the column defaults, so an omitted field and the database
	// fallback can never disagree.
	if in.Priority == "" {
		in.Priority = DefaultPriority
	}
	if e := validate.OneOf("priority", in.Priority, Priorities); e != nil {
		errs = append(errs, *e)
	}
	if in.Status == "" {
		in.Status = DefaultStatus
	}
	if e := validate.OneOf("status", in.Status, Statuses); e != nil {
		errs = append(errs, *e)
	}

	tags, tagErr := validate.Tags("tags", in.Tags)
	if tagErr != nil {
		errs = append(errs, *tagErr)
	}
	in.Tags = tags

	if in.EstimatedEffortMinutes != nil {
		if e := validate.Minutes("estimated_effort_minutes", *in.EstimatedEffortMinutes); e != nil {
			errs = append(errs, *e)
		}
	}
	if in.ActualEffortMinutes != nil {
		if e := validate.Minutes("actual_effort_minutes", *in.ActualEffortMinutes); e != nil {
			errs = append(errs, *e)
		}
	}

	in.Description = strings.TrimSpace(in.Description)
	in.Category = strings.TrimSpace(in.Category)
	return in, errs.OrNil()
}

// ValidatePatch turns a PATCH body into the column instructions the repository
// applies. Fields the client did not mention produce no instruction at all.
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
		// title is NOT NULL; clearing it is a validation failure, not a 500.
		errs = append(errs, validate.Error{Field: "title", Message: "is required"})
	}

	if v, ok := in.Description.Get(); ok {
		if e := validate.MaxLen("description", v, validate.MaxDescriptionLen); e != nil {
			errs = append(errs, *e)
		}
		p.Description = validate.TrimmedField(v)
	} else if in.Description.Cleared() {
		p.Description = optional.Null[string]()
	}

	if v, ok := in.Priority.Get(); ok {
		if e := validate.OneOf("priority", v, Priorities); e != nil {
			errs = append(errs, *e)
		} else {
			p.Priority = &v
		}
	} else if in.Priority.Cleared() {
		errs = append(errs, validate.Error{Field: "priority", Message: "must be one of: " + strings.Join(Priorities, ", ")})
	}

	if v, ok := in.Status.Get(); ok {
		if e := validate.OneOf("status", v, Statuses); e != nil {
			errs = append(errs, *e)
		} else {
			p.Status = &v
		}
	} else if in.Status.Cleared() {
		errs = append(errs, validate.Error{Field: "status", Message: "must be one of: " + strings.Join(Statuses, ", ")})
	}

	if v, ok := in.Category.Get(); ok {
		if e := validate.MaxLen("category", v, validate.MaxCategoryLen); e != nil {
			errs = append(errs, *e)
		}
		p.Category = validate.TrimmedField(v)
	} else if in.Category.Cleared() {
		p.Category = optional.Null[string]()
	}

	if v, ok := in.Tags.Get(); ok {
		tags, err := validate.Tags("tags", v)
		if err != nil {
			errs = append(errs, *err)
		} else {
			p.Tags = &tags
		}
	} else if in.Tags.Cleared() {
		// tags is NOT NULL DEFAULT '{}'; null means "no tags", not NULL.
		empty := []string{}
		p.Tags = &empty
	}

	p.Deadline = in.Deadline
	p.ParentTaskID = in.ParentTaskID

	p.EstimatedEffortMinutes = in.EstimatedEffortMinutes
	if v, ok := in.EstimatedEffortMinutes.Get(); ok {
		if e := validate.Minutes("estimated_effort_minutes", v); e != nil {
			errs = append(errs, *e)
		}
	}
	p.ActualEffortMinutes = in.ActualEffortMinutes
	if v, ok := in.ActualEffortMinutes.Get(); ok {
		if e := validate.Minutes("actual_effort_minutes", v); e != nil {
			errs = append(errs, *e)
		}
	}

	return p, errs.OrNil()
}

// ValidateFilter clamps the list query. An unknown sort is a client error
// rather than a silent fallback: silently ignoring it hides the typo.
func ValidateFilter(f Filter) (Filter, error) {
	var errs validate.Errors

	if f.Status != "" {
		if e := validate.OneOf("status", f.Status, Statuses); e != nil {
			errs = append(errs, *e)
		}
	}
	if f.Sort == "" {
		f.Sort = DefaultSort
	}
	if _, ok := Sorts[f.Sort]; !ok {
		errs = append(errs, validate.Error{Field: "sort", Message: "is not a sortable field"})
	}
	if e := validate.MaxLen("category", f.Category, validate.MaxCategoryLen); e != nil {
		errs = append(errs, *e)
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
