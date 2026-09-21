-- Reverse of the up migration.
--
-- In dependency order: answers reference attempts and questions, both of which
-- reference quizzes. No trigger and no constraint to restore -- 000011 adds
-- neither, because a quiz gets no knowledge-graph node.

DROP TABLE IF EXISTS quiz_answers;
DROP TABLE IF EXISTS quiz_attempts;
DROP TABLE IF EXISTS quiz_questions;
DROP TABLE IF EXISTS quizzes;
