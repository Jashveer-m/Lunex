// Package tools is the registry of things the assistant can do on a user's
// behalf.
//
// A tool is a fixed, reviewed Go function over one of the existing services --
// tasks, goals, notes, documents -- with a name, a description, an input
// schema, an output schema and a permission level. There is nothing else: no
// tool runs code the model wrote, builds SQL out of what the model said, or
// reaches a service method its interface does not name. The model's only power
// is to pick a tool by name and suggest arguments, and the arguments are parsed
// leniently, validated strictly, and rewritten into a canonical input before
// anything is done with them.
//
// Permission is the one distinction the package is built around.
//
// A read tool runs when the assistant decides to run it; its result goes into
// the prompt the same way retrieved documents do.
//
// A write tool does not run when the assistant decides to. There is no method
// here that takes a write tool's name and an input and runs it. The only way to
// execute one is RunApproved, which takes an *action id*, asks the Ledger to
// move that action from proposed to approved -- atomically, at most once -- and
// then runs the tool named in the approved row with the input stored in the
// approved row. The caller supplies neither. That is what makes "no write
// without an approved action" a property of this package rather than a rule
// its callers have to remember; see docs/decisions.md.
//
// Deletion is deliberately absent. None of the service interfaces below has a
// Delete method, so no tool can reach one even by mistake.
package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/documents"
	"github.com/jashveer/lifeos/backend/internal/goals"
	"github.com/jashveer/lifeos/backend/internal/notes"
	"github.com/jashveer/lifeos/backend/internal/tasks"
)

// Permission is what running a tool does to the user's data.
type Permission string

const (
	// Read tools look and change nothing. They run without approval.
	Read Permission = "read"
	// Write tools change the user's data. They run only from an approved
	// action.
	Write Permission = "write"
)

var (
	// ErrUnknownTool is a name the registry does not have.
	ErrUnknownTool = errors.New("unknown tool")
	// ErrApprovalRequired is what asking to run a write tool directly gets.
	ErrApprovalRequired = errors.New("a write tool runs only from an approved action")
	// ErrNotApprovable is an approved action naming a tool that is not a write
	// tool. It cannot happen through the action engine, which only proposes
	// writes; it is here because RunApproved does not trust its ledger to have
	// checked.
	ErrNotApprovable = errors.New("the approved action does not name a write tool")
	// ErrNoLedger is a registry built without one, asked to run a write.
	ErrNoLedger = errors.New("the registry has no ledger, so nothing can be approved")
	// ErrInvalidArguments classifies an ArgumentError.
	ErrInvalidArguments = errors.New("invalid tool arguments")
)

// ArgumentError is a call the model made that cannot become a valid input:
// a required argument missing, a date that is not a date, a task reference that
// matches nothing or more than one thing.
//
// It is kept apart from every other error because it is the model's mistake
// rather than an outage, and the orchestrator treats the two differently: an
// outage fails the turn, and an argument error is shown to the answering model
// so it can tell the user what it needs. Reason is written to be read by that
// model and, through it, by the user.
type ArgumentError struct {
	Tool   string
	Reason string
}

func (e *ArgumentError) Error() string {
	return fmt.Sprintf("%s: %s: %s", ErrInvalidArguments, e.Tool, e.Reason)
}

func (e *ArgumentError) Is(target error) bool { return target == ErrInvalidArguments }

func invalid(tool, format string, args ...any) error {
	return &ArgumentError{Tool: tool, Reason: fmt.Sprintf(format, args...)}
}

// Param is one argument a tool accepts. Params are the single description of a
// tool's input: the JSON Schema is generated from them, the routing prompt is
// rendered from them, and a test pins that the canonical input never carries a
// key they do not declare.
type Param struct {
	Name        string
	Type        string // "string" or "array" (of strings)
	Description string
	Enum        []string
	Required    bool
}

// Tool is one registered capability.
//
// The exported fields are the tool's declaration. The functions are unexported
// on purpose: nothing outside this package can run a tool except through the
// Registry, which is where the permission rule is enforced.
type Tool struct {
	Name        string
	Description string
	Permission  Permission
	Params      []Param
	// Output describes Result.Output, the JSON recorded as the action's result.
	Output Schema

	prepare  func(ctx context.Context, userID uuid.UUID, args Args) (json.RawMessage, error)
	run      func(ctx context.Context, userID uuid.UUID, input json.RawMessage) (Result, error)
	describe func(input json.RawMessage) (string, error)
}

// Call is a tool call whose arguments have been validated and made canonical.
// It is what the action engine stores and what a read tool runs from.
type Call struct {
	Tool       string
	Permission Permission
	// Input is canonical: exactly the JSON the tool's input type marshals to,
	// references resolved and values normalized. It is what the user is shown
	// and what an approval executes.
	Input json.RawMessage
	// Summary is one sentence saying what the call will do, derived from
	// Input.
	Summary string
}

// Result is what running a tool produced.
//
// Output is the JSON document recorded as the action's result, and matches the
// tool's Output schema. The typed slices carry the same records for the chat
// orchestrator, which renders them into the prompt with the functions it
// already uses for retrieved tasks and chunks -- so a task found by a search
// reads to the model exactly like a task retrieved any other way.
type Result struct {
	Output any
	Tasks  []tasks.Task
	Goals  []goals.Goal
	Notes  []notes.Note
	Chunks []documents.SearchResult
	// More reports that a search found more than it returned.
	More bool
}

// Count is how many records the result carries.
func (r Result) Count() int { return len(r.Tasks) + len(r.Goals) + len(r.Notes) + len(r.Chunks) }

// define builds a Tool whose canonical input is the Go type In.
//
// It is what keeps a stored input honest. prepare produces an In, which is
// marshalled to become Call.Input; run and describe decode Input back into an
// In *strictly* -- unknown fields rejected -- because the only thing that ever
// wrote it is json.Marshal on the same type. A stored input that does not
// round-trip is a row nobody should execute.
func define[In any](
	decl Tool,
	prepare func(ctx context.Context, userID uuid.UUID, args Args) (In, error),
	run func(ctx context.Context, userID uuid.UUID, in In) (Result, error),
	describe func(in In) string,
) Tool {
	decl.prepare = func(ctx context.Context, userID uuid.UUID, args Args) (json.RawMessage, error) {
		in, err := prepare(ctx, userID, args)
		if err != nil {
			return nil, err
		}
		raw, err := json.Marshal(in)
		if err != nil {
			return nil, fmt.Errorf("encode %s input: %w", decl.Name, err)
		}
		return raw, nil
	}
	decl.run = func(ctx context.Context, userID uuid.UUID, raw json.RawMessage) (Result, error) {
		in, err := decodeCanonical[In](raw)
		if err != nil {
			return Result{}, fmt.Errorf("decode %s input: %w", decl.Name, err)
		}
		return run(ctx, userID, in)
	}
	decl.describe = func(raw json.RawMessage) (string, error) {
		in, err := decodeCanonical[In](raw)
		if err != nil {
			return "", fmt.Errorf("decode %s input: %w", decl.Name, err)
		}
		return describe(in), nil
	}
	return decl
}

func decodeCanonical[In any](raw json.RawMessage) (In, error) {
	var in In
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return in, err
	}
	return in, nil
}
