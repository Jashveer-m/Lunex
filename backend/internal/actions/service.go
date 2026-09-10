package actions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/goals"
	"github.com/jashveer/lifeos/backend/internal/notes"
	"github.com/jashveer/lifeos/backend/internal/tasks"
	"github.com/jashveer/lifeos/backend/internal/tools"
	"github.com/jashveer/lifeos/backend/internal/validate"
)

// Store is the slice of the repository the service needs. Every method takes
// the owner id.
type Store interface {
	ByID(ctx context.Context, userID, id uuid.UUID) (Action, error)
	List(ctx context.Context, userID uuid.UUID, f Filter) ([]Action, error)
	Reject(ctx context.Context, userID, id uuid.UUID) (Action, error)
	Finish(ctx context.Context, userID, id uuid.UUID, o Outcome) (Action, error)
	PendingDuplicate(ctx context.Context, userID, conversationID uuid.UUID, call tools.Call) (Action, bool, error)
}

// Runner is the tool registry, seen from the action engine: it can run an
// approved action and describe a stored one, and that is all.
//
// Note what the engine cannot do. It never names a tool and an input to run --
// there is no such method on the registry. It hands over an action id, and the
// registry approves that action through its ledger and runs what the row
// says. The engine's part is deciding *when* to ask (only on the user's
// explicit approve) and recording what happened.
type Runner interface {
	RunApproved(ctx context.Context, userID, actionID uuid.UUID) (tools.Execution, error)
	Describe(name string, input json.RawMessage) string
}

// DefaultExecutionTimeout bounds one approved action. Every write tool is a
// single insert or update plus a graph sync, so this is generous; it exists so
// a hung database cannot hold an approval open forever.
const DefaultExecutionTimeout = 20 * time.Second

// Service holds the action use cases. It is transport agnostic.
type Service struct {
	store   Store
	runner  Runner
	log     *slog.Logger
	timeout time.Duration
}

func NewService(store Store, runner Runner, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{store: store, runner: runner, log: log, timeout: DefaultExecutionTimeout}
}

func (s *Service) Get(ctx context.Context, userID, id uuid.UUID) (Action, error) {
	return s.store.ByID(ctx, userID, id)
}

func (s *Service) List(ctx context.Context, userID uuid.UUID, f Filter) ([]Action, error) {
	f, err := ValidateFilter(f)
	if err != nil {
		return nil, err
	}
	return s.store.List(ctx, userID, f)
}

// Approve runs one proposed write, because the user said so, and records how it
// went.
//
// Everything after the approval runs on a context detached from the request's
// cancellation (and bounded by its own timeout). By the time the registry
// returns, the approval is committed and the tool has run or failed; a client
// that hung up a moment earlier must not be able to leave the row in
// `approved` with the outcome unrecorded -- the one state that means "we do not
// know whether your task was created".
//
// The error is non-nil when nothing ran (the action is not the caller's, not
// pending, or the approval itself failed) or when the outcome could not be
// recorded. A tool that ran and failed is not an error: the action comes back
// `failed` with a reason, which is the answer to the request.
func (s *Service) Approve(ctx context.Context, userID, id uuid.UUID) (Action, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.timeout)
	defer cancel()

	exec, err := s.runner.RunApproved(ctx, userID, id)
	if err != nil {
		return Action{}, err
	}

	outcome := Outcome{Result: exec.Result.Output}
	if exec.Err != nil {
		outcome = Outcome{Failed: clientMessage(exec.Err)}
		s.log.Warn("approved action failed",
			"error", exec.Err, "user_id", userID, "action_id", id, "tool", exec.Approved.Tool)
	} else {
		s.log.Info("approved action executed", "user_id", userID, "action_id", id, "tool", exec.Approved.Tool)
	}
	a, err := s.store.Finish(ctx, userID, id, outcome)
	if err != nil {
		// The tool has run (or failed) and its outcome is not on the row. Say
		// so in the log in words, because the row now reads `approved` and the
		// only record of what happened is this line.
		s.log.Error("approved action ran but its outcome could not be recorded",
			"error", err, "user_id", userID, "action_id", id, "tool", exec.Approved.Tool,
			"tool_failed", exec.Err != nil)
		return Action{}, fmt.Errorf("record outcome of action %s: %w", id, err)
	}
	return a, nil
}

// Reject declines one proposed write. Nothing runs.
func (s *Service) Reject(ctx context.Context, userID, id uuid.UUID) (Action, error) {
	a, err := s.store.Reject(ctx, userID, id)
	if err != nil {
		return Action{}, err
	}
	s.log.Info("action rejected", "user_id", userID, "action_id", id, "tool", a.ToolName)
	return a, nil
}

// Describe is the one-sentence summary of an action, derived from its stored
// input.
func (s *Service) Describe(a Action) string {
	return s.runner.Describe(a.ToolName, a.Input)
}

// PendingDuplicate is the store's lookup, for the chat turn.
func (s *Service) PendingDuplicate(ctx context.Context, userID, conversationID uuid.UUID, call tools.Call) (Action, bool, error) {
	return s.store.PendingDuplicate(ctx, userID, conversationID, call)
}

// Recent returns the most recent writes proposed in one conversation, newest
// first. The chat turn shows them to the model so it knows what became of what
// it proposed -- otherwise it would go on saying "that is waiting for your
// approval" about a task the user approved an hour ago.
func (s *Service) Recent(ctx context.Context, userID, conversationID uuid.UUID, limit int) ([]Action, error) {
	return s.store.List(ctx, userID, Filter{
		Permission: string(tools.Write), ConversationID: &conversationID, Limit: limit,
	})
}

// clientMessage is the sentence stored in error_message and shown to the user.
//
// It is a fixed set of strings rather than err.Error(), for the reason
// documents.clientMessage gives: that column is read back over the API, and a
// wrapped driver error would put a connection string in an HTTP response. The
// full error goes to the log.
func clientMessage(err error) string {
	var verrs validate.Errors
	switch {
	case errors.As(err, &verrs):
		parts := make([]string, 0, len(verrs))
		for _, v := range verrs {
			parts = append(parts, v.Field+" "+v.Message)
		}
		return "The change was refused: " + strings.Join(parts, "; ") + "."
	case errors.Is(err, tasks.ErrNotFound):
		return "The task no longer exists, so it could not be changed."
	case errors.Is(err, goals.ErrNotFound), errors.Is(err, notes.ErrNotFound):
		return "The record it refers to no longer exists."
	case errors.Is(err, tools.ErrUnknownTool), errors.Is(err, tools.ErrNotApprovable):
		return "This version of Lunex can no longer carry out that action."
	case errors.Is(err, context.DeadlineExceeded):
		// Honest rather than reassuring: a statement that timed out may or may
		// not have committed.
		return "It took too long and was stopped. Check whether the change was made before asking again."
	default:
		return "The action could not be completed."
	}
}
