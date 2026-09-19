-- Phase 10a: the study module's first slice -- study plans, and the flashcards
-- generated from the user's own documents.
--
-- set_updated_at() is defined in 000001 and drop_knowledge_node() in 000006;
-- both are reused here, not redefined.
--
-- What this migration deliberately does not have: no quizzes, no per-card
-- review state, no `correct`/`incorrect` counter, no due date and no interval.
-- Those belong to 10b-10e, and a column added now would either sit unread for
-- three phases or be the wrong column when they arrive. See docs/decisions.md.

CREATE TABLE study_plans (
    id      uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    title   text NOT NULL,
    description text,
    -- The document the plan is built from, when it is built from one. A plan
    -- can also be a plan for something with no file behind it ("finals
    -- revision"), which is why this is nullable.
    --
    -- SET NULL rather than CASCADE: deleting the PDF must not delete the plan
    -- and, through the cascade below, every card the user has made from it.
    -- The studying happened; the source file is a convenience.
    document_id uuid REFERENCES documents (id) ON DELETE SET NULL,
    -- active/completed/abandoned. Not a CHECK, for the same reason tasks.status
    -- and goals.status are not: the closed set is enforced in Go, where a bad
    -- value is a field error naming the field rather than a 500 out of the
    -- driver, and internal/study's validation is the single place it is
    -- written down.
    status     text NOT NULL DEFAULT 'active',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- Every read of this table is "my plans", so user_id leads. A plain user_id
-- index rather than a composite: the list's default order is by recency, but
-- a user has tens of plans rather than thousands, and the second column would
-- buy nothing the sort does not already get for free.
CREATE INDEX study_plans_user_id_idx ON study_plans (user_id);
-- A foreign key does not index itself, and this one is ON DELETE SET NULL:
-- without the index, deleting one document scans every plan.
CREATE INDEX study_plans_document_id_idx ON study_plans (document_id);

CREATE TRIGGER study_plans_set_updated_at
    BEFORE UPDATE ON study_plans
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE flashcards (
    id      uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    -- CASCADE, unlike the document link: a card filed under a plan is part of
    -- that plan, and deleting the plan is how a user throws the deck away.
    -- A card with no plan is one added on its own, which is allowed.
    study_plan_id uuid REFERENCES study_plans (id) ON DELETE CASCADE,
    -- Where the card's content came from, when it was generated rather than
    -- typed. It is the provenance of the answer on the back, which is the
    -- whole claim this phase makes about a generated card -- so it survives
    -- the plan being deleted and is SET NULL only when the document itself is.
    document_id uuid REFERENCES documents (id) ON DELETE SET NULL,
    front text NOT NULL,
    back  text NOT NULL,
    -- No updated_at and no trigger: a card is created and deleted, never
    -- edited, this phase. There is nothing for the column to record.
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX flashcards_user_id_idx ON flashcards (user_id);
-- The one read this table has is "the cards in this plan", and the cascade
-- from study_plans walks the same column.
CREATE INDEX flashcards_study_plan_id_idx ON flashcards (study_plan_id);
CREATE INDEX flashcards_document_id_idx ON flashcards (document_id);

-- --- the knowledge graph -------------------------------------------------------
--
-- A study plan is a thing the user has, so it gets a node like a task, a goal,
-- a note, a document, an event or an expense. Its node type is `project`,
-- which already exists: a plan is a piece of work being worked towards, which
-- is exactly what 000006 introduced `project` for. `skill` was the other
-- candidate and is wrong -- a plan is not the subject it is about, and a node
-- labelled "Linear algebra" standing for a plan would collide with the `skill`
-- node an extraction writes for the subject itself.
--
-- Flashcards get no node. A deck is hundreds of rows of one or two sentences;
-- a node per card would swamp the graph and make the mention scan fire on
-- every question that happens to share a word with an answer. The plan is the
-- thing worth connecting, and the cards hang off it.
--
-- Only the ref_table allow-list has to be widened -- `project` is already in
-- the type one -- and widening it is a migration rather than a string in the
-- Go code, which is the point of the constraint.

ALTER TABLE knowledge_nodes DROP CONSTRAINT knowledge_nodes_ref_table_check;
ALTER TABLE knowledge_nodes
    ADD CONSTRAINT knowledge_nodes_ref_table_check
        CHECK (ref_table IS NULL
               OR ref_table IN ('tasks', 'goals', 'notes', 'documents',
                                'calendar_events', 'expenses', 'study_plans'));

-- The same trigger the six mirrored tables carry, for the same reason: a plan
-- can be deleted by a path the study service never sees -- the user being
-- deleted, a psql session -- and a rule that only fires where somebody
-- remembered to call it is a rule that leaves orphans.
CREATE TRIGGER study_plans_drop_knowledge_node
    AFTER DELETE ON study_plans
    FOR EACH ROW EXECUTE FUNCTION drop_knowledge_node('study_plans');
