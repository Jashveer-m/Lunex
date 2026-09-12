# Lunex — Full End-to-End Verification (for Claude Code)

## Goal
This is a verification pass, not a build phase. Confirm the whole stack — Phases 1 through 7 — actually works together as a real user would experience it. Report what works and what doesn't. Do NOT fix, refactor, or improve anything you find broken — just report it clearly so we can decide what to do about it separately. If something is trivially broken (a typo, an obviously wrong env var), you may note the one-line fix but do not apply it without asking.

## Time-box
This should take well under an hour — it's running existing code, not writing new code. If any single step is taking more than a few minutes beyond what's expected (LLM calls can be slow on this machine, 10-80s each is normal), say so and move on rather than getting stuck.

## What to check

### 1. Automated suites (fast — run these first)
```
go build ./... && go vet ./... && gofmt -l .
go test ./...                                  # no DB/Ollama
go test ./... -count=1 -p 1                    # with TEST_DATABASE_URL set
```
Report pass/fail per package. Don't re-explain what already passed in prior phase reports — just confirm current state.

### 2. Real end-to-end walkthrough (the actual check)
Start the real stack (Postgres, Ollama running, API server) and walk through this sequence for real, using curl or the equivalent — not mocked:

1. **Register** a fresh user, **login**, confirm `/me` works
2. **Create a task, a goal, and a note** via the CRUD APIs
3. **Upload a document** (small text file), confirm it reaches `status: ready` with chunks
4. **Start a conversation, ask a question the document answers** — confirm the response cites it (Phase 3/4 check)
5. **State a durable preference in the same or a new conversation** — confirm a memory gets extracted (Phase 5 check)
6. **In a new conversation, ask something that should recall that memory** — confirm it's retrieved and cited
7. **Have a conversation that mentions a relationship between two things** (e.g. "I'm learning X for project Y") — confirm a graph node/edge gets created (Phase 6 check)
8. **Ask the assistant to create a task via chat** — confirm it's proposed, not created; **approve it**, confirm the task now exists; **ask it to create another, reject it**, confirm nothing was created (Phase 7 check)
9. **Cross-user isolation spot-check**: create a second user, confirm they cannot see the first user's tasks/documents/memories/graph/conversations (a quick manual check, not the full isolation test suite — that already runs in step 1)

### 3. What to report
For each of the 9 steps: pass/fail, and for any fail, the actual error/response — not a guess at the cause. At the end, a short summary: what's solid, what's fragile, anything discovered that wasn't caught by the phase-by-phase unit/integration tests (that's the main value of doing this — real cross-phase interaction bugs that isolated tests don't catch).

## What NOT to do
- Don't write new tests, don't refactor, don't fix bugs you find (beyond a one-line trivial fix, and ask first even then)
- Don't re-verify things Phase 1-7 reports already confirmed in isolation — focus on the *integration* between phases, since that's what hasn't been checked yet
- Don't let a single slow LLM call become a rabbit hole — if something hangs well past expected, note it and move to the next check

## Response format
Keep this short and direct — a pass/fail table for the 9 steps, then a brief summary. Skip the architecture/files/schema sections from the phase-brief format; nothing is being built.
