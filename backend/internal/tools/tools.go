// Package tools is the registry of things the assistant can do on a user's
// behalf.
//
// A tool is a fixed, reviewed Go function over one of the existing services --
// tasks, goals, notes, documents, calendar, finance, study -- with a name, a description, an input
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

	"github.com/jashveer/lifeos/backend/internal/calendar"
	"github.com/jashveer/lifeos/backend/internal/documents"
	"github.com/jashveer/lifeos/backend/internal/finance"
	"github.com/jashveer/lifeos/backend/internal/goals"
	"github.com/jashveer/lifeos/backend/internal/notes"
	"github.com/jashveer/lifeos/backend/internal/study"
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
	// Filter marks an optional argument that narrows what a read returns. The
	// router drops one whose value the user's message does not give, rather
	// than let a guessed value empty a search; see ValueGrounded. It implies
	// Grounded.
	Filter bool
	// Grounded marks an argument whose value must come from the user's message
	// whatever kind of tool it belongs to, including a write's.
	//
	// It exists because a write has one argument shaped like a filter: a
	// closed-set value the user either said or did not. Measured, on "I spent
	// 1450.50 on printer cartridges today, log it": llama3.2:3b wrote
	// `currency: "USD"` for a message with no currency in it, and the expense
	// was filed in a second currency that is never totalled with the rest --
	// which is the silent wrongness Filter exists to prevent, on the write
	// side. A title or an amount is the user's own words echoed back and is
	// checked by their eyes on the proposal; "USD" is a fact the assistant
	// made up, and it reads like one the user supplied.
	Grounded bool
	// Synonyms are the words that ground an Enum filter besides its values.
	Synonyms map[string]string
	// Cues are words one of which must also be in the message for a free-text
	// filter to be grounded -- "tag" for a tag, so a topic is not read as one.
	Cues []string
	// Aliases are the other keys a model writes for this argument. They matter
	// for a filter: the router drops an ungrounded filter by key, so a key the
	// tool reads and the declaration does not name would be a value that
	// escapes the grounding check entirely.
	Aliases []string
	// Derived marks a key the *tool* fills in while the call is prepared,
	// rather than one the model supplies.
	//
	// There are two: generate_flashcards' `cards` and generate_quiz's
	// `questions`. Those tools' proposals have to show the user the actual
	// content before they approve it, so it is written during Prepare and
	// stored in the canonical input -- which means the input carries a key the
	// model never wrote, and the invariant that every stored key is a declared
	// param has to be kept some other way. This is that way: the key is
	// declared, so the input is still exactly the declaration, and the model
	// is not offered it (Tool.InputSchema and the routing prompt both leave it
	// out) so it is never asked to invent one.
	//
	// A derived param is never Filter or Grounded: the router's grounding
	// check is about values a model read out of the message, and this is a
	// value no model wrote.
	Derived bool
}

// Offered reports whether the model is shown this parameter and may write it.
func (p Param) Offered() bool { return !p.Derived }

// Keys are every argument key this parameter is read from, its own name first.
func (p Param) Keys() []string { return append([]string{p.Name}, p.Aliases...) }

// MustBeGrounded reports whether the router has to check this argument's value
// against the message before the tool sees it.
func (p Param) MustBeGrounded() bool { return p.Filter || p.Grounded }

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
	Output   any
	Tasks    []tasks.Task
	Goals    []goals.Goal
	Notes    []notes.Note
	Events   []calendar.Event
	Expenses []finance.Expense
	Plans    []study.Plan
	// Quizzes are what search_quizzes found and what an approved generate_quiz
	// created. A quiz record carries no questions and no answers; see
	// quizRecord.
	Quizzes []study.Quiz
	Chunks  []documents.SearchResult
	// Reports are figures a tool worked out from the user's records rather
	// than records it found. They exist for analyze_spending, which answers
	// "how much did I spend on food this month" with a total: there is no row
	// that is the answer, and handing the model the hundred rows it was
	// computed from would be both wasteful and an invitation to add them up
	// again, differently. See tools.Report.
	Reports []Report
	// More reports that a search found more than it returned.
	More bool
}

// Report is one computed figure, already written out for the model.
//
// Title names it and Text is the whole of it, rendered here rather than in the
// orchestrator: the arithmetic and the words describing it belong to the tool
// that did the arithmetic, and a second place that formats totals is a second
// place they can be formatted wrongly.
type Report struct {
	Title string
	Text  string
}

// Count is how many records the result carries. A report counts as one: it is
// one thing the model is shown and one thing it can cite.
func (r Result) Count() int {
	return len(r.Tasks) + len(r.Goals) + len(r.Notes) + len(r.Events) +
		len(r.Expenses) + len(r.Plans) + len(r.Quizzes) + len(r.Chunks) + len(r.Reports)
}

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
