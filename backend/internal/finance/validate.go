package finance

import (
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/optional"
	"github.com/jashveer/lifeos/backend/internal/validate"
)

// ValidateCategoryName checks and normalizes a category name.
//
// The name is trimmed and its internal whitespace collapsed, because the
// uniqueness that makes "log it under food" resolvable is the database's
// UNIQUE (user_id, name) -- and "Food " and "Food" are two rows to that index
// and one category to a person.
func ValidateCategoryName(name string) (string, error) {
	name = strings.Join(strings.Fields(name), " ")
	var errs validate.Errors
	switch {
	case name == "":
		errs = append(errs, validate.Error{Field: "name", Message: "is required"})
	case len([]rune(name)) > MaxCategoryNameLen:
		errs = append(errs, validate.Error{
			Field:   "name",
			Message: fmt.Sprintf("must be at most %d characters", MaxCategoryNameLen),
		})
	}
	return name, errs.OrNil()
}

// ValidateCreate checks and normalizes a new expense, reporting every problem
// at once.
func ValidateCreate(in CreateInput) (CreateInput, error) {
	var errs validate.Errors

	if e := amount("amount", in.Amount); e != nil {
		errs = append(errs, *e)
	}

	currency, e := normalizeCurrency(in.Currency)
	if e != nil {
		errs = append(errs, *e)
	}
	in.Currency = currency

	in.Description = strings.TrimSpace(in.Description)
	if e := validate.MaxLen("description", in.Description, MaxDescriptionLen); e != nil {
		errs = append(errs, *e)
	}

	if in.Date.IsZero() {
		errs = append(errs, validate.Error{Field: "expense_date", Message: "is required"})
	} else {
		in.Date = Day(in.Date)
	}
	if e := categoryID("category_id", in.CategoryID); e != nil {
		errs = append(errs, *e)
	}
	if e := categoryID("related_document_id", in.RelatedDocumentID); e != nil {
		errs = append(errs, *e)
	}
	return in, errs.OrNil()
}

// ValidatePatch turns a PATCH body into the column instructions the repository
// applies. Fields the client did not mention produce no instruction at all.
func ValidatePatch(in UpdateInput) (Patch, error) {
	var errs validate.Errors
	var p Patch

	// amount, currency and expense_date are all NOT NULL, so an explicit null
	// is a validation failure rather than a column being cleared.
	if v, ok := in.Amount.Get(); ok {
		if e := amount("amount", v); e != nil {
			errs = append(errs, *e)
		} else {
			p.Amount = &v
		}
	} else if in.Amount.Cleared() {
		errs = append(errs, validate.Error{Field: "amount", Message: "is required"})
	}

	if v, ok := in.Currency.Get(); ok {
		c, e := normalizeCurrency(v)
		if e != nil {
			errs = append(errs, *e)
		} else {
			p.Currency = &c
		}
	} else if in.Currency.Cleared() {
		errs = append(errs, validate.Error{Field: "currency", Message: "is required"})
	}

	if v, ok := in.Date.Get(); ok {
		day := Day(v)
		p.Date = &day
	} else if in.Date.Cleared() {
		errs = append(errs, validate.Error{Field: "expense_date", Message: "is required"})
	}

	if v, ok := in.Description.Get(); ok {
		if e := validate.MaxLen("description", v, MaxDescriptionLen); e != nil {
			errs = append(errs, *e)
		}
		p.Description = validate.TrimmedField(v)
	} else if in.Description.Cleared() {
		p.Description = optional.Null[string]()
	}

	// Both links are nullable, so a null clears them -- and clearing needs no
	// ownership check, because null belongs to nobody.
	if v, ok := in.CategoryID.Get(); ok {
		if e := categoryID("category_id", &v); e != nil {
			errs = append(errs, *e)
		}
		p.CategoryID = optional.Of(v)
	} else if in.CategoryID.Cleared() {
		p.CategoryID = optional.Null[uuid.UUID]()
	}
	if v, ok := in.RelatedDocumentID.Get(); ok {
		if e := categoryID("related_document_id", &v); e != nil {
			errs = append(errs, *e)
		}
		p.RelatedDocumentID = optional.Of(v)
	} else if in.RelatedDocumentID.Cleared() {
		p.RelatedDocumentID = optional.Null[uuid.UUID]()
	}
	return p, errs.OrNil()
}

// ValidateFilter checks the query and clamps the paging.
//
// The range is optional here, unlike the calendar's: a history question with no
// dates in it is a reasonable question about a table that grows a row per
// purchase. What is checked is that a range the caller *did* give runs forwards
// -- `?start=2026-09-30&end=2026-09-01` returns nothing whatever is in the
// table, which is a query nobody meant to write.
func ValidateFilter(f Filter) (Filter, error) {
	var errs validate.Errors

	if f.Start != nil {
		day := Day(*f.Start)
		f.Start = &day
	}
	if f.End != nil {
		day := Day(*f.End)
		f.End = &day
	}
	if f.Start != nil && f.End != nil && f.End.Before(*f.Start) {
		errs = append(errs, validate.Error{Field: "end", Message: "must not be before start"})
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
	if e := categoryID("category_id", f.CategoryID); e != nil {
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

// amount is the CHECK constraint, in Go: an expense of nothing, of a negative
// sum, or of more than the column holds is a field error naming the amount
// rather than a 500 from Postgres.
func amount(field string, a Amount) *validate.Error {
	switch {
	case a <= 0:
		return &validate.Error{Field: field, Message: "must be greater than zero"}
	case a > MaxAmount:
		return &validate.Error{Field: field, Message: "must be at most " + MaxAmount.String()}
	}
	return nil
}

// normalizeCurrency upper-cases the code and checks its shape. An empty one is
// the column default rather than an error: most callers have one currency and
// never mention it.
//
// The code is not checked against a list of real currencies. Nothing in this
// module does anything with it except group by it and print it back, so a list
// would only be a way to reject somebody's currency for not being in a table
// that was right when it was written.
func normalizeCurrency(c string) (string, *validate.Error) {
	c = strings.ToUpper(strings.TrimSpace(c))
	if c == "" {
		return DefaultCurrency, nil
	}
	if len([]rune(c)) != CurrencyLen || !onlyLetters(c) {
		return c, &validate.Error{
			Field:   "currency",
			Message: fmt.Sprintf("must be a %d-letter currency code such as %s", CurrencyLen, DefaultCurrency),
		}
	}
	return c, nil
}

func onlyLetters(s string) bool {
	for _, r := range s {
		if !unicode.IsLetter(r) {
			return false
		}
	}
	return true
}

// categoryID rejects the zero uuid, which is what a client sends when it means
// "none" and has built the id out of nothing. The ownership of a real id is the
// service's business, not validation's.
func categoryID(field string, id *uuid.UUID) *validate.Error {
	if id != nil && *id == uuid.Nil {
		return &validate.Error{Field: field, Message: "must be an id"}
	}
	return nil
}

// Day is a timestamp reduced to the date it falls on, at midnight UTC.
//
// Every date in this module goes through it, on the way in and on the way out,
// which is what makes "spent on the 17th" one value rather than one per time of
// day the client happened to send. The column is a `date`, so Postgres would
// truncate anyway; doing it here means the value the service validated and the
// value the table holds are the same one.
func Day(t time.Time) time.Time {
	utc := t.UTC()
	return time.Date(utc.Year(), utc.Month(), utc.Day(), 0, 0, 0, 0, time.UTC)
}
