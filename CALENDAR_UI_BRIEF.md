# Lunex — Calendar UI Brief (for Claude Code)

## Goal
Add a Calendar screen to the existing frontend, following the same patterns as the Dashboard/Documents/Memories pages already built. This is a small addition, not a new phase — no backend changes.

## What exists already
- `frontend/src/lib/{api,auth,endpoints,router,types}.ts` — API client, routing, auth context
- `frontend/src/pages/{DashboardPage,DocumentsPage,MemoriesPage}.tsx` — patterns to follow for a list+create screen
- Backend: `GET /api/v1/calendar?start=&end=` (both required), `POST /api/v1/calendar`, `GET/PATCH/DELETE /api/v1/calendar/:id` — see `docs/api.md` for the full contract
- The chat Approvals flow already handles calendar events created via the assistant (`create_calendar_event` tool) — this UI is for direct calendar management, separate from that

## Screen to build

**Calendar page** (`/calendar` route, add to sidebar nav):
- A week view is enough — don't build a full month grid or day/week/month view switcher, keep it simple
- Show events for the current week by default, with prev/next week navigation (this drives the required `start`/`end` query params)
- Create event: title, start time, end time (or all-day toggle), optional location/description
- Click an event to edit or delete (with confirmation on delete)
- If an event has `related_task_id` or `related_goal_id`, show a small link/label to that task or goal

## What NOT to do
- No recurrence UI (the backend stores `recurrence_rule` as opaque, don't build a recurrence picker)
- No drag-to-reschedule, no month view, no external calendar sync UI
- No backend changes — if something's missing from the API, report it, don't add it

## Response format
Same as the original UI phase brief: what was built, files touched, how to verify (register/login already works — just click through: view the week, create an event, edit it, delete it, and confirm an event created via chat approval also shows up here).
