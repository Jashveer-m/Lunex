# Lunex — Phase 5 Brief (for Claude Code)

## Goal
Build the AI memory system: extract worth-remembering facts from conversations, store them with embeddings for retrieval, feed relevant memories into the chat orchestrator's context, and let users view/edit/delete/disable/clear their memories. No knowledge graph, no agents yet.

## What exists already (Phases 1–4)
- `internal/chat` — orchestrator, retrieval (RAG + tasks/goals/notes heuristic), grounding rule, SSE chat
- `internal/embeddings` — Ollama embedding client (768-dim, `nomic-embed-text`)
- `internal/ai` — LLM provider abstraction (`Provider` interface, `Ollama`/`Mock` implementations)
- `internal/documents` — pgvector search pattern to follow for memory retrieval

Follow the same layering (transport → service → repository) and testing pattern (MockProvider for unit tests, real Postgres+Ollama for integration/e2e).

## Memory types (per the spec)
- **episodic** — events ("user completed the OS assignment")
- **semantic** — facts ("user knows Go")
- **preference** — ("user prefers studying in the morning")
- **project** — tied to a project/goal context
- **goal** — tied to a specific goal

## Database schema (Phase 5 only)

```sql
CREATE TABLE memories (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    type        text NOT NULL,  -- episodic/semantic/preference/project/goal
    content     text NOT NULL,  -- the fact itself, one sentence
    importance  real NOT NULL DEFAULT 0.5,  -- 0.0-1.0, model's confidence this is worth keeping
    confidence  real NOT NULL DEFAULT 0.5,  -- 0.0-1.0, model's confidence the extraction is accurate
    source_conversation_id uuid REFERENCES conversations(id) ON DELETE SET NULL,
    embedding   vector(768),
    enabled     boolean NOT NULL DEFAULT true,   -- user can disable without deleting
    expires_at  timestamptz,   -- nullable, for memories that shouldn't persist forever
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);
```

Index on `user_id`, an ivfflat/hnsw index on `embedding` (match whichever choice Phase 3 made for `document_chunks` — consistency over re-deciding), and a partial index on `enabled = true` since disabled memories are never retrieved. Reuse `set_updated_at`.

## Extraction pipeline

After an assistant turn completes (in the existing chat orchestrator, not a separate job queue — stay synchronous per the established pattern):

1. Send the just-completed turn (user message + assistant response) to the LLM provider with an extraction prompt: "Extract 0-3 facts worth remembering long-term about the user from this exchange. For each: the fact, its type, an importance score, a confidence score. Return nothing if there's nothing durable here."
2. Parse the response (expect structured/JSON output — if the model doesn't reliably return valid JSON, define a fallback format and document the reliability tradeoff)
3. For each extracted fact: embed it, store it with `source_conversation_id` set
4. **Do not extract from every turn indiscriminately** — skip extraction on short/trivial exchanges (e.g. under some message length threshold) to avoid flooding memory with noise. State your threshold and reasoning.

This is a second LLM call per turn — note the latency/cost tradeoff in your response (still free/local, but slower).

## Retrieval — feeding memory into chat

Extend the orchestrator's context-building step (from Phase 4) to also retrieve top-k relevant memories via embedding similarity, same pattern as document RAG search, same minimum-similarity floor concept (tune independently, don't assume Phase 4's document floor applies here — justify your number). Include retrieved memories in the `sources` list alongside documents, with type `memory`.

## API contract

All under `RequireAuth`, scoped to caller, same error shape/strict decoding/isolation-test pattern as prior phases.

- `GET /api/v1/memories` (filter by type, enabled)
- `PATCH /api/v1/memories/:id` (edit content, toggle enabled — re-embed if content changes)
- `DELETE /api/v1/memories/:id`
- `DELETE /api/v1/memories` — clear all (require a confirmation flag in the body, e.g. `{"confirm": true}`, reject otherwise)

## What NOT to do
- No knowledge graph, no agents, no action/approval engine
- No background job queue — extraction stays synchronous/inline for this phase
- Don't let memory extraction block or fail the chat response — if extraction fails, log it and still return the chat answer to the user

## Response format
Same as prior phases: architecture, files, migration SQL, API contract, implementation, tests (unit with MockProvider, cross-user isolation, extraction quality on a few example conversations), verification commands. In verification, include a real end-to-end check: have a conversation that states a durable fact, confirm a memory was extracted, then in a *new* conversation ask something that should retrieve that memory and confirm it's used and cited.
