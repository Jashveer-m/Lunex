# Lunex — Finance UI Brief (for Claude Code)

## Goal
Add a Finance screen to the existing frontend, following the same patterns as Calendar/Dashboard/Documents. Small addition, no backend changes.

## What exists already
- `frontend/src/pages/CalendarPage.tsx` — closest pattern to follow (list + create/edit dialog, date-range navigation)
- Backend: `GET/POST /api/v1/expense-categories`, `GET/POST /api/v1/expenses`, `GET/PATCH/DELETE /api/v1/expenses/:id`, `GET /api/v1/expenses/summary?start=&end=` — see `docs/api.md`

## Screen to build

**Finance page** (`/finance` route, add to sidebar nav):
- A month selector (prev/next), defaulting to current month
- Summary view at the top: total spent this month, broken down by category (use the `/expenses/summary` endpoint) — a simple bar or list, not a complex chart
- Expense list below: date, amount, category, description
- Add expense: amount, currency (default from existing entries or a sensible default), category (dropdown from `expense-categories`, with a quick "add new category" option), date, description
- Click an expense to edit or delete (confirm on delete)
- If an expense has `related_document_id`, show a small link/label (same pattern as calendar's task/goal link)

## What NOT to do
- No multi-currency conversion or charts beyond a simple category breakdown
- No budget-setting UI (not built on the backend yet)
- No backend changes — report any API gap found instead of adding to it

## Response format
Same as the Calendar UI report: what was built, files touched, verification steps (add an expense, confirm it appears in the summary, edit it, delete it, and confirm an expense logged via chat approval also shows up here).
