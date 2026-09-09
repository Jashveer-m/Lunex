# Lunex — Phase 4 Brief (for Claude Code)

## Goal
Build the AI chat assistant: a provider abstraction over the LLM, an orchestrator that pulls context (RAG search from Phase 3, plus tasks/goals/notes from Phase 2) into the prompt, and a chat API with streaming and citations. No memory system, no agents, no knowledge graph, no action/approval engine yet — this phase makes the assistant answer questions grounded in the user's own data.

## What exists already (Phases 1–3)
- Auth, tasks/goals/notes CRUD (per-user isolated), documents + RAG search (`internal/documents` — has a service-layer search function, not just an HTTP handler)
- `internal/embeddings` — Ollama embedding client, reuse its HTTP client pattern for the chat client
- Same layering convention: transport → service → repository

## Stack additions
- **LLM provider abstraction** — `internal/ai/provider.go` defining an interface (e.g. `Chat(ctx, messages, opts) (stream, error)`), with:
  - `OllamaProvider` — calls local Ollama `/api/chat` with model `llama3.2:3b` (configurable via env var, not hardcoded)
  - `MockProvider` — returns canned/deterministic responses, used in tests so the test suite doesn't depend on Ollama being up
- Do NOT build a cloud provider implementation this phase — just leave the interface able to accept one later. Note this in your response.

## Database schema (Phase 4 only)

```sql
CREATE TABLE conversations (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    title      text NOT NULL DEFAULT 'New conversation',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE messages (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    conversation_id uuid NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    role            text NOT NULL,  -- user/assistant/system
    content         text NOT NULL,
    sources         jsonb,          -- document/task/goal/note references used to ground this answer, null if none
    created_at      timestamptz NOT NULL DEFAULT now()
);
```

Standard indexes (user_id on conversations, conversation_id on messages). Reuse `set_updated_at`.

## The orchestrator (core of this phase)

For each user message:
1. Retrieve relevant document chunks via Phase 3's RAG search function (top ~5)
2. Retrieve relevant tasks/goals/notes — simple heuristic is fine this phase (e.g. upcoming deadlines, recently updated items), not semantic search on them yet
3. Build a prompt: system instructions + retrieved context + conversation history + user message
4. Call the LLM provider
5. Persist the user message and assistant response, with `sources` recording exactly what was retrieved and used
6. Return the response (streamed to the client)

**Grounding rule (from the master spec):** the assistant must never claim an answer came from the user's documents/tasks/goals if it didn't. If no relevant context was retrieved, say so plainly rather than inventing a citation. Bake this into the system prompt and note it explicitly in your response.

## API contract

All under `RequireAuth`, scoped to the caller.

- `POST /api/v1/conversations` — create a new conversation
- `GET /api/v1/conversations` — list user's conversations
- `GET /api/v1/conversations/:id` — conversation with its messages
- `DELETE /api/v1/conversations/:id`
- `POST /api/v1/conversations/:id/messages` — send a message, stream the response (Server-Sent Events or chunked — your call, explain which and why)

Same error shape, same strict decoding, same cross-user isolation test pattern as Phases 2/3.

## What NOT to do
- No memory system (that's Phase 5 per the roadmap) — conversation history via the `messages` table is enough context for now, no separate memory extraction/storage
- No agents (Study/Career/Finance/etc.) — one general assistant this phase
- No knowledge graph
- No action/approval engine — the assistant only answers, it doesn't create/modify tasks or goals yet even if asked (say so if a user asks it to take an action)
- No cloud LLM provider implementation — interface only

## Response format
Same as prior phases: architecture, files, migration SQL, API contract, implementation, tests (unit tests using MockProvider, plus the cross-user isolation test), verification commands. In verification, include a real end-to-end check: ask a question that should retrieve a Phase 3 document and confirm the response cites it; ask an unrelated question and confirm no false citation appears.
