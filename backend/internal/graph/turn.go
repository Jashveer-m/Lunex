package graph

import "github.com/google/uuid"

// Turn is one completed exchange, as the relationship extractor is allowed to
// read it.
//
// The graph extractor used to read the exchange as it stood when the answer
// was written, and for a turn that proposed a change that is the one moment the
// change does not exist. Approving "Renew my passport" left two disconnected
// nodes: an extracted `project` node carrying the edge, invented from the
// proposal's wording, and the real `task` node the approval created, with no
// edges at all. Unconfirmed and Anchor are the two halves of the fix.
type Turn struct {
	UserMessage      string
	AssistantMessage string
	// Unconfirmed is the text of changes the conversation asked for that have
	// not happened: proposed and waiting, rejected, or failed. A relationship
	// with an end named in one is not stored from the turn. If the change is
	// approved, the relationship is extracted again then, with an Anchor; if
	// it never is, there is nothing for it to be about.
	Unconfirmed []string
	// Anchor is set when the exchange is being read because an approved change
	// has just been carried out. The relationships kept are the ones with an
	// end named in the record the change produced, and that end is the record's
	// own node -- not a new node with a similar label.
	Anchor *Anchor
}

// Anchor is the row an executed action created or changed.
type Anchor struct {
	RefTable string // tasks, goals or notes
	RefID    uuid.UUID
	Label    string // its title
}

// namesUnconfirmed reports whether an extracted entity is named in a change that
// has not happened. The user is never one: they exist whatever was approved.
func namesUnconfirmed(label string, unconfirmed []string) bool {
	if IsSelf(label) {
		return false
	}
	for _, change := range unconfirmed {
		if GroundedInMessage(label, change) {
			return true
		}
	}
	return false
}

// namesAnchor reports whether an extracted entity is the anchored record: one
// significant word of it in the record's title, the same looseness
// GroundedInMessage allows against the user's message -- "passport renewal" is
// the task "Renew my passport".
func namesAnchor(label string, a *Anchor) bool {
	return a != nil && !IsSelf(label) && GroundedInMessage(label, a.Label)
}
