# Lunex — Phase 10a Brief (for Claude Code)

## Goal
Build the first slice of the Study module: study plans and document-grounded flashcards. This is 10a of 5 planned sub-phases (10a plans+flashcards, 10b quizzes, 10c weak-topic tracking, 10d spaced repetition, 10e sessions/streaks) — build only this slice, the others come as separate briefs on top of this one.

## What exists already (Phases 1–9 + hardening)
- `internal/documents` — has chunks with embeddings (`document_chunks`), a service-layer search function
- `internal/finance` — most recent module, closest pattern to copy (transport → service → repository, tools, approval-gated writes, graph node sync)
- `internal/graph`, `internal/tools`, `internal/agents` — established patterns

Follow the same layering and testing conventions as Phase 9.

## Database schema (Phase 10a only)

```sql
CREATE TABLE study_plans (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    title       text NOT NULL,
    description text,
    document_id uuid REFERENCES documents(id) ON DELETE SET NULL,  -- optional: plan built from a specific document
    status      text NOT NULL DEFAULT 'active',  -- active/completed/abandoned
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE flashcards (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id       uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    study_plan_id uuid REFERENCES study_plans(id) ON DELETE CASCADE,
    document_id   uuid REFERENCES documents(id) ON DELETE SET NULL,  -- source, if generated from a document
    front         text NOT NULL,
    back          text NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now()
);
```

Index on `user_id` for both, `study_plan_id` on flashcards. Reuse `set_updated_at` on `study_plans`.

## Flashcard generation (the "document-grounded" part)
A tool/service function that takes a `document_id`, pulls its chunks (via the existing documents search/read function), and asks the LLM to generate N flashcards (front/back pairs) grounded in that content — same reasoning discipline as memory/graph extraction: the LLM should produce cards *from* the retrieved text, not invent content, and a lenient-parser approach (per Phase 5/6 findings) rather than strict JSON mode.

## Tools (register with existing tool registry)

Read:
- `search_study_plans`

Write (requires approval):
- `create_study_plan`
- `generate_flashcards` — takes a document reference, proposes a batch of flashcards (the approval card should show the generated cards for review before they're created, not just "create N flashcards")

## API contract

All under `RequireAuth`, scoped to caller.

- `GET/POST /api/v1/study-plans`
- `GET/PATCH/DELETE /api/v1/study-plans/:id`
- `GET /api/v1/study-plans/:id/flashcards`
- `POST /api/v1/study-plans/:id/flashcards` (manual add, direct — not through chat approval, for when a user wants to add one by hand)
- `DELETE /api/v1/flashcards/:id`

Same error shape, strict decoding, 404-not-403, cross-user isolation test.

## Graph sync
`study_plans` gets a node (`type: 'skill'` doesn't fit — add `type: 'project'` reuse, or introduce nothing new this phase; flashcards don't need individual nodes, too granular).

## What NOT to do
- No quizzes yet (10b)
- No weak-topic tracking, no spaced repetition, no session tracking (10c/d/e)
- No flashcard review/study UI interaction logic (e.g. "I got this right/wrong") — that's tied to weak-topic tracking in 10c, don't build it prematurely here

## Response format
Same as prior phases: architecture, files, migration SQL, API contract, implementation, tests (unit + cross-user isolation + grounding test — flashcards must trace to the source document, not invented content), verification commands. In verification: generate flashcards from a real uploaded document, confirm they're proposed not created, approve, confirm they exist and their content actually reflects the document.
