package study

import (
	"strings"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/optional"
	"github.com/jashveer/lifeos/backend/internal/validate"
)

// ValidateCreatePlan checks and normalizes a new plan, reporting every problem
// at once.
func ValidateCreatePlan(in CreatePlanInput) (CreatePlanInput, error) {
	var errs validate.Errors

	title, e := validate.Title("title", in.Title)
	if e != nil {
		errs = append(errs, *e)
	}
	in.Title = title

	in.Description = strings.TrimSpace(in.Description)
	if e := validate.MaxLen("description", in.Description, validate.MaxDescriptionLen); e != nil {
		errs = append(errs, *e)
	}

	// An unstated status is `active`, which is the column default and the only
	// state a plan can reasonably be born in. It is filled in here rather than
	// left to the database so the value the service validated and the value
	// the row holds are the same one.
	if strings.TrimSpace(in.Status) == "" {
		in.Status = StatusActive
	}
	in.Status = strings.ToLower(strings.TrimSpace(in.Status))
	if e := validate.OneOf("status", in.Status, Statuses); e != nil {
		errs = append(errs, *e)
	}

	if e := linkID("document_id", in.DocumentID); e != nil {
		errs = append(errs, *e)
	}
	return in, errs.OrNil()
}

// ValidatePatch turns a PATCH body into the column instructions the repository
// applies. Fields the client did not mention produce no instruction at all.
func ValidatePatch(in UpdatePlanInput) (Patch, error) {
	var errs validate.Errors
	var p Patch

	// title and status are NOT NULL, so an explicit null is a validation
	// failure rather than a column being cleared.
	if v, ok := in.Title.Get(); ok {
		title, e := validate.Title("title", v)
		if e != nil {
			errs = append(errs, *e)
		} else {
			p.Title = &title
		}
	} else if in.Title.Cleared() {
		errs = append(errs, validate.Error{Field: "title", Message: "is required"})
	}

	if v, ok := in.Status.Get(); ok {
		status := strings.ToLower(strings.TrimSpace(v))
		if e := validate.OneOf("status", status, Statuses); e != nil {
			errs = append(errs, *e)
		} else {
			p.Status = &status
		}
	} else if in.Status.Cleared() {
		errs = append(errs, validate.Error{Field: "status", Message: "is required"})
	}

	if v, ok := in.Description.Get(); ok {
		if e := validate.MaxLen("description", v, validate.MaxDescriptionLen); e != nil {
			errs = append(errs, *e)
		}
		p.Description = validate.TrimmedField(v)
	} else if in.Description.Cleared() {
		p.Description = optional.Null[string]()
	}

	// The document link is nullable, so a null clears it -- and clearing needs
	// no ownership check, because null belongs to nobody.
	if v, ok := in.DocumentID.Get(); ok {
		if e := linkID("document_id", &v); e != nil {
			errs = append(errs, *e)
		}
		p.DocumentID = optional.Of(v)
	} else if in.DocumentID.Cleared() {
		p.DocumentID = optional.Null[uuid.UUID]()
	}
	return p, errs.OrNil()
}

// ValidateFilter checks the query and clamps the paging.
func ValidateFilter(f Filter) (Filter, error) {
	var errs validate.Errors

	if f.Status != "" {
		f.Status = strings.ToLower(strings.TrimSpace(f.Status))
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
	f.Query = strings.TrimSpace(f.Query)
	if e := validate.MaxLen("q", f.Query, validate.MaxQueryLen); e != nil {
		errs = append(errs, *e)
	}
	if e := linkID("document_id", f.DocumentID); e != nil {
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

// ValidateCardFilter clamps the paging on a deck read. The limits are the
// card ones, not the resource ones; see DefaultCardLimit.
func ValidateCardFilter(f CardFilter) (CardFilter, error) {
	var errs validate.Errors
	switch {
	case f.Limit < 0:
		errs = append(errs, validate.Error{Field: "limit", Message: "must not be negative"})
	case f.Limit == 0:
		f.Limit = DefaultCardLimit
	case f.Limit > MaxCardLimit:
		f.Limit = MaxCardLimit
	}
	if f.Offset < 0 {
		errs = append(errs, validate.Error{Field: "offset", Message: "must not be negative"})
	}
	return f, errs.OrNil()
}

// ValidateCard checks and normalizes one card, whichever side it came from:
// the manual-add endpoint, or a card the model wrote.
//
// Both sides are required. A card with no back is not a card that is merely
// incomplete -- it is one that will be shown to somebody trying to learn, with
// nothing on the other side.
func ValidateCard(c NewCard) (NewCard, error) {
	var errs validate.Errors
	c.Front = collapse(c.Front)
	c.Back = collapse(c.Back)
	for _, side := range []struct {
		field string
		value string
	}{{"front", c.Front}, {"back", c.Back}} {
		switch {
		case side.value == "":
			errs = append(errs, validate.Error{Field: side.field, Message: "is required"})
		default:
			if e := validate.MaxLen(side.field, side.value, MaxCardSideLen); e != nil {
				errs = append(errs, *e)
			}
		}
	}
	return c, errs.OrNil()
}

// ValidateGenerate checks and normalizes a generation request, clamping the
// count rather than refusing it: "make me fifty cards" is a reasonable thing
// to say and MaxCardsPerBatch is the honest answer to it.
func ValidateGenerate(in GenerateInput) (GenerateInput, error) {
	var errs validate.Errors

	if in.DocumentID == uuid.Nil {
		errs = append(errs, validate.Error{Field: "document_id", Message: "is required"})
	}
	if e := linkID("study_plan_id", in.StudyPlanID); e != nil {
		errs = append(errs, *e)
	}
	in.Topic = collapse(in.Topic)
	if e := validate.MaxLen("topic", in.Topic, MaxTopicLen); e != nil {
		errs = append(errs, *e)
	}
	switch {
	case in.Count < 0:
		errs = append(errs, validate.Error{Field: "count", Message: "must not be negative"})
	case in.Count == 0:
		in.Count = DefaultCardsPerBatch
	case in.Count > MaxCardsPerBatch:
		in.Count = MaxCardsPerBatch
	}
	return in, errs.OrNil()
}

// linkID rejects the zero uuid, which is what a client sends when it means
// "none" and has built the id out of nothing. The ownership of a real id is
// the service's business, not validation's.
func linkID(field string, id *uuid.UUID) *validate.Error {
	if id != nil && *id == uuid.Nil {
		return &validate.Error{Field: field, Message: "must be an id"}
	}
	return nil
}

// collapse trims a value and collapses its internal whitespace.
//
// It is applied to both sides of every card, including the ones the model
// wrote, because a model asked for JSON writes multi-line strings with the
// indentation of the document it read, and "What  is\n  the boiling point?" is
// the same question as the tidy one to a person and a different string to
// everything else -- the duplicate check in ParseFlashcards included.
func collapse(s string) string { return strings.Join(strings.Fields(s), " ") }
