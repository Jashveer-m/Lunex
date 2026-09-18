# Lunex — Phase 9 Brief (for Claude Code)

## Goal
Build the finance module: expense tracking with categories, spending analysis, graph node sync, and read/write tools registered with the existing agent/tool system — following the exact pattern from Phase 8 (calendar). The assistant can log expenses (via approval) and answer spending questions, but must never present itself as giving professional financial advice.

## What exists already (Phases 1–8 + hardening)
- `internal/calendar` — the most recent module to copy the pattern from exactly: transport → service → repository, required-window-style filters where relevant, graph node sync via trigger, tools with the no-invented-filter-values rule, approval-gated writes
- `internal/tools` — registry, `Param.Aliases`, the grounding rules from the hardening pass (arguments must come from the conversation, not be invented)
- `internal/graph` — node sync pattern (trigger-based cascade, established in Phase 8 to work around the polymorphic FK issue)
- `internal/agents` — router pattern

Follow the same layering and testing conventions as every prior phase, especially Phase 8's.

## Database schema (Phase 9 only)

```sql
CREATE TABLE expense_categories (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name       text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (user_id, name)
);

CREATE TABLE expenses (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    amount       numeric(12,2) NOT NULL CHECK (amount > 0),
    currency     text NOT NULL DEFAULT 'INR',
    category_id  uuid REFERENCES expense_categories(id) ON DELETE SET NULL,
    description  text,
    expense_date date NOT NULL,
    related_document_id uuid REFERENCES documents(id) ON DELETE SET NULL,  -- e.g. a receipt/invoice already uploaded
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);
```

Index on `(user_id, expense_date)` for range queries (same reasoning as calendar's composite index — leading `user_id` also serves the cascade). Reuse `set_updated_at`. Seed a small set of default categories on user registration (e.g. Food, Transport, Housing, Utilities, Other) — decide whether this is a migration-time seed or created lazily on first use, and justify.

## Node sync
Expenses get a graph node (`type: 'expense'`), same trigger-based pattern as Phase 8. Categories do not need their own nodes.

## Tools (register with existing tool registry)

Read:
- `search_expenses` — filter by date range, category; category and range must be grounded per the hardening rule, never invented
- `analyze_spending` — aggregate by category/time period (e.g. "how much did I spend on X this month") — this can be a read tool that does the aggregation server-side rather than dumping raw rows at the model

Write (requires approval):
- `create_expense`

## API contract

All under `RequireAuth`, scoped to caller, same conventions as prior phases.

- `GET/POST /api/v1/expense-categories`
- `GET /api/v1/expenses?start=&end=&category_id=` (start/end optional here, unlike calendar — a finance history query without a date range is reasonable, unlike an unbounded calendar)
- `POST /api/v1/expenses`
- `GET/PATCH/DELETE /api/v1/expenses/:id`
- `GET /api/v1/expenses/summary?start=&end=` — aggregated totals by category, backing the `analyze_spending` tool and reusable by a future dashboard

Same error shape, strict decoding, 404-not-403, cross-user isolation test.

## The financial-advice boundary (from the spec)
The system prompt must clearly frame spending analysis as informational/educational, not professional financial advice — the master spec explicitly requires this distinction. Add a rule to the chat system prompt: when discussing spending patterns, budgets, or financial decisions, the assistant states observations from the user's own data, never prescriptive financial advice ("you should invest in X"), and does not claim any regulatory or professional authority. Write a test that checks this framing appears when finance tools are used.

## What NOT to do
- No budget-setting/tracking-against-budget feature this phase (spec mentions "budget recommendations" as a capability, but start with tracking + analysis; budgets can be a later addition)
- No multi-currency conversion — store the currency as given, don't convert or fetch exchange rates
- No delete-via-chat tool (same rule as tasks/calendar)
- No receipt OCR/auto-extraction from uploaded documents this phase — `related_document_id` just links to an already-uploaded document, it doesn't trigger parsing

## Response format
Same as Phase 8: architecture, files, migration SQL, API contract, implementation, tests (unit + cross-user isolation + no-invented-filter-values pattern), verification commands. In verification, include a real end-to-end check: ask the assistant to log an expense, confirm it's proposed not created, approve it, confirm it exists; ask "how much have I spent this month" and confirm the answer is grounded in actual data and phrased as informational, not advice.
