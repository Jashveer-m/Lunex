package graph

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

// Repository is the Postgres store for the knowledge graph. As everywhere else
// in this codebase, `user_id = $n` is part of the statement rather than a check
// applied after the row is already in memory -- and here it is part of every
// statement on both tables, so an edge can never join two users' nodes even if
// a caller passed the wrong id.
type Repository struct {
	db *sql.DB
}

func NewRepository(db *sql.DB) *Repository { return &Repository{db: db} }

const nodeColumns = `id, user_id, type, label, ref_table, ref_id, created_at, updated_at`

func scanNode(row interface{ Scan(...any) error }) (Node, error) {
	var n Node
	err := row.Scan(&n.ID, &n.UserID, &n.Type, &n.Label, &n.RefTable, &n.RefID,
		&n.CreatedAt, &n.UpdatedAt)
	return n, err
}

const edgeColumns = `id, user_id, from_node_id, to_node_id, relationship, confidence,
	source_conversation_id, created_at`

func scanEdge(row interface{ Scan(...any) error }) (Edge, error) {
	var e Edge
	err := row.Scan(&e.ID, &e.UserID, &e.FromNodeID, &e.ToNodeID, &e.Relationship,
		&e.Confidence, &e.SourceConversationID, &e.CreatedAt)
	return e, err
}

// --- nodes ------------------------------------------------------------------

// EnsureRefNode creates or refreshes the node mirroring one row of tasks,
// goals, notes or documents.
//
// It is an upsert on (user_id, ref_table, ref_id), which is what makes
// sync-on-write idempotent: the same task synced on create and again on every
// update produces one node whose label follows the title. The label is
// refreshed rather than left alone because a node whose label still says the
// old title is a node the mention scan matches on a name the user no longer
// uses -- and it would be shown to the model as the current one.
func (r *Repository) EnsureRefNode(ctx context.Context, userID uuid.UUID, refTable string, refID uuid.UUID, nodeType, label string) (Node, error) {
	n, err := scanNode(r.db.QueryRowContext(ctx, `
		INSERT INTO knowledge_nodes (user_id, type, label, ref_table, ref_id)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (user_id, ref_table, ref_id) WHERE ref_table IS NOT NULL
		DO UPDATE SET label = EXCLUDED.label, updated_at = now()
		RETURNING `+nodeColumns,
		userID, nodeType, label, refTable, refID))
	if err != nil {
		return Node{}, fmt.Errorf("upsert graph node for %s %s: %w", refTable, refID, err)
	}
	return n, nil
}

// EnsureExtractedNode creates or returns the node for a name a conversation
// used. It is an upsert on (user_id, type, lower(label)), so two turns
// extracted concurrently produce one node rather than racing to insert two.
//
// DO UPDATE rather than DO NOTHING because DO NOTHING returns no row on a
// conflict, and this call always has to hand back the node -- there is an edge
// waiting to be attached to it. The update sets the label to the incoming
// spelling, which is deliberate: the newest way the user wrote a name is the
// one to display.
func (r *Repository) EnsureExtractedNode(ctx context.Context, userID uuid.UUID, nodeType, label string) (Node, error) {
	n, err := scanNode(r.db.QueryRowContext(ctx, `
		INSERT INTO knowledge_nodes (user_id, type, label)
		VALUES ($1, $2, $3)
		ON CONFLICT (user_id, type, lower(label)) WHERE ref_table IS NULL
		DO UPDATE SET label = EXCLUDED.label, updated_at = now()
		RETURNING `+nodeColumns,
		userID, nodeType, label))
	if err != nil {
		return Node{}, fmt.Errorf("upsert graph node %q: %w", label, err)
	}
	return n, nil
}

// NodesByLabel returns every node of one user whose label matches, folded.
//
// Oldest first, so a caller with no better rule picks deterministically: the
// node that has been in the graph longest is the one the user's other edges
// already point at.
func (r *Repository) NodesByLabel(ctx context.Context, userID uuid.UUID, label string) ([]Node, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT `+nodeColumns+` FROM knowledge_nodes
		WHERE user_id = $1 AND lower(label) = lower($2)
		ORDER BY created_at ASC, id ASC`, userID, label)
	if err != nil {
		return nil, fmt.Errorf("select graph nodes by label: %w", err)
	}
	return collectNodes(rows)
}

func (r *Repository) NodeByID(ctx context.Context, userID, id uuid.UUID) (Node, error) {
	n, err := scanNode(r.db.QueryRowContext(ctx,
		`SELECT `+nodeColumns+` FROM knowledge_nodes WHERE id = $1 AND user_id = $2`, id, userID))
	if errors.Is(err, sql.ErrNoRows) {
		return Node{}, ErrNotFound
	}
	if err != nil {
		return Node{}, fmt.Errorf("select graph node: %w", err)
	}
	return n, nil
}

// Nodes lists one user's nodes, optionally of one type.
func (r *Repository) Nodes(ctx context.Context, userID uuid.UUID, f Filter) ([]Node, error) {
	where := []string{"user_id = $1"}
	args := []any{userID}
	if f.Type != "" {
		args = append(args, f.Type)
		where = append(where, fmt.Sprintf("type = $%d", len(args)))
	}
	args = append(args, f.Limit, f.Offset)

	rows, err := r.db.QueryContext(ctx, `
		SELECT `+nodeColumns+` FROM knowledge_nodes
		WHERE `+strings.Join(where, " AND ")+`
		ORDER BY type ASC, lower(label) ASC, id ASC
		LIMIT $`+strconv.Itoa(len(args)-1)+` OFFSET $`+strconv.Itoa(len(args)), args...)
	if err != nil {
		return nil, fmt.Errorf("select graph nodes: %w", err)
	}
	return collectNodes(rows)
}

// MentionCandidates returns the nodes whose label appears somewhere in text.
//
// The `position(...) > 0` test is a substring match, and a substring match is
// not what the caller wants -- "Go" occurs inside "going". It is a *prefilter*:
// it does in SQL the part that has to touch every row, and hands back the
// handful the word-boundary test in Mentions then rules on. Doing the boundary
// test in SQL would mean building a regular expression out of a user-supplied
// label, which is an injection into the pattern language rather than into the
// statement, and Postgres has no quoting function for it.
//
// The text is truncated by the caller; the label floor keeps one-character
// nodes from matching everything.
func (r *Repository) MentionCandidates(ctx context.Context, userID uuid.UUID, text string, limit int) ([]Node, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT `+nodeColumns+` FROM knowledge_nodes
		WHERE user_id = $1
		  AND char_length(label) >= $2
		  AND position(lower(label) in lower($3)) > 0
		ORDER BY char_length(label) DESC, created_at ASC, id ASC
		LIMIT $4`, userID, MinMentionLen, text, limit)
	if err != nil {
		return nil, fmt.Errorf("select mentioned graph nodes: %w", err)
	}
	return collectNodes(rows)
}

// DeleteNode removes one node and, through the foreign keys, its edges.
func (r *Repository) DeleteNode(ctx context.Context, userID, id uuid.UUID) error {
	res, err := r.db.ExecContext(ctx,
		`DELETE FROM knowledge_nodes WHERE id = $1 AND user_id = $2`, id, userID)
	return oneRow(res, err)
}

func collectNodes(rows *sql.Rows) ([]Node, error) {
	defer rows.Close()
	out := []Node{}
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, fmt.Errorf("scan graph node: %w", err)
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate graph nodes: %w", err)
	}
	return out, nil
}

// --- edges ------------------------------------------------------------------

// CreateEdge writes one extracted relationship.
//
// The same triple stated in two conversations is one edge, not two: the upsert
// on (user_id, from_node_id, to_node_id, relationship) keeps the higher
// confidence and leaves the original provenance in place. Keeping the first
// source_conversation_id rather than the latest is the honest choice -- the
// column records where the assistant first learned this, and overwriting it
// would make the earliest evidence unfindable.
func (r *Repository) CreateEdge(ctx context.Context, userID uuid.UUID, in EdgeInput) (Edge, error) {
	e, err := scanEdge(r.db.QueryRowContext(ctx, `
		INSERT INTO knowledge_edges (user_id, from_node_id, to_node_id, relationship,
		                             confidence, source_conversation_id)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (user_id, from_node_id, to_node_id, relationship)
		DO UPDATE SET confidence = GREATEST(knowledge_edges.confidence, EXCLUDED.confidence)
		RETURNING `+edgeColumns,
		userID, in.FromNodeID, in.ToNodeID, in.Relationship, in.Confidence,
		in.SourceConversationID))
	if err != nil {
		return Edge{}, fmt.Errorf("upsert graph edge: %w", err)
	}
	return e, nil
}

// EdgesAmong returns the edges whose *both* endpoints are in the given set.
//
// Both, not either: it backs the graph read, and a client drawing the result
// needs every edge to have two nodes it was also given. An edge to a node
// outside the page would be a dangling reference.
func (r *Repository) EdgesAmong(ctx context.Context, userID uuid.UUID, nodeIDs []uuid.UUID, limit int) ([]Edge, error) {
	if len(nodeIDs) == 0 {
		return []Edge{}, nil
	}
	ids := make([]string, len(nodeIDs))
	for i, id := range nodeIDs {
		ids[i] = id.String()
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT `+edgeColumns+` FROM knowledge_edges
		WHERE user_id = $1
		  AND from_node_id = ANY($2::uuid[])
		  AND to_node_id = ANY($2::uuid[])
		ORDER BY created_at ASC, id ASC
		LIMIT $3`, userID, ids, limit)
	if err != nil {
		return nil, fmt.Errorf("select graph edges: %w", err)
	}
	defer rows.Close()

	out := []Edge{}
	for rows.Next() {
		e, err := scanEdge(rows)
		if err != nil {
			return nil, fmt.Errorf("scan graph edge: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate graph edges: %w", err)
	}
	return out, nil
}

// Neighbors returns one hop out from a node, in both directions.
//
// One statement rather than two, joined to knowledge_nodes so the far end
// arrives with the edge: the caller wants "REQUIRES the backend project", and
// fetching the labels afterwards would be a query per neighbour. The join is
// on the node id *and* the same user_id, which is redundant given the edge is
// already owner-scoped and is left in anyway -- it is the statement that would
// have to be wrong twice for a cross-user row to appear.
//
// The CASE is the direction: `incoming` is true when the edge points at the
// node being asked about, which is the difference between "the user STUDIES
// Go" and a claim nobody made.
func (r *Repository) Neighbors(ctx context.Context, userID, nodeID uuid.UUID, limit int) ([]Neighbor, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT e.id, e.user_id, e.from_node_id, e.to_node_id, e.relationship, e.confidence,
		       e.source_conversation_id, e.created_at,
		       n.id, n.user_id, n.type, n.label, n.ref_table, n.ref_id, n.created_at, n.updated_at,
		       (e.to_node_id = $2) AS incoming
		FROM knowledge_edges e
		JOIN knowledge_nodes n
		  ON n.user_id = e.user_id
		 AND n.id = CASE WHEN e.from_node_id = $2 THEN e.to_node_id ELSE e.from_node_id END
		WHERE e.user_id = $1 AND ($2 IN (e.from_node_id, e.to_node_id))
		ORDER BY e.confidence DESC, e.created_at ASC, e.id ASC
		LIMIT $3`, userID, nodeID, limit)
	if err != nil {
		return nil, fmt.Errorf("select graph neighbours: %w", err)
	}
	defer rows.Close()

	out := []Neighbor{}
	for rows.Next() {
		var nb Neighbor
		if err := rows.Scan(
			&nb.Edge.ID, &nb.Edge.UserID, &nb.Edge.FromNodeID, &nb.Edge.ToNodeID,
			&nb.Edge.Relationship, &nb.Edge.Confidence, &nb.Edge.SourceConversationID,
			&nb.Edge.CreatedAt,
			&nb.Node.ID, &nb.Node.UserID, &nb.Node.Type, &nb.Node.Label,
			&nb.Node.RefTable, &nb.Node.RefID, &nb.Node.CreatedAt, &nb.Node.UpdatedAt,
			&nb.Incoming,
		); err != nil {
			return nil, fmt.Errorf("scan graph neighbour: %w", err)
		}
		out = append(out, nb)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate graph neighbours: %w", err)
	}
	return out, nil
}

func (r *Repository) DeleteEdge(ctx context.Context, userID, id uuid.UUID) error {
	res, err := r.db.ExecContext(ctx,
		`DELETE FROM knowledge_edges WHERE id = $1 AND user_id = $2`, id, userID)
	return oneRow(res, err)
}

// oneRow turns a delete that matched nothing into ErrNotFound, which is the
// same answer a row belonging to somebody else gets.
func oneRow(res sql.Result, err error) error {
	if err != nil {
		return fmt.Errorf("delete: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
