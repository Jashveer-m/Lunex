# Lunex — Phase 8 Brief (for Claude Code)

## Goal
Build the calendar module: events CRUD, graph node sync (like tasks/goals/notes/documents already get), and read/write tools registered with the existing agent/tool system from Phase 7, so the assistant can check the calendar and propose new events (subject to the same approval flow as task creation).

## What exists already (Phases 1–7 + hardening)
- `internal/tasks`, `internal/goals`, `internal/notes` — CRUD pattern to follow exactly
- `internal/graph` — node sync pattern (`NodeSyncer` interface, called inline on write from each module's service)
- `internal/tools` — tool registry pattern (`search_tasks`, `create_task`, etc., with the Phase-hardening fix that filter/argument values must come from the conversation, never invented)
- `internal/actions` — approval flow for write tools (propose → approve/reject → execute)
- `internal/chat` — orchestrator `Deps` pattern for pluggable context sources

Follow the same layering (transport → service → repository) and testing conventions (cross-user isolation test, MockProvider for chat-adjacent unit tests) as every prior phase.

## Database schema (Phase 8 only)

```sql
CREATE TABLE calendar_events (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id        uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    title          text NOT NULL,
    description    text,
    start_time     timestamptz NOT NULL,
    end_time       timestamptz NOT NULL,
    all_day        boolean NOT NULL DEFAULT false,
    location       text,
    recurrence_rule text,  -- e.g. RRULE string, nullable; do not build recurrence expansion logic this phase, just store it
    related_task_id uuid REFERENCES tasks(id) ON DELETE SET NULL,
    related_goal_id uuid REFERENCES goals(id) ON DELETE SET NULL,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    CHECK (end_time >= start_time)
);
```

Index on `user_id`, and on `(user_id, start_time)` for the common "what's on my calendar between X and Y" query. Reuse `set_updated_at`.

## Node sync
`calendar_events` gets a graph node like tasks/goals/notes/documents (`type: 'event'`, matching the spec's entity list). Follow the exact pattern from Phase 6/7 — inline sync on write, unique on `(user_id, ref_table, ref_id)`.

## Tools (register with existing tool registry)

Read:
- `search_calendar` — query by date range, returns events in that window

Write (requires approval, same as `create_task`):
- `create_calendar_event`

Apply the hardening-pass rule: no invented filter/argument values — a date range or event detail must come from the conversation, not be fabricated.

## API contract

All under `RequireAuth`, scoped to caller, same conventions as prior phases.

- `GET /api/v1/calendar?start=&end=` (date range query, required — don't allow an unbounded "all events" query)
- `POST /api/v1/calendar`
- `GET/PATCH/DELETE /api/v1/calendar/:id`

Same error shape, strict decoding, 404-not-403 isolation pattern, cross-user isolation test.

## Chat integration
Extend the orchestrator's context-building (same place tasks/goals/notes heuristic retrieval happens) to include today's/upcoming calendar events when relevant — this is what lets "plan my day" style questions work per the master spec's example. Light heuristic (e.g. events in the next 24-48h), not a new retrieval subsystem.

## What NOT to do
- No recurrence expansion logic (store `recurrence_rule` as an opaque string, don't compute repeated instances)
- No external calendar sync (Google Calendar, iCal import/export) — purely internal this phase
- No delete-event tool via chat (same reasoning as Phase 7's no-delete-tools rule) — deletion stays a direct API/UI action only

## Response format
Same as prior phases: architecture, files, migration SQL, API contract, implementation, tests (unit + cross-user isolation + the no-invented-filter-values test pattern from the hardening pass), verification commands. In verification, include a real end-to-end check: ask the assistant to schedule something, confirm it's proposed not created, approve it, confirm it appears in `GET /calendar`.
