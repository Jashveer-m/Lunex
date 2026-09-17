package calendar

import (
	"fmt"
	"strings"
	"time"

	"github.com/jashveer/lifeos/backend/internal/optional"
	"github.com/jashveer/lifeos/backend/internal/validate"
)

// ValidateCreate checks and normalizes a new event, reporting every problem at
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
	if e := validate.MaxLen("location", in.Location, MaxLocationLen); e != nil {
		errs = append(errs, *e)
	}
	if e := validate.MaxLen("recurrence_rule", in.RecurrenceRule, MaxRecurrenceRuleLen); e != nil {
		errs = append(errs, *e)
	}

	switch {
	case in.StartTime.IsZero():
		errs = append(errs, validate.Error{Field: "start_time", Message: "is required"})
	case in.EndTime.IsZero():
		// An event with a start and no end is the one shape worth defaulting:
		// a client that says when something begins and not when it ends means
		// it is an instant, not an error. It is still recorded as an interval.
		in.EndTime = in.StartTime
	}
	if e := interval(in.StartTime, in.EndTime); e != nil {
		errs = append(errs, *e)
	}

	in.Description = strings.TrimSpace(in.Description)
	in.Location = strings.TrimSpace(in.Location)
	in.RecurrenceRule = strings.TrimSpace(in.RecurrenceRule)
	return in, errs.OrNil()
}

// ValidatePatch turns a PATCH body into the column instructions the repository
// applies. Fields the client did not mention produce no instruction at all.
//
// It cannot check `end >= start` on its own: a patch that moves only the start
// has to be compared with the stored row, which is the service's job. What it
// does check is a patch that carries both ends, so the obvious case is a 400
// rather than a round trip to the database.
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

	if v, ok := in.Location.Get(); ok {
		if e := validate.MaxLen("location", v, MaxLocationLen); e != nil {
			errs = append(errs, *e)
		}
		p.Location = validate.TrimmedField(v)
	} else if in.Location.Cleared() {
		p.Location = optional.Null[string]()
	}

	if v, ok := in.RecurrenceRule.Get(); ok {
		if e := validate.MaxLen("recurrence_rule", v, MaxRecurrenceRuleLen); e != nil {
			errs = append(errs, *e)
		}
		p.RecurrenceRule = validate.TrimmedField(v)
	} else if in.RecurrenceRule.Cleared() {
		p.RecurrenceRule = optional.Null[string]()
	}

	// Both ends are NOT NULL, so null is a validation failure rather than a
	// column being cleared.
	if v, ok := in.StartTime.Get(); ok {
		p.StartTime = &v
	} else if in.StartTime.Cleared() {
		errs = append(errs, validate.Error{Field: "start_time", Message: "is required"})
	}
	if v, ok := in.EndTime.Get(); ok {
		p.EndTime = &v
	} else if in.EndTime.Cleared() {
		errs = append(errs, validate.Error{Field: "end_time", Message: "is required"})
	}
	if p.StartTime != nil && p.EndTime != nil {
		if e := interval(*p.StartTime, *p.EndTime); e != nil {
			errs = append(errs, *e)
		}
	}

	if v, ok := in.AllDay.Get(); ok {
		p.AllDay = &v
	} else if in.AllDay.Cleared() {
		// all_day is NOT NULL DEFAULT false; null means "not an all-day
		// event", not NULL.
		no := false
		p.AllDay = &no
	}

	p.RelatedTaskID = in.RelatedTaskID
	p.RelatedGoalID = in.RelatedGoalID
	return p, errs.OrNil()
}

// ValidateFilter checks the range query and clamps the paging.
//
// The range is required. An unbounded read of this table is not offered, and
// the absence is answered as a field error naming both parameters rather than
// by quietly picking a window the caller did not ask for: a client that got
// back "your next 50 events" when it asked for "all of them" would draw the
// wrong calendar and never know.
func ValidateFilter(f Filter) (Filter, error) {
	var errs validate.Errors

	switch {
	case f.Start.IsZero():
		errs = append(errs, validate.Error{Field: "start", Message: "is required"})
	case f.End.IsZero():
		errs = append(errs, validate.Error{Field: "end", Message: "is required"})
	case !f.End.After(f.Start):
		// Half-open, so an empty window returns nothing whatever is in it --
		// which is a query nobody meant to write.
		errs = append(errs, validate.Error{Field: "end", Message: "must be after start"})
	case f.End.Sub(f.Start) > MaxWindow:
		errs = append(errs, validate.Error{
			Field:   "end",
			Message: fmt.Sprintf("must be at most %d days after start", int(MaxWindow/(24*time.Hour))),
		})
	}

	if f.Sort == "" {
		f.Sort = DefaultSort
	}
	if _, ok := Sorts[f.Sort]; !ok {
		errs = append(errs, validate.Error{Field: "sort", Message: "is not a sortable field"})
	}
	f.Query = strings.TrimSpace(f.Query)
	if e := validate.MaxLen("q", f.Query, validate.MaxQueryLen); e != nil {
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

// interval is the CHECK constraint, in Go: an event that ends before it starts
// is a field error naming the end, not a 500 from Postgres.
func interval(start, end time.Time) *validate.Error {
	if end.Before(start) {
		return &validate.Error{Field: "end_time", Message: "must not be before start_time"}
	}
	return nil
}
