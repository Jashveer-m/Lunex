# Lunex — Phase 6 Brief (for Claude Code)

## Goal
Build the personal knowledge graph: relational nodes/edges tables, auto-populated nodes from existing resources (tasks/goals/notes/documents), relationship extraction from chat turns (same pattern as Phase 5's memory extraction), a query API, and a light integration into the chat orchestrator's context (1-hop neighbor lookup, not full graph reasoning).

## What exists already (Phases 1–5)
- `internal/memories` — extraction pipeline pattern (threshold, LLM call, lenient parser, filters, embed+store) — reuse this shape for relationship extraction
- `internal/chat` — orchestrator with `Deps` taking optional searchers (documents, memories) — add a graph searcher the same way
- `internal/tasks`, `internal/goals`, `internal/notes`, `internal/documents` — existing resources that become graph nodes

Follow the same layering and testing conventions as prior phases.

## Entities and relationships (per the spec, scoped down)
Node types this phase: `task`, `goal`, `note`, `document`, `skill`, `person`, `project` (the last three are extracted from conversation, not existing tables — no dedicated table for them elsewhere).
Edge types this phase: `RELATED_TO`, `REQUIRES`, `DEPENDS_ON`, `WORKS_ON`, `KNOWS`, `INTERESTED_IN`, `STUDIES`, `COMPLETED`, `GOAL_OF`. (Skip `SPENT_ON`, `VISITED` — no expense/trip modules exist yet.)

## Database schema (Phase 6 only)

```sql
CREATE TABLE knowledge_nodes (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    type         text NOT NULL,   -- task/goal/note/document/skill/person/project
    label        text NOT NULL,  -- display name, e.g. "Go", "Alice", the task title
    ref_table    text,           -- 'tasks'/'goals'/'notes'/'documents', null for extracted-only nodes (skill/person/project)
    ref_id       uuid,           -- id in that table, null if ref_table is null
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE knowledge_edges (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    from_node_id uuid NOT NULL REFERENCES knowledge_nodes(id) ON DELETE CASCADE,
    to_node_id   uuid NOT NULL REFERENCES knowledge_nodes(id) ON DELETE CASCADE,
    relationship text NOT NULL,
    confidence   real NOT NULL DEFAULT 0.5,
    source_conversation_id uuid REFERENCES conversations(id) ON DELETE SET NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    CHECK (from_node_id != to_node_id)
);
```

Add a CHECK constraint on `knowledge_nodes.type` and `knowledge_edges.relationship` (allow-lists above). Unique constraint on `(user_id, ref_table, ref_id)` where `ref_table IS NOT NULL`, so a task never gets two nodes. Indexes on `user_id`, `(from_node_id)`, `(to_node_id)`. Reuse `set_updated_at` on nodes.

## Node sync (existing resources → nodes)

When a task/goal/note/document is created, ensure a corresponding node exists (create if missing, matched by `ref_table`+`ref_id`). When deleted, cascade removes the node (and its edges) automatically via FK. Decide and justify: does this happen inline in each module's service (tasks/goals/notes/documents each call into a `graph.Sync` function), or is it computed lazily on first graph query? Inline-on-write is likely simpler and more consistent with this project's synchronous style — but make the call and explain it.

## Relationship extraction from chat

After a turn (same point as Phase 5's memory extraction, can run alongside it or be combined into one LLM call — your call, explain the tradeoff):

1. Extract 0-3 relationships mentioned in the exchange: two entities and how they relate (e.g. "user is learning Go for the backend project" → `(user)-[STUDIES]->(Go, skill)`, `(Go, skill)-[REQUIRES... or RELATED_TO]->(backend project, project)`)
2. Match extracted entity labels against existing nodes for this user (fuzzy or exact — state which and why) before creating a new node, so "Go" mentioned twice doesn't become two nodes
3. Store the edge with `confidence` and `source_conversation_id`
4. Apply a similar quality filter to Phase 5's (confidence floor, skip trivial turns) — reuse the same threshold constant if it still makes sense, or justify a different one

## Chat integration (light)

Extend orchestrator context-building: if the user's message mentions something matching an existing node label, fetch its 1-hop neighbors and include them as context (labelled `type: "graph"` in sources), e.g. user asks about "Go" → graph shows it's `REQUIRES`-related to "backend project" (a goal) and that goal's deadline. This is a simple lookup, not a graph traversal algorithm — don't build multi-hop reasoning this phase.

## API contract

All under `RequireAuth`, scoped to caller, same conventions as prior phases.

- `GET /api/v1/knowledge-graph` — full graph for the user (nodes + edges), or filtered by `?type=`
- `GET /api/v1/knowledge-graph/nodes/:id` — a node with its 1-hop neighbors
- `DELETE /api/v1/knowledge-graph/nodes/:id` — only allowed for extracted-only nodes (skill/person/project); reject deleting a node backed by `ref_table` (they follow their source resource's lifecycle) with a clear error
- `DELETE /api/v1/knowledge-graph/edges/:id`

## What NOT to do
- No graph database (Neo4j etc.) — relational only, per the spec
- No multi-hop graph reasoning or graph algorithms (shortest path, centrality, etc.)
- No expense/trip/place/habit/course/job node types — those modules don't exist yet
- No UI/visualization — API only

## Response format
Same as prior phases: architecture, files, migration SQL, API contract, implementation, tests (unit with MockProvider, cross-user isolation, node dedup, sync-on-write), verification commands. In verification, include a real end-to-end check: create a task, confirm its node exists; have a conversation mentioning a relationship between two things; confirm the edge was extracted; ask a related question in a new conversation and confirm the graph context surfaces.
