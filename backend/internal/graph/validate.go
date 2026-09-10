package graph

import (
	"github.com/jashveer/lifeos/backend/internal/validate"
)

// ValidateFilter clamps the graph read; an unknown type is a client error
// rather than a silent fallback to "everything", which would answer a typo'd
// filter with data the caller did not ask for.
func ValidateFilter(f Filter) (Filter, error) {
	var errs validate.Errors

	if f.Type != "" {
		if e := validate.OneOf("type", f.Type, NodeTypes); e != nil {
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

// ValidateCandidate is the quality gate between what the model proposed and
// what reaches the database.
//
// It is stricter than the CHECK constraints on purpose. The constraints stop a
// row the schema cannot represent; this stops a row the schema would happily
// hold and that would make the graph worse: a sentence used as an entity name,
// a relationship the model was not sure it read, and a node related to itself.
//
// userMessage is the user's half of the exchange, and it is what
// GroundedInMessage checks both entity names against -- see there for the
// measured failure that check exists for.
//
// It returns a reason rather than an error value because every caller does the
// same thing with it -- writes it to the debug log and moves to the next
// candidate. Nothing here reaches a user.
func ValidateCandidate(c Candidate, userMessage string) (string, bool) {
	switch {
	case !Nameable(c.From):
		return "the name of the first entity is not a name", false
	case !Nameable(c.To):
		return "the name of the second entity is not a name", false
	case !GroundedInMessage(c.From, userMessage):
		return "the first entity is not named in the user's message", false
	case !GroundedInMessage(c.To, userMessage):
		return "the second entity is not named in the user's message", false
	case FoldLabel(c.From) == FoldLabel(c.To):
		// The CHECK constraint would reject the edge once both ends resolved
		// to the same node; catching it here means the node is never created
		// for the sake of an edge that cannot exist.
		return "both ends are the same entity", false
	case c.Confidence < MinKeptConfidence:
		return "the model scored it too low", false
	}
	return "", true
}
