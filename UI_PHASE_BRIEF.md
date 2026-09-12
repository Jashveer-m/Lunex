# Lunex — Frontend Phase Brief (for Claude Code)

## Goal
Build a working UI over everything implemented so far (Phases 1–7): auth, tasks/goals/notes, document upload, AI chat with citations and action approval, and a memory viewer. This is the first time the product will be seen, not just curl'd — make it functional and reasonably clean, not a bare admin table dump.

## What exists already
- `frontend/` — Vite + React + TypeScript + Tailwind scaffold, currently just a health-check panel. Build on it, don't restart it.
- Full backend API across Phases 1–7 (see `docs/api.md` for the complete contract) — auth, tasks, goals, notes, documents, conversations/messages (SSE), memories, knowledge-graph, actions.

## Screens to build

1. **Auth** — register and login forms. Store the access token in memory (React state/context), store the refresh token in a way that survives a page reload (localStorage is acceptable for this project's scope — flag the XSS tradeoff in your response, same as Phase 1's deferred decision). On 401, attempt a silent refresh before forcing re-login.

2. **Dashboard** — tasks, goals, and notes in one view (tabs or sections). Create/edit/delete for each, matching the existing API fields. Keep it simple — a clean list with a create form, not a kanban board.

3. **Documents** — upload (drag-and-drop or file picker), list with status (processing/ready/failed), delete. Show chunk count when ready.

4. **Chat** — the core screen. Conversation list + active conversation view. Send a message, stream the response token-by-token (the backend sends SSE `sources`/`token`/`done`/`error`/`action` frames — render tokens as they arrive, don't wait for `done`). Show citations inline or as a footer list (document/memory/graph sources, with title and similarity). When an `action` frame arrives (a proposed write), render it as a distinct card with **Approve** / **Reject** buttons that call the real endpoints — this is important, the assistant should never claim an action happened without the user clicking one of these.

5. **Memories** — list view (filter by type, enabled/disabled), edit content, toggle enabled, delete individual, "clear all" with a confirmation dialog (the API requires `{"confirm": true}` — surface a real confirmation step in the UI, not a silent call).

## Design direction
Clean, minimal, modern — should not look like a generic admin dashboard template. Consult the frontend-design conventions you have access to for spacing/typography/color choices. Dark mode is a nice-to-have, not required this pass. Loading and error states matter more than polish — every screen needs a visible loading indicator and a real error message on failure, not a silent blank state.

## What NOT to do
- No mobile app (already scoped out of this project)
- No graph visualization (a simple list of nodes/edges is enough if you show the knowledge graph at all — don't build a force-directed graph renderer)
- No new backend endpoints or backend changes — if you find the UI needs something the API doesn't provide, stop and report it rather than modifying the backend
- No offline/PWA features, no state management library unless genuinely needed (React context/state is likely enough for this scope)

## Response format
Same as prior phases: architecture (routing, state/auth handling), files, API integration notes, what's implemented per screen, known gaps, and verification steps (how to run `npm run dev` and manually click through register → login → create a task → upload a document → chat with a citation → approve a proposed action → view memories). Screenshots aren't necessary — a clear description of what each screen does is enough.
