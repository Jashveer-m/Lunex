# Lunex — Phase 10c Brief (for Claude Code)

## Goal
Aggregate quiz results by topic across all attempts, surface weak topics (ones the user repeatedly gets wrong), and make this visible both via API and proactively in chat. This is 10c of 5 (10a plans+flashcards, 10b quizzes done; 10d spaced repetition, 10e sessions next).

## What exists already (10a + 10b + everything before)
- `internal/study` — `quiz_questions.topic`, `quiz_answers.correct` — the raw data this phase aggregates. No new tables needed if the data already captures what's required; check before adding one.
- `internal/chat` — orchestrator context-building (tasks/goals/notes/calendar heuristic retrieval) — this phase adds a similar heuristic source for weak topics

## What to build

**An aggregation query/service function**, not a new table: group `quiz_answers` by `quiz_questions.topic` (joined through `quiz_questions`) for a user, computing attempts count and correct rate per topic. A topic is "weak" below some threshold (decide and justify a reasonable one, e.g. correct rate under 60% with at least 2 attempts — avoid flagging a topic on a single wrong answer).

**API:**
- `GET /api/v1/study/weak-topics` — returns topics below the threshold, sorted worst-first, with attempt count and correct rate per topic

**Chat integration — the "unique" part:** when the user asks something related to a weak topic (or asks "how am I doing" / "what should I study"), the orchestrator should be able to surface weak-topic information proactively, similar to how calendar events already get pulled into context on a heuristic basis. Add weak-topics as a light heuristic context source: if the user's message or a recently active study plan overlaps with a weak topic, mention it — don't build a separate notification system, just make it available as grounded context the model can reference (and must actually reference, not invent commentary about "weak areas" with no data behind it — same grounding discipline as every other module).

## Tools

Read only this phase — no new writes:
- `get_weak_topics`

## What NOT to do
- No new quiz-taking logic (that's done, 10b)
- No spaced repetition scheduling yet (10d) — this phase just surfaces *what* is weak, not *when* to revisit it
- No session/streak tracking (10e)
- No proactive push/notification — "surfacing in chat when relevant" means available as context, not an unprompted interruption outside a conversation

## Response format
Same as 10a/10b: architecture, files (should be small — mostly a query + one endpoint + one chat context source, no migration if no new table is needed), API contract, implementation, tests (unit + cross-user isolation + a test confirming the chat heuristic only surfaces real data, never invented commentary), verification commands. In verification: take a quiz, get some answers wrong, confirm `get_weak_topics` reflects it correctly, then ask the assistant something related and confirm it references the actual weak topic with actual numbers, not vague commentary.
