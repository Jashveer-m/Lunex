// Package actions is the action engine: the record of every tool call the
// assistant makes on a user's behalf, and the approval flow that stands between
// a proposed write and the user's data.
//
// The state machine is small and it is the whole of the package:
//
//	read:   (runs during the turn) ─▶ executed | failed
//	write:  proposed ─▶ approved ─▶ executed | failed
//	                 └─▶ rejected
//
// A read is recorded already finished: it ran when the assistant decided to run
// it, because looking changes nothing. A write is recorded as *proposed* and
// nothing else -- Insert refuses a write in any other state -- and it leaves
// that state in exactly two ways: the user rejects it, or the user approves it,
// which is one atomic UPDATE that only one request can win and that is the
// only thing internal/tools will run a write tool after. There is no
// transition that skips `approved`, and no code path that reaches a write tool
// from the chat turn. That rule is the one this phase is not allowed to relax,
// even if the user asks; see docs/decisions.md.
//
// `approved` is transient. It means "the user said yes and the tool is
// running", and it becomes executed or failed within the same request. A row
// left in `approved` is a process that died mid-execution, whose outcome is
// unknown -- and it is never retried automatically, because retrying a create
// that did in fact land would create it twice.
package actions

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/tools"
)

var (
	// ErrNotFound covers both "no such action" and "that action is somebody
	// else's", which is what keeps the API from confirming foreign ids.
	ErrNotFound = errors.New("action not found")
	// ErrNotPending is approving or rejecting an action that is no longer
	// proposed: already approved, rejected, executed or failed -- or a read,
	// which was never pending at all.
	ErrNotPending = errors.New("action is not awaiting approval")
	// ErrInvalidState is an attempt to record an action in a state the machine
	// above does not allow, such as a write that is already executed. It is a
	// programming error and is refused before any SQL runs.
	ErrInvalidState = errors.New("invalid action state")
)

// The statuses, mirrored by the CHECK constraint in migration 000007.
const (
	StatusProposed = "proposed"
	StatusApproved = "approved"
	StatusRejected = "rejected"
	StatusExecuted = "executed"
	StatusFailed   = "failed"
)

// Statuses is the allow-list for the ?status= filter.
var Statuses = []string{StatusProposed, StatusApproved, StatusRejected, StatusExecuted, StatusFailed}

// Permissions is the allow-list for the ?permission_level= filter.
var Permissions = []string{string(tools.Read), string(tools.Write)}

// Action mirrors a row of the actions table.
type Action struct {
	ID             uuid.UUID
	UserID         uuid.UUID
	ConversationID *uuid.UUID
	ToolName       string
	// Input is the canonical input the tool runs from: validated, references
	// resolved. For a write it is exactly what the user approves.
	Input      json.RawMessage
	Permission tools.Permission
	Status     string
	// Result is nil unless the action executed.
	Result json.RawMessage
	// ErrorMessage is nil unless the action failed. It is written for the user
	// and never carries the underlying error; see clientMessage.
	ErrorMessage *string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// Pending reports whether the action is waiting for the user.
func (a Action) Pending() bool { return a.Status == StatusProposed }

// NewAction is an action about to be recorded. Build one with Proposal or Ran
// rather than by hand: they are the only two shapes a new row may have.
type NewAction struct {
	Call   tools.Call
	Status string
	// Result is the read's output, for an executed read.
	Result any
	// ErrorMessage is why a read failed.
	ErrorMessage string
}

// Proposal is a write the assistant wants to make, recorded for the user to
// approve or reject.
func Proposal(call tools.Call) NewAction {
	return NewAction{Call: call, Status: StatusProposed}
}

// Ran is a read that has already run.
func Ran(call tools.Call, result tools.Result) NewAction {
	return NewAction{Call: call, Status: StatusExecuted, Result: result.Output}
}

// Validate enforces the state machine on insert. A write can only be born
// proposed; a read can only be born finished. Insert calls it on every action
// before writing any, and it is exported so a test double of the turn's store
// can hold itself to the same rule.
func (n NewAction) Validate() error {
	switch n.Call.Permission {
	case tools.Write:
		if n.Status != StatusProposed || n.Result != nil || n.ErrorMessage != "" {
			return errors.Join(ErrInvalidState, errors.New("a write action can only be recorded as proposed"))
		}
	case tools.Read:
		switch {
		case n.Status == StatusExecuted && n.Result != nil && n.ErrorMessage == "":
		case n.Status == StatusFailed && n.Result == nil && n.ErrorMessage != "":
		default:
			return errors.Join(ErrInvalidState, errors.New("a read action can only be recorded executed with a result, or failed with a reason"))
		}
	default:
		return errors.Join(ErrInvalidState, errors.New("unknown permission level "+string(n.Call.Permission)))
	}
	if n.Call.Tool == "" || len(n.Call.Input) == 0 {
		return errors.Join(ErrInvalidState, errors.New("an action needs a tool and an input"))
	}
	return nil
}

// Filter is the query behind GET /actions.
type Filter struct {
	Status         string
	Permission     string
	ConversationID *uuid.UUID
	Limit          int
	Offset         int
}

// Paging bounds, as everywhere else.
const (
	DefaultLimit = 50
	MaxLimit     = 200
)
