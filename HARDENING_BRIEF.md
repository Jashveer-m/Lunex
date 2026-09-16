# Lunex — Hardening Pass (for Claude Code)

## Goal
Fix the bugs found during the full end-to-end verification and UI testing passes. This is a bug-fix pass, not a new phase — no new features, no new endpoints, no schema changes unless a fix genuinely requires one (state clearly if so).

## Fixes

### 1. Default timeouts too low for this machine
Raise defaults in `config.go`: `MEMORY_EXTRACT_TIMEOUT` and `GRAPH_EXTRACT_TIMEOUT` to 180s, `AGENT_TIMEOUT` to 180s, `CHAT_TIMEOUT` to 12–18m (match what `scripts/e2e.sh` already recommends). These are defaults, not hardcoded — still overridable via env.

### 2. Extraction reads unconfirmed/misattributed content (root cause behind three symptoms)
Three related bugs, one fix each but same underlying principle — **extraction pipelines (memory and graph) must only read confirmed reality**, never a proposed-but-not-approved action, never content that only appears because it was retrieved from a document rather than said by the user:

- **Rejected/proposed actions become memories.** A rejected "book a flight to Delhi" was stored as if it happened; a proposed task became a memory before approval. Fix: memory extraction must run on the actual conversation text only, and must not treat a tool-call proposal (approved or not) as a fact about what the user has/did. If it needs the outcome of an action, it should only see `status: executed` results, never `proposed`/`rejected`.
- **Graph extractor reads proposals before they exist.** Approving "Renew my passport" left two disconnected nodes — an extracted `project` node with the edge, and the real `task` node with no edges, because the graph extractor ran on the proposal text, not the executed result. Fix: run graph extraction (like memory extraction) on the confirmed exchange, and if an action was approved, link the edge to the real node created by that execution, not a freshly-invented node with a similar label.
- **Document content misattributed as user facts.** Asking about an uploaded document produced a memory "The user knows that tap water stunted last year's seedlings" — content from the document, phrased as being about the user, which slips past the existing `AboutTheUser` check because it literally contains the word "user". Tighten this check: a fact whose content matches or closely paraphrases a retrieved document chunk should be rejected regardless of phrasing, not just checked for the word "user".

### 3. Non-work preferences never extracted
The extraction prompt defines "preference" too narrowly ("how they like to work") — "I'm vegetarian" and similar personal (non-work) preferences return `[]`. Broaden the prompt's framing of what counts as a preference worth remembering, and re-verify against a few examples spanning food/lifestyle/work preferences, not just work ones.

### 4. Assistant misdescribes the approval flow
The assistant tells users to "type 'approve'" or "type yes/no" in chat to approve/reject an action — this does nothing; only the real `POST /actions/:id/approve` / `/reject` endpoints (surfaced via the UI's Approve/Reject buttons) work. Fix the system prompt so the assistant accurately describes how approval actually works (a card/button in the UI), not an invented chat command.

### 5. Tool-calling invents search filter values
The router/tool-calling step added filter values (e.g. `tag: "<unknown>"`, `tag: "greenhouse"`) that never appeared anywhere in the conversation, and the tool used them literally — causing a real search to return 0 results because of a hallucinated filter. Fix: constrain tool-call argument construction so filter values must come from the conversation text; if the model doesn't have a real value for an optional filter, it should omit the field entirely, not invent one. Add a test that specifically checks this.

### 6. Empty conversation missing `messages` field
`GET /conversations/:id` omits the `messages` field entirely (via `omitempty`) when a conversation has no messages yet, contradicting `docs/api.md` which says it's always present. This broke the frontend until the client was made to tolerate it. Fix: always include `messages` as an empty array `[]`, never omit the field.

## Minor fixes (bundle together, skip any that turns out non-trivial — note it instead of getting stuck)
- A similarity score (e.g. "with a similarity of 0.79") leaks into user-facing assistant text — it should stay internal/metadata, not appear in the generated response.
- Graph edge direction is sometimes backwards (e.g. `TypeScript -REQUIRES-> recipe app` when it should be the reverse) — check the extraction prompt's directionality instructions.
- Log level is hardcoded to Info in `cmd/api/main.go` — make it configurable via env var so debug-level extraction-drop reasons can be seen when needed.
- Relative date parsing ("next Friday") is off by a day in some cases — check the date-resolution logic against the actual current date.

## What NOT to do
- No new features, no new endpoints, no schema migrations unless truly unavoidable (say so clearly if one is)
- Don't refactor unrelated code while you're in these files — keep changes scoped to the actual fixes
- Don't weaken any existing test to make it pass — if a fix breaks a test, the test was probably right and the fix needs adjusting

## Response format
Same as prior phases, but scoped to fixes: for each numbered fix, what was wrong, what changed, and how you verified it's actually fixed (a test, or a real run — prefer a real run for the extraction/hallucination fixes since those are exactly the kind of thing unit tests with mocks don't catch). Full test suite + gofmt/vet at the end. Note any fix you could not safely make and why.
