package goals

import (
	"strings"

	"github.com/jashveer/lifeos/backend/internal/optional"
	"github.com/jashveer/lifeos/backend/internal/validate"
)

// ValidateCreate checks and normalizes a new goal, reporting every problem at
// once rather than failing on the first.
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
	// type has no column default: unlike priority or status there is no
	// sensible fallback, so it is required.
	if e := validate.OneOf("type", in.Type, Types); e != nil {
		errs = append(errs, *e)
	}
	if in.Status == "" {
		in.Status = DefaultStatus
	}
	if e := validate.OneOf("status", in.Status, Statuses); e != nil {
		errs = append(errs, *e)
	}

	in.Description = strings.TrimSpace(in.Description)
	return in, errs.OrNil()
}

// ValidatePatch turns a PATCH body into column instructions.
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

	if v, ok := in.Description.Get(); ok {
		if e := validate.MaxLen("description", v, validate.MaxDescriptionLen); e != nil {
			errs = append(errs, *e)
		}
		p.Description = validate.TrimmedField(v)
	} else if in.Description.Cleared() {
		p.Description = optional.Null[string]()
	}

	if v, ok := in.Type.Get(); ok {
		if e := validate.OneOf("type", v, Types); e != nil {
			errs = append(errs, *e)
		} else {
			p.Type = &v
		}
	} else if in.Type.Cleared() {
		errs = append(errs, validate.Error{Field: "type", Message: "must be one of: " + strings.Join(Types, ", ")})
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

	p.Deadline = in.Deadline
	return p, errs.OrNil()
}

func ValidateMilestone(in MilestoneInput) (MilestoneInput, error) {
	var errs validate.Errors
	title, err := validate.Title("title", in.Title)
	if err != nil {
		errs = append(errs, *err)
	}
	in.Title = title
	return in, errs.OrNil()
}

func ValidateMilestonePatch(in MilestoneUpdateInput) (MilestonePatch, error) {
	var errs validate.Errors
	var p MilestonePatch

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

	if v, ok := in.Completed.Get(); ok {
		p.Completed = &v
	} else if in.Completed.Cleared() {
		errs = append(errs, validate.Error{Field: "completed", Message: "must be true or false"})
	}

	p.TargetDate = in.TargetDate
	return p, errs.OrNil()
}

// ValidateFilter clamps the list query. An unknown sort is rejected rather than
// silently ignored, so a typo surfaces instead of returning the wrong order.
func ValidateFilter(f Filter) (Filter, error) {
	var errs validate.Errors

	if f.Status != "" {
		if e := validate.OneOf("status", f.Status, Statuses); e != nil {
			errs = append(errs, *e)
		}
	}
	if f.Type != "" {
		if e := validate.OneOf("type", f.Type, Types); e != nil {
			errs = append(errs, *e)
		}
	}
	f.Query = strings.TrimSpace(f.Query)
	if e := validate.MaxLen("q", f.Query, validate.MaxQueryLen); e != nil {
		errs = append(errs, *e)
	}
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
