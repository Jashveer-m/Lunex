package actions

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/tools"
)

// Repository is the Postgres store for actions. As everywhere else, `user_id =
// $n` is part of every statement rather than a check applied afterwards -- and
// here it is also part of the approval itself, so an approve aimed at somebody
// else's action matches no row and runs nothing.
type Repository struct {
	db *sql.DB
}

func NewRepository(db *sql.DB) *Repository { return &Repository{db: db} }

// Repository is the registry's ledger: its Approve is the gate every write tool
// passes through.
var _ tools.Ledger = (*Repository)(nil)

const columns = `id, user_id, conversation_id, tool_name, input, permission_level, status,
	result, error_message, created_at, updated_at`

func scan(row interface{ Scan(...any) error }) (Action, error) {
	var a Action
	var input, result []byte
	var permission string
	if err := row.Scan(&a.ID, &a.UserID, &a.ConversationID, &a.ToolName, &input, &permission,
		&a.Status, &result, &a.ErrorMessage, &a.CreatedAt, &a.UpdatedAt); err != nil {
		return Action{}, err
	}
	a.Permission = tools.Permission(permission)
	a.Input = json.RawMessage(input)
	if len(result) > 0 {
		a.Result = json.RawMessage(result)
	}
	return a, nil
}

// Querier is what Insert writes through: the pool, or a transaction somebody
// else owns. *sql.DB and *sql.Tx both satisfy it.
type Querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Insert records new actions through q.
//
// It is a function over a Querier rather than only a method on the repository
// because the chat turn records its actions in the same transaction as its
// messages -- a failed turn persists nothing, proposals included -- and that
// transaction belongs to the chat repository. The SQL for this table still
// lives only here.
//
// Every action is checked against the state machine before any row is
// written, so a batch with one invalid action writes none.
func Insert(ctx context.Context, q Querier, userID uuid.UUID, conversationID *uuid.UUID, in []NewAction) ([]Action, error) {
	for _, n := range in {
		if err := n.Validate(); err != nil {
			return nil, err
		}
	}
	out := make([]Action, 0, len(in))
	for i, n := range in {
		var result, errMsg any
		if n.Result != nil {
			raw, err := json.Marshal(n.Result)
			if err != nil {
				return nil, fmt.Errorf("encode action %d result: %w", i, err)
			}
			// Text with an explicit cast, as with messages.sources: the pgx
			// stdlib driver sends []byte as bytea, which jsonb will not take.
			result = string(raw)
		}
		if n.ErrorMessage != "" {
			errMsg = n.ErrorMessage
		}
		a, err := scan(q.QueryRowContext(ctx, `
			INSERT INTO actions (user_id, conversation_id, tool_name, input, permission_level,
			                     status, result, error_message, created_at, updated_at)
			VALUES ($1, $2, $3, $4::jsonb, $5, $6, $7::jsonb, $8, clock_timestamp(), clock_timestamp())
			RETURNING `+columns,
			userID, conversationID, n.Call.Tool, string(n.Call.Input), string(n.Call.Permission),
			n.Status, result, errMsg))
		if err != nil {
			return nil, fmt.Errorf("insert action %d: %w", i, err)
		}
		out = append(out, a)
	}
	return out, nil
}

// Record is Insert on the repository's own pool, for a caller with no
// transaction to join.
func (r *Repository) Record(ctx context.Context, userID uuid.UUID, conversationID *uuid.UUID, in ...NewAction) ([]Action, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin record: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once Commit has run
	out, err := Insert(ctx, tx, userID, conversationID, in)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit record: %w", err)
	}
	return out, nil
}

func (r *Repository) ByID(ctx context.Context, userID, id uuid.UUID) (Action, error) {
	a, err := scan(r.db.QueryRowContext(ctx,
		`SELECT `+columns+` FROM actions WHERE id = $1 AND user_id = $2`, id, userID))
	if errors.Is(err, sql.ErrNoRows) {
		return Action{}, ErrNotFound
	}
	if err != nil {
		return Action{}, fmt.Errorf("select action: %w", err)
	}
	return a, nil
}

// List returns the owner's actions, newest first.
func (r *Repository) List(ctx context.Context, userID uuid.UUID, f Filter) ([]Action, error) {
	where := []string{"user_id = $1"}
	args := []any{userID}
	add := func(clause string, arg any) {
		args = append(args, arg)
		where = append(where, fmt.Sprintf(clause, len(args)))
	}
	if f.Status != "" {
		add("status = $%d", f.Status)
	}
	if f.Permission != "" {
		add("permission_level = $%d", f.Permission)
	}
	if f.ConversationID != nil {
		add("conversation_id = $%d", *f.ConversationID)
	}
	args = append(args, f.Limit, f.Offset)
	rows, err := r.db.QueryContext(ctx, `
		SELECT `+columns+` FROM actions
		WHERE `+strings.Join(where, " AND ")+`
		ORDER BY created_at DESC, id DESC
		LIMIT $`+strconv.Itoa(len(args)-1)+` OFFSET $`+strconv.Itoa(len(args)), args...)
	if err != nil {
		return nil, fmt.Errorf("select actions: %w", err)
	}
	defer rows.Close()
	out := []Action{}
	for rows.Next() {
		a, err := scan(rows)
		if err != nil {
			return nil, fmt.Errorf("scan action: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate actions: %w", err)
	}
	return out, nil
}

// Approve is the ledger's gate: it moves one proposed write to approved and
// returns what it approved.
//
// It is a single conditional UPDATE, which is what makes it single-use. Two
// requests approving the same action at once both run this statement; Postgres
// row-locks the first, and the second re-evaluates `status = 'proposed'`
// against the committed row, finds it false, and matches nothing. Exactly one
// of them gets a row back, and only the one that gets a row back runs the
// tool.
//
// `permission_level = 'write'` is in the WHERE clause as well as implied by the
// CHECK constraint that a read is never proposed: the gate should not depend
// on a constraint defined somewhere else to be right.
func (r *Repository) Approve(ctx context.Context, userID, id uuid.UUID) (tools.Approved, error) {
	var a tools.Approved
	var input []byte
	err := r.db.QueryRowContext(ctx, `
		UPDATE actions SET status = 'approved'
		WHERE id = $1 AND user_id = $2 AND status = 'proposed' AND permission_level = 'write'
		RETURNING id, tool_name, input`, id, userID).Scan(&a.ActionID, &a.Tool, &input)
	if errors.Is(err, sql.ErrNoRows) {
		return tools.Approved{}, r.whyNot(ctx, userID, id)
	}
	if err != nil {
		return tools.Approved{}, fmt.Errorf("approve action: %w", err)
	}
	a.Input = json.RawMessage(input)
	return a, nil
}

// Reject moves one proposed write to rejected. Nothing runs, and nothing ever
// will: rejected is final.
func (r *Repository) Reject(ctx context.Context, userID, id uuid.UUID) (Action, error) {
	a, err := scan(r.db.QueryRowContext(ctx, `
		UPDATE actions SET status = 'rejected'
		WHERE id = $1 AND user_id = $2 AND status = 'proposed'
		RETURNING `+columns, id, userID))
	if errors.Is(err, sql.ErrNoRows) {
		return Action{}, r.whyNot(ctx, userID, id)
	}
	if err != nil {
		return Action{}, fmt.Errorf("reject action: %w", err)
	}
	return a, nil
}

// Outcome is how an approved action ended: a result, or a reason it failed.
type Outcome struct {
	Result any
	Failed string
}

// Finish records the outcome of an approved action. It only moves an action
// that is approved, so an outcome can be recorded once.
func (r *Repository) Finish(ctx context.Context, userID, id uuid.UUID, o Outcome) (Action, error) {
	var query string
	var arg any
	if o.Failed != "" {
		query = `UPDATE actions SET status = 'failed', error_message = $3
			WHERE id = $1 AND user_id = $2 AND status = 'approved' RETURNING ` + columns
		arg = o.Failed
	} else {
		raw, err := json.Marshal(o.Result)
		if err != nil {
			return Action{}, fmt.Errorf("encode action result: %w", err)
		}
		query = `UPDATE actions SET status = 'executed', result = $3::jsonb
			WHERE id = $1 AND user_id = $2 AND status = 'approved' RETURNING ` + columns
		arg = string(raw)
	}
	a, err := scan(r.db.QueryRowContext(ctx, query, id, userID, arg))
	if errors.Is(err, sql.ErrNoRows) {
		return Action{}, r.whyNot(ctx, userID, id)
	}
	if err != nil {
		return Action{}, fmt.Errorf("finish action: %w", err)
	}
	return a, nil
}

// PendingDuplicate returns an action in this conversation that already proposes
// exactly this call and is still waiting, if there is one.
//
// It is what stops a repeated request from queueing two identical proposals
// the user could then approve twice. The comparison is jsonb equality on the
// canonical input, which is key-order-insensitive: two proposals are the same
// when they would do the same thing.
func (r *Repository) PendingDuplicate(ctx context.Context, userID, conversationID uuid.UUID, call tools.Call) (Action, bool, error) {
	a, err := scan(r.db.QueryRowContext(ctx, `
		SELECT `+columns+` FROM actions
		WHERE user_id = $1 AND conversation_id = $2 AND status = 'proposed'
		  AND tool_name = $3 AND input = $4::jsonb
		ORDER BY created_at DESC LIMIT 1`,
		userID, conversationID, call.Tool, string(call.Input)))
	if errors.Is(err, sql.ErrNoRows) {
		return Action{}, false, nil
	}
	if err != nil {
		return Action{}, false, fmt.Errorf("select pending duplicate: %w", err)
	}
	return a, true, nil
}

// whyNot tells a missing action from one that exists but is not pending, after
// a conditional UPDATE matched nothing. Somebody else's action is missing: the
// lookup is owner-scoped like everything else.
func (r *Repository) whyNot(ctx context.Context, userID, id uuid.UUID) error {
	var status string
	err := r.db.QueryRowContext(ctx,
		`SELECT status FROM actions WHERE id = $1 AND user_id = $2`, id, userID).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("select action status: %w", err)
	}
	return fmt.Errorf("%w: it is %s", ErrNotPending, status)
}
