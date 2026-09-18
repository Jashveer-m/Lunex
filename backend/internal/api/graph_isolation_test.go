package api_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jashveer/lifeos/backend/internal/ai"
	"github.com/jashveer/lifeos/backend/internal/graph"
)

// These run the whole Phase 6 stack -- router, RequireAuth, the graph service,
// the resource services that sync into it, and the real SQL -- against a
// throwaway Postgres. They are skipped unless TEST_DATABASE_URL is set.

// A conversation that states a relationship, and the JSON a model would answer
// the extraction prompt with. It has to name entities the exchange actually
// mentions, because the assertions below follow them into the graph.
const statingARelationship = "Something about how I am working: I have been learning Go " +
	"this term so that I can finish the backend project before the deadline."

// linkingProvider plays both parts of a turn the way rememberingProvider does:
// the relationship extraction is told apart from the memory extraction and
// from an ordinary answer by what its prompt asks for, not by call order.
func linkingProvider() *ai.Mock {
	return &ai.Mock{ReplyFunc: func(msgs []ai.Message) string {
		prompt := ai.PromptText(msgs)
		switch {
		case strings.Contains(prompt, "You extract relationships between things"):
			return `[{"from":"the user","from_type":"person","relationship":"STUDIES","to":"Go","to_type":"skill","confidence":0.9},
			         {"from":"the backend project","from_type":"project","relationship":"REQUIRES","to":"Go","to_type":"skill","confidence":0.85}]`
		case strings.Contains(prompt, "You extract durable facts"):
			return `[]`
		case strings.Contains(prompt, "] graph:"):
			return "Go is what the backend project needs [S1]."
		default:
			return "I could not find anything about that in your data."
		}
	}}
}

// readGraph reads the caller's whole graph through the API.
func readGraph(t *testing.T, srv *httptest.Server, token, query string) map[string]any {
	t.Helper()
	resp, body := doJSON(t, srv, http.MethodGet, "/api/v1/knowledge-graph"+query, token, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /knowledge-graph%s: status %d: %v", query, resp.StatusCode, body)
	}
	return body
}

func graphNodes(t *testing.T, body map[string]any) []map[string]any {
	t.Helper()
	raw, _ := body["nodes"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, n := range raw {
		m, _ := n.(map[string]any)
		out = append(out, m)
	}
	return out
}

func findNode(t *testing.T, body map[string]any, label string) map[string]any {
	t.Helper()
	for _, n := range graphNodes(t, body) {
		if n["label"] == label {
			return n
		}
	}
	t.Fatalf("no node labelled %q in %v", label, body["nodes"])
	return nil
}

// The property sync-on-write exists for, over HTTP: creating a task through
// the API puts a node in the graph, keyed to the task and labelled with its
// title.
func TestCreatingAResourceCreatesItsGraphNode(t *testing.T) {
	srv := isolationServer(t)
	alice := register(t, srv, "alice@example.com")

	taskID := create(t, srv, alice, "/api/v1/tasks", map[string]any{"title": "Write the scheduler"})
	goalID := create(t, srv, alice, "/api/v1/goals",
		map[string]any{"title": "the backend project", "type": "project"})
	noteID := create(t, srv, alice, "/api/v1/notes", map[string]any{"title": "Concurrency notes"})

	body := readGraph(t, srv, alice, "")
	if count, _ := body["node_count"].(float64); count != 3 {
		t.Fatalf("the graph holds %v nodes, want 3: %v", count, body["nodes"])
	}
	for _, tc := range []struct{ label, kind, table, id string }{
		{"Write the scheduler", graph.NodeTask, "tasks", taskID},
		{"the backend project", graph.NodeGoal, "goals", goalID},
		{"Concurrency notes", graph.NodeNote, "notes", noteID},
	} {
		n := findNode(t, body, tc.label)
		switch {
		case n["type"] != tc.kind:
			t.Fatalf("%q is a %v, want %v", tc.label, n["type"], tc.kind)
		case n["ref_table"] != tc.table:
			t.Fatalf("%q points at %v, want %v", tc.label, n["ref_table"], tc.table)
		case n["ref_id"] != tc.id:
			t.Fatalf("%q points at %v, want %v", tc.label, n["ref_id"], tc.id)
		case n["extracted"] != false:
			t.Fatalf("%q reports itself as extracted", tc.label)
		}
	}

	// Renaming follows, and does not duplicate.
	resp, _ := doJSON(t, srv, http.MethodPatch, "/api/v1/tasks/"+taskID, alice,
		map[string]any{"title": "Write the CFS scheduler"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH /tasks: %d", resp.StatusCode)
	}
	body = readGraph(t, srv, alice, "")
	if count, _ := body["node_count"].(float64); count != 3 {
		t.Fatalf("renaming made the graph %v nodes, want 3", count)
	}
	findNode(t, body, "Write the CFS scheduler")

	// And deleting the task takes its node, through the trigger.
	resp, _ = doJSON(t, srv, http.MethodDelete, "/api/v1/tasks/"+taskID, alice, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE /tasks: %d", resp.StatusCode)
	}
	body = readGraph(t, srv, alice, "")
	if count, _ := body["node_count"].(float64); count != 2 {
		t.Fatalf("deleting the task left %v nodes, want 2: %v", count, body["nodes"])
	}
	_ = noteID
}

// The end-to-end property Phase 6 exists for, over HTTP: a relationship stated
// in one conversation is extracted into the graph, and a *different*
// conversation that names one end of it gets the connection as context, uses
// it and cites it.
func TestARelationshipLearnedInOneConversationGroundsAnother(t *testing.T) {
	srv := isolationServerWithProvider(t, linkingProvider())
	alice := register(t, srv, "alice@example.com")

	// The goal exists first, so the extracted "the backend project" has an
	// existing node to attach to rather than making a parallel one.
	goalID := create(t, srv, alice, "/api/v1/goals",
		map[string]any{"title": "the backend project", "type": "project"})

	convID := create(t, srv, alice, "/api/v1/conversations", map[string]any{})
	_, events := ask(t, srv, alice, convID, statingARelationship)
	_, done := answer(t, events)
	if done == nil {
		t.Fatal("the stating turn produced no done event")
	}
	linked, _ := done["linked"].([]any)
	if len(linked) != 2 {
		t.Fatalf("the turn reported %d new relationships, want 2: %v", len(linked), done["linked"])
	}

	// The graph now holds the goal's node, the user, and Go -- and the
	// extracted "the backend project" resolved onto the goal rather than
	// creating a second node with the same name.
	body := readGraph(t, srv, alice, "")
	if count, _ := body["node_count"].(float64); count != 3 {
		t.Fatalf("the graph holds %v nodes, want 3: %v", count, body["nodes"])
	}
	project := findNode(t, body, "the backend project")
	if project["ref_id"] != goalID {
		t.Fatalf("the extracted project did not resolve onto the goal: %v", project)
	}
	if edges, _ := body["edge_count"].(float64); edges != 2 {
		t.Fatalf("the graph holds %v edges, want 2: %v", edges, body["edges"])
	}
	self := findNode(t, body, graph.SelfLabel)
	if self["type"] != graph.NodePerson || self["extracted"] != true {
		t.Fatalf("the self node is %v", self)
	}

	// A brand new conversation: nothing is shared with the first one but the
	// graph, so anything the assistant knows here came out of it.
	recallID := create(t, srv, alice, "/api/v1/conversations", map[string]any{})
	_, events = ask(t, srv, alice, recallID, "why am I bothering with Go?")
	text, done := answer(t, events)

	sources := doneSources(t, done)
	var graphSources []map[string]any
	for _, s := range sources {
		if s["type"] == "graph" {
			graphSources = append(graphSources, s)
		}
	}
	if len(graphSources) != 1 {
		t.Fatalf("the new conversation retrieved %d graph sources: %v", len(graphSources), sources)
	}
	if graphSources[0]["title"] != "Go" {
		t.Fatalf("the graph source is %v, want the node the question named", graphSources[0])
	}
	if graphSources[0]["cited"] != true {
		t.Fatalf("the answer did not cite the connection it was given: %q", text)
	}
	excerpt, _ := graphSources[0]["excerpt"].(string)
	if !strings.Contains(excerpt, `"the backend project" (goal) REQUIRES "Go" (skill)`) {
		t.Fatalf("the graph source does not carry the link: %q", excerpt)
	}
}

// A node with a 1-hop neighbourhood, read directly.
func TestReadingANodeReturnsItsNeighbours(t *testing.T) {
	srv := isolationServerWithProvider(t, linkingProvider())
	alice := register(t, srv, "alice@example.com")
	create(t, srv, alice, "/api/v1/goals", map[string]any{"title": "the backend project", "type": "project"})

	convID := create(t, srv, alice, "/api/v1/conversations", map[string]any{})
	if _, events := ask(t, srv, alice, convID, statingARelationship); answer2(t, events) == nil {
		t.Fatal("the stating turn failed")
	}

	goNode := findNode(t, readGraph(t, srv, alice, ""), "Go")
	resp, body := doJSON(t, srv, http.MethodGet, "/api/v1/knowledge-graph/nodes/"+goNode["id"].(string), alice, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /knowledge-graph/nodes: %d: %v", resp.StatusCode, body)
	}
	if count, _ := body["neighbor_count"].(float64); count != 2 {
		t.Fatalf("Go has %v neighbours, want 2: %v", count, body)
	}
	neighbors, _ := body["neighbors"].([]any)
	for _, raw := range neighbors {
		nb, _ := raw.(map[string]any)
		// Both edges point *at* Go, and the direction is carried rather than
		// flattened away.
		if nb["incoming"] != true {
			t.Fatalf("neighbour %v is reported as outgoing from Go", nb)
		}
		edge, _ := nb["edge"].(map[string]any)
		if edge["source_conversation_id"] != convID {
			t.Fatalf("the edge does not record where it came from: %v", edge)
		}
	}
}

// The delete rule the endpoint exists to enforce.
func TestDeletingGraphNodesAndEdges(t *testing.T) {
	srv := isolationServerWithProvider(t, linkingProvider())
	alice := register(t, srv, "alice@example.com")
	create(t, srv, alice, "/api/v1/goals", map[string]any{"title": "the backend project", "type": "project"})

	convID := create(t, srv, alice, "/api/v1/conversations", map[string]any{})
	if _, events := ask(t, srv, alice, convID, statingARelationship); answer2(t, events) == nil {
		t.Fatal("the stating turn failed")
	}

	body := readGraph(t, srv, alice, "")
	mirrored := findNode(t, body, "the backend project")
	extracted := findNode(t, body, "Go")

	// A node that mirrors a goal is refused, with a message that says what to
	// delete instead. It is 409, not 404: the node exists and Alice owns it.
	resp, out := doJSON(t, srv, http.MethodDelete,
		"/api/v1/knowledge-graph/nodes/"+mirrored["id"].(string), alice, nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("deleting a mirrored node = %d, want 409: %v", resp.StatusCode, out)
	}
	if out["error"] != "node_is_backed" {
		t.Fatalf("error = %v, want node_is_backed", out["error"])
	}

	// One edge can go on its own, leaving both of its nodes.
	edges, _ := body["edges"].([]any)
	first, _ := edges[0].(map[string]any)
	resp, out = doJSON(t, srv, http.MethodDelete,
		"/api/v1/knowledge-graph/edges/"+first["id"].(string), alice, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE /edges = %d: %v", resp.StatusCode, out)
	}
	after := readGraph(t, srv, alice, "")
	if count, _ := after["edge_count"].(float64); count != 1 {
		t.Fatalf("the graph holds %v edges after one delete, want 1", count)
	}
	if count, _ := after["node_count"].(float64); count != 3 {
		t.Fatalf("deleting an edge left %v nodes, want 3 -- it must not take its endpoints", count)
	}

	// An extracted node is Alice's to remove, and its edges go with it.
	resp, out = doJSON(t, srv, http.MethodDelete,
		"/api/v1/knowledge-graph/nodes/"+extracted["id"].(string), alice, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("deleting an extracted node = %d: %v", resp.StatusCode, out)
	}
	after = readGraph(t, srv, alice, "")
	if count, _ := after["node_count"].(float64); count != 2 {
		t.Fatalf("the graph holds %v nodes, want 2", count)
	}
	if count, _ := after["edge_count"].(float64); count != 0 {
		t.Fatalf("%v edges survived their node", count)
	}
}

// The ?type= filter returns a closed subgraph: no edge in it points at a node
// it did not also return.
func TestGraphTypeFilterReturnsADrawableSubgraph(t *testing.T) {
	srv := isolationServerWithProvider(t, linkingProvider())
	alice := register(t, srv, "alice@example.com")
	create(t, srv, alice, "/api/v1/goals", map[string]any{"title": "the backend project", "type": "project"})

	convID := create(t, srv, alice, "/api/v1/conversations", map[string]any{})
	if _, events := ask(t, srv, alice, convID, statingARelationship); answer2(t, events) == nil {
		t.Fatal("the stating turn failed")
	}

	body := readGraph(t, srv, alice, "?type=skill")
	nodes := graphNodes(t, body)
	if len(nodes) != 1 || nodes[0]["label"] != "Go" {
		t.Fatalf("the skill filter returned %v", body["nodes"])
	}
	if count, _ := body["edge_count"].(float64); count != 0 {
		t.Fatalf("the filtered read returned %v dangling edges", count)
	}

	resp, out := doJSON(t, srv, http.MethodGet, "/api/v1/knowledge-graph?type=invoice", alice, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("an unknown type filter = %d, want 400: %v", resp.StatusCode, out)
	}
}

// The test the whole ownership design exists for, for Phase 6's endpoints.
func TestCrossUserGraphIsolation(t *testing.T) {
	srv := isolationServerWithProvider(t, linkingProvider())
	alice := register(t, srv, "alice@example.com")
	bob := register(t, srv, "bob@example.com")

	create(t, srv, alice, "/api/v1/goals", map[string]any{"title": "the backend project", "type": "project"})
	convID := create(t, srv, alice, "/api/v1/conversations", map[string]any{})
	if _, events := ask(t, srv, alice, convID, statingARelationship); answer2(t, events) == nil {
		t.Fatal("the stating turn failed")
	}

	body := readGraph(t, srv, alice, "")
	node := findNode(t, body, "Go")
	edges, _ := body["edges"].([]any)
	edge, _ := edges[0].(map[string]any)

	// Every one of these is a row that exists and that Bob does not own. The
	// answer must be 404 -- a 403 would confirm the id is real, and a 409 on
	// the mirrored node would confirm it is backed by something.
	for _, tc := range []struct {
		name, method, path string
	}{
		{"read a node", http.MethodGet, "/api/v1/knowledge-graph/nodes/" + node["id"].(string)},
		{"delete a node", http.MethodDelete, "/api/v1/knowledge-graph/nodes/" + node["id"].(string)},
		{"delete a mirrored node", http.MethodDelete,
			"/api/v1/knowledge-graph/nodes/" + findNode(t, body, "the backend project")["id"].(string)},
		{"delete an edge", http.MethodDelete, "/api/v1/knowledge-graph/edges/" + edge["id"].(string)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, out := doJSON(t, srv, tc.method, tc.path, bob, nil)
			if resp.StatusCode != http.StatusNotFound {
				t.Fatalf("%s %s as the wrong user = %d, want 404: %v", tc.method, tc.path, resp.StatusCode, out)
			}
			if out["error"] != "not_found" {
				t.Fatalf("error = %v, want not_found", out["error"])
			}
		})
	}

	// Bob's graph is empty, and asking about Alice's node retrieves nothing.
	bobGraph := readGraph(t, srv, bob, "")
	if count, _ := bobGraph["node_count"].(float64); count != 0 {
		t.Fatalf("Bob sees %v nodes, want 0: %v", count, bobGraph)
	}
	bobConv := create(t, srv, bob, "/api/v1/conversations", map[string]any{})
	_, events := ask(t, srv, bob, bobConv, "why am I bothering with Go?")
	_, done := answer(t, events)
	for _, s := range doneSources(t, done) {
		if s["type"] == "graph" {
			t.Fatalf("Bob's question reached Alice's graph: %v", s)
		}
	}

	// And none of that touched Alice's graph.
	after := readGraph(t, srv, alice, "")
	if a, b := after["node_count"], body["node_count"]; a != b {
		t.Fatalf("Alice's graph went from %v to %v nodes after Bob's attempts", b, a)
	}
	if a, b := after["edge_count"], body["edge_count"]; a != b {
		t.Fatalf("Alice's graph went from %v to %v edges after Bob's attempts", b, a)
	}
}

// The graph endpoints are behind RequireAuth like everything else.
func TestGraphRequiresAuth(t *testing.T) {
	srv := newTestServer(t)
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/knowledge-graph"},
		{http.MethodGet, "/api/v1/knowledge-graph/nodes/" + zeroUUID},
		{http.MethodDelete, "/api/v1/knowledge-graph/nodes/" + zeroUUID},
		{http.MethodDelete, "/api/v1/knowledge-graph/edges/" + zeroUUID},
	} {
		resp, body := doJSON(t, srv, tc.method, tc.path, "", nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s %s without a token = %d, want 401: %v", tc.method, tc.path, resp.StatusCode, body)
		}
	}
}

const zeroUUID = "00000000-0000-0000-0000-000000000000"

// doneSources pulls the sources recorded on the assistant message in a `done`
// frame -- the stored citation trail, not the pre-generation `sources` event.
func doneSources(t *testing.T, done map[string]any) []map[string]any {
	t.Helper()
	if done == nil {
		t.Fatal("the turn produced no done event")
	}
	message, _ := done["message"].(map[string]any)
	raw, _ := message["sources"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, s := range raw {
		m, _ := s.(map[string]any)
		out = append(out, m)
	}
	return out
}

// answer2 is `answer` for the calls that only need to know the turn finished.
func answer2(t *testing.T, events []sseEvent) map[string]any {
	t.Helper()
	_, done := answer(t, events)
	return done
}
