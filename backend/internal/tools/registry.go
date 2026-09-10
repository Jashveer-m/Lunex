package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"

	"github.com/google/uuid"
)

// Ledger is the record of actions the registry consults before it runs a write
// tool. internal/actions implements it over the actions table.
//
// It is declared here, and taken at construction, for the same reason the
// resource modules declare their NodeSyncer: this package depends on
// "something that can approve an action", not on the package that stores them.
type Ledger interface {
	// Approve moves one of the user's actions from proposed to approved and
	// returns what was approved.
	//
	// It must be atomic and single-use: of any number of concurrent calls for
	// one action, exactly one succeeds, and every call after that fails. An
	// action that is not the user's, does not exist, or is not proposed is an
	// error. RunApproved runs nothing unless this returned without one.
	Approve(ctx context.Context, userID, actionID uuid.UUID) (Approved, error)
}

// Approved is an action the ledger has just approved: the tool it names and the
// canonical input stored with it. It is the only thing a write tool is ever run
// from.
type Approved struct {
	ActionID uuid.UUID
	Tool     string
	Input    json.RawMessage
}

// Execution is the outcome of running an approved action. Err is the tool's own
// failure; the approval itself succeeded, and the action engine records Err as
// the action's outcome rather than treating it as a failed request.
type Execution struct {
	Approved Approved
	Result   Result
	Err      error
}

// Registry is the fixed set of tools, and the only way to run one.
type Registry struct {
	byName map[string]Tool
	names  []string
	ledger Ledger
}

// toolName is the allowed shape of a name: it goes into a prompt, an SSE frame
// and a database column, and nothing about it should need escaping in any of
// them.
var toolName = regexp.MustCompile(`^[a-z][a-z_]{2,63}$`)

// NewRegistry registers the tools, in order. A tool without a name, a
// description, a permission or its functions, or two with one name, is a
// programming error and an error here rather than a surprise at the first call.
//
// ledger may be nil, in which case no write can ever run: RunApproved answers
// ErrNoLedger. That is a registry for a process that only reads.
func NewRegistry(ledger Ledger, tools ...Tool) (*Registry, error) {
	r := &Registry{byName: make(map[string]Tool, len(tools)), ledger: ledger}
	for _, t := range tools {
		switch {
		case !toolName.MatchString(t.Name):
			return nil, fmt.Errorf("tool name %q is not snake_case", t.Name)
		case t.Description == "":
			return nil, fmt.Errorf("tool %s has no description", t.Name)
		case t.Permission != Read && t.Permission != Write:
			return nil, fmt.Errorf("tool %s has permission %q, want read or write", t.Name, t.Permission)
		case t.prepare == nil || t.run == nil || t.describe == nil:
			return nil, fmt.Errorf("tool %s was not built with define", t.Name)
		}
		if _, dup := r.byName[t.Name]; dup {
			return nil, fmt.Errorf("tool %s is registered twice", t.Name)
		}
		r.byName[t.Name] = t
		r.names = append(r.names, t.Name)
	}
	return r, nil
}

// Tool returns one tool's declaration.
func (r *Registry) Tool(name string) (Tool, bool) {
	t, ok := r.byName[name]
	return t, ok
}

// Tools returns every tool, in registration order.
func (r *Registry) Tools() []Tool {
	out := make([]Tool, 0, len(r.names))
	for _, n := range r.names {
		out = append(out, r.byName[n])
	}
	return out
}

// Prepare turns a model-written call into a validated, canonical Call. It may
// read -- update_task looks the task up to resolve "the scheduler task" to an
// id -- and it never writes, for any tool.
//
// An argument problem is an *ArgumentError. Anything else is an outage the
// caller should treat as one.
func (r *Registry) Prepare(ctx context.Context, userID uuid.UUID, name string, args Args) (Call, error) {
	t, ok := r.byName[name]
	if !ok {
		return Call{}, fmt.Errorf("%w: %q", ErrUnknownTool, name)
	}
	if args == nil {
		args = Args{}
	}
	input, err := t.prepare(ctx, userID, args)
	if err != nil {
		return Call{}, err
	}
	summary, err := t.describe(input)
	if err != nil {
		return Call{}, err
	}
	return Call{Tool: t.Name, Permission: t.Permission, Input: input, Summary: summary}, nil
}

// RunRead runs a read tool.
//
// The permission is looked up in the registry rather than read off the Call,
// which is a plain struct any caller can build: a Call claiming `read` for
// create_task is refused here exactly like one that says `write`.
func (r *Registry) RunRead(ctx context.Context, userID uuid.UUID, call Call) (Result, error) {
	t, ok := r.byName[call.Tool]
	if !ok {
		return Result{}, fmt.Errorf("%w: %q", ErrUnknownTool, call.Tool)
	}
	if t.Permission != Read {
		return Result{}, fmt.Errorf("%w: %s", ErrApprovalRequired, t.Name)
	}
	return t.run(ctx, userID, call.Input)
}

// RunApproved approves one proposed action and runs it. It is the only path
// from this package to a write tool.
//
// The caller names an action and nothing else. Whether it may run is the
// ledger's answer, not the caller's claim, and *what* runs -- the tool and its
// input -- comes out of the row the ledger just approved. So there is no way to
// get a write executed with an input the user did not approve, or without an
// approval at all, or twice: the ledger's approve is single-use.
//
// The error is non-nil only when nothing ran: the action was not found, not
// pending, or the ledger failed. Once the approval has happened the outcome is
// in Execution.Err, because an approved action whose tool then failed is a fact
// to record, not a request to retry.
func (r *Registry) RunApproved(ctx context.Context, userID, actionID uuid.UUID) (Execution, error) {
	if r.ledger == nil {
		return Execution{}, ErrNoLedger
	}
	approved, err := r.ledger.Approve(ctx, userID, actionID)
	if err != nil {
		return Execution{}, err
	}
	exec := Execution{Approved: approved}
	t, ok := r.byName[approved.Tool]
	switch {
	case !ok:
		// A tool that existed when the action was proposed and has since been
		// removed. The row stays approved-then-failed rather than running
		// something else under the old name.
		exec.Err = fmt.Errorf("%w: %q", ErrUnknownTool, approved.Tool)
	case t.Permission != Write:
		exec.Err = fmt.Errorf("%w: %s", ErrNotApprovable, t.Name)
	default:
		exec.Result, exec.Err = t.run(ctx, userID, approved.Input)
	}
	return exec, nil
}

// Describe renders a stored input as the one sentence a proposal is shown as.
// It is derived on every read rather than stored, so the summary can never
// disagree with the input it describes.
func (r *Registry) Describe(name string, input json.RawMessage) string {
	t, ok := r.byName[name]
	if !ok {
		return "Run " + name + " (a tool this version no longer has)."
	}
	s, err := t.describe(input)
	if err != nil {
		return "Run " + name + " (its stored input could not be read)."
	}
	return s
}
