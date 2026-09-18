package db_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/chat"
	"github.com/jashveer/lifeos/backend/internal/graph"
	"github.com/jashveer/lifeos/backend/internal/tasks"
)

// These cover the Phase 6 SQL: the two tables and their constraints, the
// upserts that make sync and extraction idempotent, the 1-hop query in both
// directions, the mention prefilter, and the cascades -- including the trigger
// that removes a node when the row it mirrors goes away, which is the one
// piece of this phase that has no equivalent in Go.

func seedRefNode(t *testing.T, repo *graph.Repository, owner uuid.UUID, table string, refID uuid.UUID, kind, label string) graph.Node {
	t.Helper()
	n, err := repo.EnsureRefNode(context.Background(), owner, table, refID, kind, label)
	if err != nil {
		t.Fatalf("ensure ref node %q: %v", label, err)
	}
	return n
}

func seedNode(t *testing.T, repo *graph.Repository, owner uuid.UUID, kind, label string) graph.Node {
	t.Helper()
	n, err := repo.EnsureExtractedNode(context.Background(), owner, kind, label)
	if err != nil {
		t.Fatalf("ensure node %q: %v", label, err)
	}
	return n
}

func seedEdge(t *testing.T, repo *graph.Repository, owner uuid.UUID, from, to graph.Node, rel string, confidence float64, conv *uuid.UUID) graph.Edge {
	t.Helper()
	e, err := repo.CreateEdge(context.Background(), owner, graph.EdgeInput{
		FromNodeID: from.ID, ToNodeID: to.ID, Relationship: rel,
		Confidence: confidence, SourceConversationID: conv,
	})
	if err != nil {
		t.Fatalf("create edge: %v", err)
	}
	return e
}

func TestGraphRoundTrip(t *testing.T) {
	pool := testDB(t)
	repo := graph.NewRepository(pool)
	ctx := context.Background()
	alice := makeUser(t, pool, "alice@example.com")
	conv := seedConversation(t, chat.NewRepository(pool), alice, "About Go")

	self := seedNode(t, repo, alice, graph.NodePerson, graph.SelfLabel)
	goSkill := seedNode(t, repo, alice, graph.NodeSkill, "Go")
	e := seedEdge(t, repo, alice, self, goSkill, graph.RelStudies, 0.9, &conv.ID)

	switch {
	case goSkill.UserID != alice:
		t.Fatalf("owner = %v, want %v", goSkill.UserID, alice)
	case goSkill.Type != graph.NodeSkill || goSkill.Label != "Go":
		t.Fatalf("node = %+v", goSkill)
	case !goSkill.Extracted():
		t.Fatal("a node with no ref reports itself as mirrored")
	case goSkill.RefTable != nil || goSkill.RefID != nil:
		t.Fatalf("ref = %v/%v, want NULL", goSkill.RefTable, goSkill.RefID)
	}
	switch {
	case e.Relationship != graph.RelStudies:
		t.Fatalf("relationship = %q", e.Relationship)
	case e.SourceConversationID == nil || *e.SourceConversationID != conv.ID:
		t.Fatalf("source = %v, want the conversation it came from", e.SourceConversationID)
	}
	// `real` is single precision, so it comes back near rather than exactly
	// equal. The API rounds it; the column keeps what it was given.
	if diff := e.Confidence - 0.9; diff > 1e-6 || diff < -1e-6 {
		t.Fatalf("confidence = %v, want ~0.9", e.Confidence)
	}

	read, err := repo.NodeByID(ctx, alice, goSkill.ID)
	if err != nil || read.ID != goSkill.ID {
		t.Fatalf("NodeByID: %+v, %v", read, err)
	}
}

// The two unique indexes, which are what make "one node per row" and "Go
// mentioned twice is one node" properties of the database rather than of a
// lucky interleaving.
func TestGraphNodeUpsertsAreIdempotent(t *testing.T) {
	pool := testDB(t)
	repo := graph.NewRepository(pool)
	alice := makeUser(t, pool, "alice@example.com")
	taskID := uuid.New()

	first := seedRefNode(t, repo, alice, "tasks", taskID, graph.NodeTask, "Write the scheduler")
	again := seedRefNode(t, repo, alice, "tasks", taskID, graph.NodeTask, "Write the CFS scheduler")
	if first.ID != again.ID {
		t.Fatal("syncing the same task twice made two nodes")
	}
	if again.Label != "Write the CFS scheduler" {
		t.Fatalf("label = %q; the upsert did not follow the title", again.Label)
	}
	if !again.UpdatedAt.After(first.UpdatedAt) {
		t.Fatal("the set_updated_at trigger did not fire on the upsert")
	}

	// The extracted index folds case, which is the duplication that actually
	// happens: the same model writes "Go" then "go".
	up := seedNode(t, repo, alice, graph.NodeSkill, "Go")
	down := seedNode(t, repo, alice, graph.NodeSkill, "go")
	if up.ID != down.ID {
		t.Fatal(`"Go" and "go" became two nodes`)
	}

	// A different *type* with the same label is a different thing, though.
	person := seedNode(t, repo, alice, graph.NodePerson, "Go")
	if person.ID == up.ID {
		t.Fatal("a person and a skill named Go were folded into one node")
	}

	var n int
	if err := pool.QueryRow(`SELECT count(*) FROM knowledge_nodes WHERE user_id = $1`, alice).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("the table holds %d nodes, want 3", n)
	}
}

// The same relationship stated in two conversations is one edge, with the
// better confidence kept and the original provenance left in place.
func TestGraphEdgeUpsertKeepsTheHigherConfidenceAndTheFirstSource(t *testing.T) {
	pool := testDB(t)
	repo := graph.NewRepository(pool)
	alice := makeUser(t, pool, "alice@example.com")
	convRepo := chat.NewRepository(pool)
	first := seedConversation(t, convRepo, alice, "first")
	second := seedConversation(t, convRepo, alice, "second")

	self := seedNode(t, repo, alice, graph.NodePerson, graph.SelfLabel)
	goSkill := seedNode(t, repo, alice, graph.NodeSkill, "Go")

	a := seedEdge(t, repo, alice, self, goSkill, graph.RelStudies, 0.7, &first.ID)
	b := seedEdge(t, repo, alice, self, goSkill, graph.RelStudies, 0.95, &second.ID)
	if a.ID != b.ID {
		t.Fatal("restating a relationship made a second edge")
	}
	if diff := b.Confidence - 0.95; diff > 1e-6 || diff < -1e-6 {
		t.Fatalf("confidence = %v, want the restatement's higher 0.95", b.Confidence)
	}
	if b.SourceConversationID == nil || *b.SourceConversationID != first.ID {
		t.Fatal("restating overwrote where the relationship was first learned")
	}

	// A lower-confidence restatement does not lower it back.
	c := seedEdge(t, repo, alice, self, goSkill, graph.RelStudies, 0.5, &second.ID)
	if diff := c.Confidence - 0.95; diff > 1e-6 || diff < -1e-6 {
		t.Fatalf("confidence = %v; a weaker restatement lowered it", c.Confidence)
	}

	// A different relationship between the same pair is a different edge.
	d := seedEdge(t, repo, alice, self, goSkill, graph.RelKnows, 0.8, &first.ID)
	if d.ID == a.ID {
		t.Fatal("STUDIES and KNOWS between the same pair were folded into one edge")
	}
}

// The CHECK constraints, each pinned separately: they are the difference
// between a typo in the Go code being a write error and being a row no client
// knows how to render.
func TestGraphConstraints(t *testing.T) {
	pool := testDB(t)
	repo := graph.NewRepository(pool)
	alice := makeUser(t, pool, "alice@example.com")
	self := seedNode(t, repo, alice, graph.NodePerson, graph.SelfLabel)
	goSkill := seedNode(t, repo, alice, graph.NodeSkill, "Go")

	t.Run("an unknown node type is rejected", func(t *testing.T) {
		if _, err := pool.Exec(
			`INSERT INTO knowledge_nodes (user_id, type, label) VALUES ($1, 'invoice', 'coffee')`, alice); err == nil {
			t.Fatal("a node type outside the allow-list was stored")
		}
	})

	t.Run("half a reference is rejected", func(t *testing.T) {
		if _, err := pool.Exec(
			`INSERT INTO knowledge_nodes (user_id, type, label, ref_table) VALUES ($1, 'task', 't', 'tasks')`, alice); err == nil {
			t.Fatal("a node with a ref_table and no ref_id was stored")
		}
		if _, err := pool.Exec(
			`INSERT INTO knowledge_nodes (user_id, type, label, ref_id) VALUES ($1, 'task', 't', gen_random_uuid())`, alice); err == nil {
			t.Fatal("a node with a ref_id and no ref_table was stored")
		}
	})

	t.Run("an unknown ref_table is rejected", func(t *testing.T) {
		if _, err := pool.Exec(
			`INSERT INTO knowledge_nodes (user_id, type, label, ref_table, ref_id)
			 VALUES ($1, 'task', 't', 'receipts', gen_random_uuid())`, alice); err == nil {
			t.Fatal("a ref_table outside the allow-list was stored")
		}
	})

	t.Run("an unknown relationship is rejected", func(t *testing.T) {
		if _, err := pool.Exec(
			`INSERT INTO knowledge_edges (user_id, from_node_id, to_node_id, relationship)
			 VALUES ($1, $2, $3, 'SPENT_ON')`, alice, self.ID, goSkill.ID); err == nil {
			t.Fatal("a relationship outside the allow-list was stored")
		}
	})

	t.Run("a confidence outside [0,1] is rejected", func(t *testing.T) {
		if _, err := pool.Exec(
			`INSERT INTO knowledge_edges (user_id, from_node_id, to_node_id, relationship, confidence)
			 VALUES ($1, $2, $3, 'KNOWS', 1.5)`, alice, self.ID, goSkill.ID); err == nil {
			t.Fatal("a confidence of 1.5 was stored")
		}
	})

	t.Run("a self edge is rejected", func(t *testing.T) {
		if _, err := pool.Exec(
			`INSERT INTO knowledge_edges (user_id, from_node_id, to_node_id, relationship)
			 VALUES ($1, $2, $2, 'RELATED_TO')`, alice, self.ID); err == nil {
			t.Fatal("a node related to itself was stored")
		}
	})
}

// The 1-hop query, in both directions and with the far end joined in. This is
// what the chat lookup and the node read both run.
func TestGraphNeighborsAnswerInBothDirections(t *testing.T) {
	pool := testDB(t)
	repo := graph.NewRepository(pool)
	ctx := context.Background()
	alice := makeUser(t, pool, "alice@example.com")

	self := seedNode(t, repo, alice, graph.NodePerson, graph.SelfLabel)
	goSkill := seedNode(t, repo, alice, graph.NodeSkill, "Go")
	project := seedNode(t, repo, alice, graph.NodeProject, "the backend project")
	unrelated := seedNode(t, repo, alice, graph.NodeSkill, "Rust")

	seedEdge(t, repo, alice, self, goSkill, graph.RelStudies, 0.9, nil)
	seedEdge(t, repo, alice, project, goSkill, graph.RelRequires, 0.85, nil)
	seedEdge(t, repo, alice, self, unrelated, graph.RelInterestedIn, 0.6, nil)

	found, err := repo.Neighbors(ctx, alice, goSkill.ID, graph.MaxNeighbors)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 2 {
		t.Fatalf("Go has %d neighbours, want 2: %+v", len(found), found)
	}
	// Ordered by confidence, so the strongest link is first.
	if found[0].Node.Label != graph.SelfLabel || found[1].Node.Label != "the backend project" {
		t.Fatalf("neighbours are %q then %q, want the ordering by confidence",
			found[0].Node.Label, found[1].Node.Label)
	}
	for _, nb := range found {
		if !nb.Incoming {
			t.Fatalf("%q is reported as outgoing from Go; both edges point at Go", nb.Node.Label)
		}
		if nb.Node.Type == "" || nb.Node.UserID != alice {
			t.Fatalf("the far end came back unpopulated: %+v", nb.Node)
		}
	}

	// From the other end the same edge is outgoing.
	fromSelf, err := repo.Neighbors(ctx, alice, self.ID, graph.MaxNeighbors)
	if err != nil {
		t.Fatal(err)
	}
	if len(fromSelf) != 2 {
		t.Fatalf("the self node has %d neighbours, want 2", len(fromSelf))
	}
	for _, nb := range fromSelf {
		if nb.Incoming {
			t.Fatalf("%q is reported as incoming to the self node", nb.Node.Label)
		}
	}
}

// The graph read returns a closed subgraph: every edge has both endpoints in
// the node list it came with.
func TestGraphEdgesAmongIsClosedOverItsNodes(t *testing.T) {
	pool := testDB(t)
	repo := graph.NewRepository(pool)
	ctx := context.Background()
	alice := makeUser(t, pool, "alice@example.com")

	self := seedNode(t, repo, alice, graph.NodePerson, graph.SelfLabel)
	goSkill := seedNode(t, repo, alice, graph.NodeSkill, "Go")
	seedEdge(t, repo, alice, self, goSkill, graph.RelStudies, 0.9, nil)

	both, err := repo.EdgesAmong(ctx, alice, []uuid.UUID{self.ID, goSkill.ID}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(both) != 1 {
		t.Fatalf("found %d edges, want 1", len(both))
	}
	one, err := repo.EdgesAmong(ctx, alice, []uuid.UUID{goSkill.ID}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(one) != 0 {
		t.Fatalf("an edge with one endpoint outside the set was returned: %+v", one)
	}
	if empty, err := repo.EdgesAmong(ctx, alice, nil, 100); err != nil || len(empty) != 0 {
		t.Fatalf("EdgesAmong(nil) = %+v, %v", empty, err)
	}
}

// The mention prefilter over-collects on purpose -- the word-boundary test is
// applied in Go -- but it must collect the right candidates and no others.
func TestGraphMentionCandidatesPrefilter(t *testing.T) {
	pool := testDB(t)
	repo := graph.NewRepository(pool)
	ctx := context.Background()
	alice := makeUser(t, pool, "alice@example.com")

	seedNode(t, repo, alice, graph.NodeSkill, "Go")
	seedNode(t, repo, alice, graph.NodeProject, "the backend project")
	seedNode(t, repo, alice, graph.NodeSkill, "Haskell")

	found, err := repo.MentionCandidates(ctx, alice, "how is GO going on the Backend Project?", 50)
	if err != nil {
		t.Fatal(err)
	}
	labels := make([]string, len(found))
	for i, n := range found {
		labels[i] = n.Label
	}
	// Case-insensitive, and longest label first so the more specific node wins
	// a limited budget.
	if strings.Join(labels, ",") != "the backend project,Go" {
		t.Fatalf("candidates = %v, want the two mentioned labels, longest first", labels)
	}
}

// The trigger that stands in for a foreign key the schema cannot have. This is
// the whole reason node removal is done in SQL rather than from the resource
// service: a subtask is deleted by a cascade nothing in Go ever sees.
func TestDeletingAResourceRemovesItsNodeAndEdges(t *testing.T) {
	pool := testDB(t)
	repo := graph.NewRepository(pool)
	taskRepo := tasks.NewRepository(pool)
	ctx := context.Background()
	alice := makeUser(t, pool, "alice@example.com")

	parent, err := taskRepo.Create(ctx, alice, tasks.CreateInput{
		Title: "Ship the backend", Priority: "high", Status: "pending",
	})
	if err != nil {
		t.Fatal(err)
	}
	child, err := taskRepo.Create(ctx, alice, tasks.CreateInput{
		Title: "Write the scheduler", Priority: "high", Status: "pending", ParentTaskID: &parent.ID,
	})
	if err != nil {
		t.Fatal(err)
	}

	parentNode := seedRefNode(t, repo, alice, "tasks", parent.ID, graph.NodeTask, parent.Title)
	childNode := seedRefNode(t, repo, alice, "tasks", child.ID, graph.NodeTask, child.Title)
	goSkill := seedNode(t, repo, alice, graph.NodeSkill, "Go")
	seedEdge(t, repo, alice, childNode, goSkill, graph.RelRequires, 0.9, nil)

	// Deleting the parent cascades to the subtask in SQL. Both nodes have to
	// go, and the edge with them -- and nothing in Go is on that path.
	if err := taskRepo.Delete(ctx, alice, parent.ID); err != nil {
		t.Fatal(err)
	}

	for _, n := range []graph.Node{parentNode, childNode} {
		if _, err := repo.NodeByID(ctx, alice, n.ID); !errors.Is(err, graph.ErrNotFound) {
			t.Fatalf("the node for %q survived its task: %v", n.Label, err)
		}
	}
	var edges int
	if err := pool.QueryRow(`SELECT count(*) FROM knowledge_edges WHERE user_id = $1`, alice).Scan(&edges); err != nil {
		t.Fatal(err)
	}
	if edges != 0 {
		t.Fatalf("%d edges survived their node", edges)
	}
	// The skill node is not a mirror of anything and stays.
	if _, err := repo.NodeByID(ctx, alice, goSkill.ID); err != nil {
		t.Fatalf("an extracted node was removed with the task: %v", err)
	}
}

// Deleting the conversation an edge came from keeps the edge and forgets its
// provenance, matching what memories do in Phase 5.
func TestDeletingAConversationNullsTheEdgeSource(t *testing.T) {
	pool := testDB(t)
	repo := graph.NewRepository(pool)
	ctx := context.Background()
	alice := makeUser(t, pool, "alice@example.com")
	convRepo := chat.NewRepository(pool)
	conv := seedConversation(t, convRepo, alice, "About Go")

	self := seedNode(t, repo, alice, graph.NodePerson, graph.SelfLabel)
	goSkill := seedNode(t, repo, alice, graph.NodeSkill, "Go")
	seedEdge(t, repo, alice, self, goSkill, graph.RelStudies, 0.9, &conv.ID)

	if err := convRepo.DeleteConversation(ctx, alice, conv.ID); err != nil {
		t.Fatal(err)
	}
	found, err := repo.Neighbors(ctx, alice, goSkill.ID, graph.MaxNeighbors)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 {
		t.Fatalf("the edge went with its conversation: %+v", found)
	}
	if found[0].Edge.SourceConversationID != nil {
		t.Fatalf("source = %v, want NULL", found[0].Edge.SourceConversationID)
	}
}

// Deleting a node takes its edges through a foreign key that does exist.
func TestDeletingANodeCascadesToItsEdges(t *testing.T) {
	pool := testDB(t)
	repo := graph.NewRepository(pool)
	ctx := context.Background()
	alice := makeUser(t, pool, "alice@example.com")

	self := seedNode(t, repo, alice, graph.NodePerson, graph.SelfLabel)
	goSkill := seedNode(t, repo, alice, graph.NodeSkill, "Go")
	seedEdge(t, repo, alice, self, goSkill, graph.RelStudies, 0.9, nil)

	if err := repo.DeleteNode(ctx, alice, goSkill.ID); err != nil {
		t.Fatal(err)
	}
	var edges int
	if err := pool.QueryRow(`SELECT count(*) FROM knowledge_edges`).Scan(&edges); err != nil {
		t.Fatal(err)
	}
	if edges != 0 {
		t.Fatalf("%d edges survived their node", edges)
	}
	if _, err := repo.NodeByID(ctx, alice, self.ID); err != nil {
		t.Fatalf("the other end of the edge was removed too: %v", err)
	}
	if err := repo.DeleteNode(ctx, alice, goSkill.ID); !errors.Is(err, graph.ErrNotFound) {
		t.Fatalf("deleting a gone node returned %v", err)
	}
}

// The property every table in this codebase has: one user cannot reach
// another's rows, and the answer is "not found" rather than "forbidden".
func TestGraphIsolatesUsers(t *testing.T) {
	pool := testDB(t)
	repo := graph.NewRepository(pool)
	ctx := context.Background()
	alice := makeUser(t, pool, "alice@example.com")
	bob := makeUser(t, pool, "bob@example.com")

	self := seedNode(t, repo, alice, graph.NodePerson, graph.SelfLabel)
	goSkill := seedNode(t, repo, alice, graph.NodeSkill, "Go")
	e := seedEdge(t, repo, alice, self, goSkill, graph.RelStudies, 0.9, nil)

	// Bob has a node with the same label; it is a different node.
	bobGo := seedNode(t, repo, bob, graph.NodeSkill, "Go")
	if bobGo.ID == goSkill.ID {
		t.Fatal("two users' identically named nodes were folded into one")
	}

	if _, err := repo.NodeByID(ctx, bob, goSkill.ID); !errors.Is(err, graph.ErrNotFound) {
		t.Fatalf("bob read alice's node: %v", err)
	}
	if err := repo.DeleteNode(ctx, bob, goSkill.ID); !errors.Is(err, graph.ErrNotFound) {
		t.Fatalf("bob deleted alice's node: %v", err)
	}
	if err := repo.DeleteEdge(ctx, bob, e.ID); !errors.Is(err, graph.ErrNotFound) {
		t.Fatalf("bob deleted alice's edge: %v", err)
	}
	if found, err := repo.Neighbors(ctx, bob, goSkill.ID, graph.MaxNeighbors); err != nil || len(found) != 0 {
		t.Fatalf("bob expanded alice's node: %+v, %v", found, err)
	}
	if found, err := repo.NodesByLabel(ctx, bob, "Go"); err != nil || len(found) != 1 || found[0].ID != bobGo.ID {
		t.Fatalf("resolving a label for bob returned %+v", found)
	}
	if found, err := repo.MentionCandidates(ctx, bob, "how is Go going?", 50); err != nil || len(found) != 1 || found[0].ID != bobGo.ID {
		t.Fatalf("the mention scan for bob returned %+v", found)
	}

	// Alice's graph is untouched.
	nodes, err := repo.Nodes(ctx, alice, graph.Filter{Limit: graph.DefaultLimit})
	if err != nil || len(nodes) != 2 {
		t.Fatalf("alice's graph = %+v, %v", nodes, err)
	}
}

// Deleting a user takes their whole graph, through the foreign keys on both
// tables.
func TestDeletingAUserCascadesToTheGraph(t *testing.T) {
	pool := testDB(t)
	repo := graph.NewRepository(pool)
	alice := makeUser(t, pool, "alice@example.com")

	self := seedNode(t, repo, alice, graph.NodePerson, graph.SelfLabel)
	goSkill := seedNode(t, repo, alice, graph.NodeSkill, "Go")
	seedEdge(t, repo, alice, self, goSkill, graph.RelStudies, 0.9, nil)

	if _, err := pool.Exec(`DELETE FROM users WHERE id = $1`, alice); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"knowledge_nodes", "knowledge_edges"} {
		var n int
		if err := pool.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("%d rows survived in %s", n, table)
		}
	}
}

// The ?type= filter and paging.
func TestGraphNodesFilterAndPage(t *testing.T) {
	pool := testDB(t)
	repo := graph.NewRepository(pool)
	ctx := context.Background()
	alice := makeUser(t, pool, "alice@example.com")

	seedNode(t, repo, alice, graph.NodeSkill, "Go")
	seedNode(t, repo, alice, graph.NodeSkill, "Rust")
	seedNode(t, repo, alice, graph.NodePerson, "Priya")

	skills, err := repo.Nodes(ctx, alice, graph.Filter{Type: graph.NodeSkill, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(skills) != 2 {
		t.Fatalf("the skill filter returned %d nodes", len(skills))
	}
	page, err := repo.Nodes(ctx, alice, graph.Filter{Limit: 2, Offset: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 1 {
		t.Fatalf("the second page holds %d nodes, want 1", len(page))
	}
}
