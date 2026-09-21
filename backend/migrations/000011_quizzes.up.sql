-- Phase 10b: quizzes -- multiple-choice questions written from a document or a
-- study plan, and the graded attempts a user makes at them.
--
-- This is the second slice of the study module and it reuses 000010's shapes
-- rather than inventing new ones: the same nullable `study_plan_id` and
-- `document_id` a flashcard carries, with the same two different ON DELETE
-- rules and for the same reasons.
--
-- What this migration deliberately does not have. There is no weak-topic
-- table and no per-topic counter (10c), no due date, interval or ease factor
-- (10d), and no session or streak row (10e). `quiz_questions.topic` and
-- `quiz_answers.correct` are the two columns 10c will read; they are recorded
-- here and nothing aggregates them, which is the whole difference between
-- storing the evidence and building the analysis. See docs/decisions.md.

CREATE TABLE quizzes (
    id      uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    -- CASCADE, like a flashcard's: a quiz filed under a plan is part of that
    -- plan, and deleting the plan is how a user throws the whole thing away.
    -- A quiz under no plan is one made straight from a document, which is
    -- allowed.
    study_plan_id uuid REFERENCES study_plans (id) ON DELETE CASCADE,
    -- SET NULL, like a flashcard's: the document is where the questions came
    -- from, and deleting the PDF must not delete the attempts somebody has
    -- already sat. The quiz survives its source; what it loses is the link.
    document_id uuid REFERENCES documents (id) ON DELETE SET NULL,
    title      text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- Every read of this table is "my quizzes", so user_id leads.
CREATE INDEX quizzes_user_id_idx ON quizzes (user_id);
-- Both foreign keys are indexed because neither indexes itself and both are
-- walked on delete -- the plan's by the cascade, the document's by SET NULL.
CREATE INDEX quizzes_study_plan_id_idx ON quizzes (study_plan_id);
CREATE INDEX quizzes_document_id_idx ON quizzes (document_id);

CREATE TABLE quiz_questions (
    id      uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    quiz_id uuid NOT NULL REFERENCES quizzes (id) ON DELETE CASCADE,
    question text NOT NULL,
    -- A JSON array of strings: ["A", "B", "C", "D"]. jsonb rather than text[]
    -- because it is read and written whole and never searched into, and
    -- because it is what the brief's contract names; the shape is validated in
    -- Go (2..6 options, none blank, none repeated) for the reason the status
    -- columns are, which migration 000010 gives.
    options jsonb NOT NULL,
    -- Which of them is right, as an index into options. Not a CHECK against
    -- jsonb_array_length: the bound is validated in Go alongside the options
    -- themselves, where a bad value is a field error naming the field rather
    -- than a 500 out of the driver.
    correct_index int NOT NULL,
    -- A short tag for the thing this question is about ("mast feed timing").
    -- Nothing in this phase reads it; 10c does, and a tag recorded per
    -- question is the evidence that phase needs. Nullable, because a model
    -- that does not name one must not have a name invented for it.
    topic text,
    -- clock_timestamp(), not now(): the questions of one quiz are inserted in
    -- one transaction, and now() is the transaction's start time, so every row
    -- would carry the identical stamp and the read order would fall to the
    -- tie-break on a random uuid. That shuffles a quiz out of the order it was
    -- written in -- and therefore out of the order of the document it came
    -- from. It is the same call flashcards makes, for the same reason; the
    -- default is here so a row inserted by any other path gets it too.
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

-- The one read this table has is "the questions in this quiz", and the cascade
-- from quizzes walks the same column.
CREATE INDEX quiz_questions_quiz_id_idx ON quiz_questions (quiz_id);

CREATE TABLE quiz_attempts (
    id      uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    quiz_id uuid NOT NULL REFERENCES quizzes (id) ON DELETE CASCADE,
    started_at   timestamptz NOT NULL DEFAULT now(),
    -- Null until the attempt is finalised. It is what makes "in progress" and
    -- "finished" one column rather than a status word that can disagree with
    -- the score beside it.
    completed_at timestamptz,
    -- How many were right, filled on completion. Null while the attempt is
    -- open: a half-finished attempt has no score, and 0 would read as one.
    score int
);

CREATE INDEX quiz_attempts_user_id_idx ON quiz_attempts (user_id);
CREATE INDEX quiz_attempts_quiz_id_idx ON quiz_attempts (quiz_id);

CREATE TABLE quiz_answers (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    attempt_id uuid NOT NULL REFERENCES quiz_attempts (id) ON DELETE CASCADE,
    question_id uuid NOT NULL REFERENCES quiz_questions (id) ON DELETE CASCADE,
    selected_index int NOT NULL,
    -- Graded when the answer is submitted, not worked out again on read. The
    -- question's correct_index can only change by the question being deleted,
    -- which takes this row with it -- so the stored verdict cannot drift from
    -- the question, and 10c reads one column instead of a join.
    correct boolean NOT NULL,
    -- clock_timestamp() for the same reason quiz_questions has it. Answers are
    -- inserted one per request today, so now() would do; the default is what
    -- keeps the ordering right if a batch path is ever added, which is exactly
    -- the assumption that broke the deck read in 10a.
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

-- One answer per question per attempt, enforced here rather than by the
-- service checking first. The service does check -- it has a better error to
-- give -- but a check is a race and a constraint is not: two concurrent
-- submissions of the same question would both pass a SELECT and both insert,
-- and the attempt would be scored out of more answers than it has questions.
CREATE UNIQUE INDEX quiz_answers_attempt_question_idx ON quiz_answers (attempt_id, question_id);
-- The cascade from quiz_questions walks this column, and nothing else does.
CREATE INDEX quiz_answers_question_id_idx ON quiz_answers (question_id);

-- --- the knowledge graph -------------------------------------------------------
--
-- A quiz gets no node, and neither does an attempt. The allow-list is
-- untouched and there is no trigger on these tables.
--
-- The rule 000010 set is that the *plan* is the thing worth connecting and the
-- cards hang off it; a quiz is the same kind of thing as a deck -- content
-- generated from a document, under a plan that already has a node -- and a
-- node per quiz would put a second label for the same subject in the graph,
-- which is what makes the mention scan fire twice on one word. An attempt is a
-- worse candidate still: it is an event, not a thing the user has, and there
-- would be one of them every time they sat down.
