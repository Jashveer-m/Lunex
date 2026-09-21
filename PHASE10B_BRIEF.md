# Lunex — Phase 10b Brief (for Claude Code)

## Goal
Build quizzes: multiple-choice questions generated from a document/study plan, graded attempts, results stored for later use by 10c (weak-topic tracking). This is 10b of 5 (10a plans+flashcards done, 10c weak-topics, 10d spaced repetition, 10e sessions next).

## What exists already (Phase 10a + everything before)
- `internal/study` — `study_plans`, `flashcards`, the grounding pattern (`grounding.go` — generated content must trace back to source passages), generation-at-prepare-time pattern (cards/questions are generated when the tool call is prepared, not when approved, so the approval shows exactly what will be created)
- `internal/documents` — `Passages()` for ordered chunk reads
- Same tools/agents/chat integration conventions as every module since Phase 7

Follow `internal/study`'s exact pattern — this is an extension of the same package/module, not a new one. Reuse the grounding check for question/answer generation.

## Database schema (Phase 10b only)

```sql
CREATE TABLE quizzes (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id       uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    study_plan_id uuid REFERENCES study_plans(id) ON DELETE CASCADE,
    document_id   uuid REFERENCES documents(id) ON DELETE SET NULL,
    title         text NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE quiz_questions (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    quiz_id     uuid NOT NULL REFERENCES quizzes(id) ON DELETE CASCADE,
    question    text NOT NULL,
    options     jsonb NOT NULL,  -- array of strings, e.g. ["A", "B", "C", "D"]
    correct_index int NOT NULL,
    topic       text,  -- a short tag for weak-topic tracking in 10c, e.g. "mast feed timing"
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE quiz_attempts (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    quiz_id     uuid NOT NULL REFERENCES quizzes(id) ON DELETE CASCADE,
    started_at  timestamptz NOT NULL DEFAULT now(),
    completed_at timestamptz,
    score       int  -- number correct, filled on completion
);

CREATE TABLE quiz_answers (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    attempt_id     uuid NOT NULL REFERENCES quiz_attempts(id) ON DELETE CASCADE,
    question_id    uuid NOT NULL REFERENCES quiz_questions(id) ON DELETE CASCADE,
    selected_index int NOT NULL,
    correct        boolean NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now()
);
```

Index `user_id` on quizzes and attempts, `quiz_id` on questions and attempts. Use `clock_timestamp()` for `quiz_answers.created_at` if answers are inserted in a batch (per the ordering bug found in 10a).

## Generation (same grounding pattern as 10a)
`generate_quiz` tool: takes a document/study-plan reference, generates N multiple-choice questions from the passages, each proposed question's correct answer checked against the source passage before it's shown for approval — reuse `grounding.go`'s approach exactly. A wrong distractor is fine (it's supposed to be wrong); the *correct* answer must trace to the text.

## Tools

Read:
- `search_quizzes`

Write (requires approval, generated at prepare-time like flashcards):
- `generate_quiz`

Grading is NOT a write-tool-with-approval — submitting quiz answers is the user directly answering their own quiz, not the assistant proposing a change. Make it a direct API action.

## API contract

All under `RequireAuth`, scoped to caller.

- `GET/POST /api/v1/quizzes` (POST is for the chat-approval path writing the generated quiz)
- `GET/DELETE /api/v1/quizzes/:id`
- `POST /api/v1/quizzes/:id/attempts` — start an attempt
- `POST /api/v1/quiz-attempts/:id/answers` — submit an answer to one question
- `POST /api/v1/quiz-attempts/:id/complete` — finalize, compute score
- `GET /api/v1/quiz-attempts/:id` — attempt with its answers and score

Same error shape, strict decoding, 404-not-403, cross-user isolation test.

## What NOT to do
- No weak-topic aggregation across attempts yet (10c) — just store `topic` per question and `correct` per answer, don't build the analysis
- No spaced repetition scheduling (10d)
- No session/streak tracking (10e)
- Don't let a user answer a question twice in the same attempt, but don't build a full "retry quiz" flow — a new attempt is just a new row

## Response format
Same as 10a: architecture, files, migration SQL, API contract, implementation, tests (unit + cross-user isolation + grounding test for generated questions), verification commands. In verification: generate a quiz from a real document, confirm proposed not created, approve, take the quiz (answer all questions), complete it, confirm the score is correct and every correct answer traces to the source document.
