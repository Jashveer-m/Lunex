package chat

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/actions"
)

// Repository is the Postgres store for conversations and messages.
//
// Note where ownership lives. conversations carries user_id and every
// statement against it names the caller. messages does not -- the brief's
// schema hangs it off the conversation alone -- so every message statement
// joins conversations and filters on user_id there. The scoping is still in
// the WHERE clause rather than in a check after the fact, which is the
// property the whole design rests on; it just costs one join.
type Repository struct {
	db *sql.DB
}

func NewRepository(db *sql.DB) *Repository { return &Repository{db: db} }

// conversationColumns is the shared SELECT/RETURNING list. message_count is
// counted on read, so it can never disagree with the rows in messages.
const conversationColumns = `conversations.id, conversations.user_id, conversations.title,
	conversations.created_at, conversations.updated_at,
	(SELECT count(*) FROM messages WHERE messages.conversation_id = conversations.id) AS message_count`

func scanConversation(row interface{ Scan(...any) error }) (Conversation, error) {
	var c Conversation
	err := row.Scan(&c.ID, &c.UserID, &c.Title, &c.CreatedAt, &c.UpdatedAt, &c.MessageCount)
	return c, err
}

func (r *Repository) CreateConversation(ctx context.Context, userID uuid.UUID, title string) (Conversation, error) {
	c, err := scanConversation(r.db.QueryRowContext(ctx, `
		INSERT INTO conversations (user_id, title) VALUES ($1, $2)
		RETURNING `+conversationColumns, userID, title))
	if err != nil {
		return Conversation{}, fmt.Errorf("insert conversation: %w", err)
	}
	return c, nil
}

func (r *Repository) ConversationByID(ctx context.Context, userID, id uuid.UUID) (Conversation, error) {
	c, err := scanConversation(r.db.QueryRowContext(ctx, `
		SELECT `+conversationColumns+` FROM conversations
		WHERE conversations.id = $1 AND conversations.user_id = $2`, id, userID))
	if errors.Is(err, sql.ErrNoRows) {
		return Conversation{}, ErrNotFound
	}
	if err != nil {
		return Conversation{}, fmt.Errorf("select conversation: %w", err)
	}
	return c, nil
}

func (r *Repository) ListConversations(ctx context.Context, userID uuid.UUID, f Filter) ([]Conversation, error) {
	// Sorts is a fixed map; no request text reaches the ORDER BY.
	rows, err := r.db.QueryContext(ctx, `
		SELECT `+conversationColumns+` FROM conversations
		WHERE conversations.user_id = $1
		ORDER BY `+Sorts[f.Sort]+`, id ASC
		LIMIT $2 OFFSET $3`, userID, f.Limit, f.Offset)
	if err != nil {
		return nil, fmt.Errorf("select conversations: %w", err)
	}
	defer rows.Close()

	out := []Conversation{}
	for rows.Next() {
		c, err := scanConversation(rows)
		if err != nil {
			return nil, fmt.Errorf("scan conversation: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate conversations: %w", err)
	}
	return out, nil
}

// DeleteConversation removes the conversation; messages cascade from it.
func (r *Repository) DeleteConversation(ctx context.Context, userID, id uuid.UUID) error {
	res, err := r.db.ExecContext(ctx,
		`DELETE FROM conversations WHERE id = $1 AND user_id = $2`, id, userID)
	if err != nil {
		return fmt.Errorf("delete conversation: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete conversation: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// Messages returns the most recent `limit` messages, oldest first.
//
// The window is taken from the end rather than the start because both callers
// want the same thing: the conversation as it stands now. Reversing in SQL
// keeps the LIMIT on the indexed descending scan instead of reading every row
// of a long conversation to discard the front of it.
//
// A conversation that does not exist, or is somebody else's, returns no rows
// rather than ErrNotFound. Both callers establish ownership first; this method
// is the one that must not leak, and returning nothing is how it does not.
func (r *Repository) Messages(ctx context.Context, userID, convID uuid.UUID, limit int) ([]Message, error) {
	if limit <= 0 || limit > MaxMessagesPerRead {
		limit = MaxMessagesPerRead
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, conversation_id, role, content, sources, created_at FROM (
			SELECT m.id, m.conversation_id, m.role, m.content, m.sources, m.created_at
			FROM messages m
			JOIN conversations c ON c.id = m.conversation_id
			WHERE m.conversation_id = $1 AND c.user_id = $2
			ORDER BY m.created_at DESC, m.id DESC
			LIMIT $3
		) recent
		ORDER BY created_at ASC, id ASC`, convID, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("select messages: %w", err)
	}
	defer rows.Close()

	out := []Message{}
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate messages: %w", err)
	}
	return out, nil
}

func scanMessage(row interface{ Scan(...any) error }) (Message, error) {
	var m Message
	var raw []byte
	if err := row.Scan(&m.ID, &m.ConversationID, &m.Role, &m.Content, &raw, &m.CreatedAt); err != nil {
		return Message{}, fmt.Errorf("scan message: %w", err)
	}
	// NULL sources stays nil: "retrieval found nothing" and "this message was
	// never grounded" are different facts and the column keeps them apart.
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &m.Sources); err != nil {
			return Message{}, fmt.Errorf("decode message sources: %w", err)
		}
	}
	return m, nil
}

// AppendTurn writes a whole turn and names the conversation, in one
// transaction -- the turn's actions included, through actions.Insert, so the
// SQL for that table still lives in its own package while the commit that
// makes a proposal real is the same one that makes the answer describing it
// real.
//
// The leading UPDATE does three jobs at once: it proves the conversation is
// the caller's (no rows means it is not), it applies the derived title if the
// conversation is still using the default, and it moves updated_at through the
// set_updated_at trigger so the conversation list sorts by real activity --
// inserting a message would not otherwise touch the parent row.
func (r *Repository) AppendTurn(ctx context.Context, userID, convID uuid.UUID, msgs []NewMessage, titleIfDefault string, acts []actions.NewAction) ([]Message, []actions.Action, error) {
	if len(msgs) == 0 && len(acts) == 0 {
		return nil, nil, nil
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("begin turn: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once Commit has run

	var id uuid.UUID
	err = tx.QueryRowContext(ctx, `
		UPDATE conversations
		SET title = CASE WHEN title = $3 AND $4 <> '' THEN $4 ELSE title END
		WHERE id = $1 AND user_id = $2
		RETURNING id`, convID, userID, DefaultTitle, titleIfDefault).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, fmt.Errorf("touch conversation: %w", err)
	}

	out := make([]Message, 0, len(msgs))
	for i, m := range msgs {
		var sources any
		if len(m.Sources) > 0 {
			raw, err := json.Marshal(m.Sources)
			if err != nil {
				return nil, nil, fmt.Errorf("encode message sources: %w", err)
			}
			// Passed as text with an explicit cast: the pgx stdlib driver
			// sends a []byte as bytea, which jsonb will not take.
			sources = string(raw)
		}

		// clock_timestamp(), not the column's now() default. now() is the
		// transaction's start time, so both messages of a turn would carry the
		// identical timestamp and their read order would fall to the tie-break
		// on a random uuid -- which could put the answer before the question.
		// clock_timestamp() reads the wall clock per statement.
		written, err := scanMessage(tx.QueryRowContext(ctx, `
			INSERT INTO messages (conversation_id, role, content, sources, created_at)
			VALUES ($1, $2, $3, $4::jsonb, clock_timestamp())
			RETURNING id, conversation_id, role, content, sources, created_at`,
			convID, m.Role, m.Content, sources))
		if err != nil {
			return nil, nil, fmt.Errorf("insert message %d: %w", i, err)
		}
		out = append(out, written)
	}

	var recorded []actions.Action
	if len(acts) > 0 {
		if recorded, err = actions.Insert(ctx, tx, userID, &convID, acts); err != nil {
			return nil, nil, fmt.Errorf("record turn actions: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, nil, fmt.Errorf("commit turn: %w", err)
	}
	return out, recorded, nil
}
