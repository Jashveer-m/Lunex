-- Phase 9: the finance module -- what the user spent, on what, and its place in
-- the knowledge graph.
--
-- set_updated_at() is defined in 000001 and drop_knowledge_node() in 000006;
-- both are reused here, not redefined.
--
-- There is no currency conversion and no rate table. An expense stores the
-- currency it was incurred in and nothing ever converts it, so a total is only
-- ever a total *per currency* -- which is why the summary query below groups by
-- it. See docs/decisions.md.

CREATE TABLE expense_categories (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name       text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- One "Food" per user, and "food" is the same category.
--
-- The uniqueness is what makes a category name safe to resolve by hand ("log it
-- under food") and what stops the seed below from running twice. It is
-- case-insensitive because the resolution is: a plain UNIQUE (user_id, name)
-- would let "Food" and "food" both exist, and then "log it under food" matches
-- two categories and a spending breakdown reports one category as two lines.
-- The name is still stored as the user spelled it; only the comparison ignores
-- case. It is the call 000001 makes for users.email, for the same reason.
CREATE UNIQUE INDEX expense_categories_user_id_name_key
    ON expense_categories (user_id, lower(name));

CREATE TABLE expenses (
    id       uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id  uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    -- numeric, never a float: money is exact, and 12,2 holds ten digits before
    -- the point, which is more than a personal ledger needs and still inside an
    -- int64 of hundredths -- which is how Go carries it. The CHECK is `> 0`:
    -- an expense of nothing is a row nobody meant to write, and a refund is not
    -- modelled this phase.
    amount   numeric(12,2) NOT NULL CHECK (amount > 0),
    -- Stored as given. Nothing converts it; see the header.
    currency text NOT NULL DEFAULT 'INR',
    -- SET NULL rather than CASCADE: deleting a category must not delete the
    -- spending filed under it. The money was still spent.
    category_id  uuid REFERENCES expense_categories (id) ON DELETE SET NULL,
    description  text,
    -- A date, not a timestamp. What people record is "I spent this on Tuesday",
    -- and a timestamptz would invent a time of day and a timezone for it --
    -- which would then make "this month" depend on which one.
    expense_date date NOT NULL,
    -- A receipt or invoice already uploaded. It links, and nothing more: no
    -- parsing, no extraction. SET NULL for the same reason as the category.
    related_document_id uuid REFERENCES documents (id) ON DELETE SET NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- Every read of this table is "my expenses, over these dates" -- the list, the
-- summary and both tools -- so the one index that matters leads with the owner
-- and then the date.
--
-- It also serves a plain user_id lookup on its own, including the one the
-- ON DELETE CASCADE from users makes, which is why there is no standalone
-- user_id index beside it: the same call 000006 made for knowledge_nodes and
-- 000008 for calendar_events.
CREATE INDEX expenses_user_id_expense_date_idx ON expenses (user_id, expense_date);

-- A foreign key does not index itself, and both of these are ON DELETE SET
-- NULL: without the indexes, deleting one category or one document scans every
-- expense.
CREATE INDEX expenses_category_id_idx ON expenses (category_id);
CREATE INDEX expenses_related_document_id_idx ON expenses (related_document_id);

CREATE TRIGGER expenses_set_updated_at
    BEFORE UPDATE ON expenses
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- --- the default categories ---------------------------------------------------
--
-- Every user starts with the same five, and they are seeded by a trigger on
-- `users` rather than by the registration code or by a one-off INSERT here.
--
-- A one-off INSERT would only cover the users who exist the moment this
-- migration runs; everyone who registers afterwards would get nothing, so it
-- would have to be paired with application code anyway.
--
-- Doing it in that application code -- users.Repository.Create, which already
-- inserts the user and its profile in one transaction -- would make the users
-- package depend on the finance schema, and would only fire on the one path
-- that remembered to call it. A trigger makes "every user has the default
-- categories" true of the table whatever creates a user: the API, a psql
-- session, a fixture, a future admin import. It is the argument
-- drop_knowledge_node makes in 000006, applied to an insert.
--
-- Lazy creation on first use was the other candidate and is worse for a reason
-- that is not about correctness: an empty category list gives a new user a
-- "pick a category" control with nothing in it, and gives create_expense
-- nothing to resolve "food" against -- so the very first expense would be
-- filed under nothing. See docs/decisions.md.
--
-- ON CONFLICT DO NOTHING so the backfill below and the trigger cannot collide,
-- and so a user re-created with the same id is not an error.
CREATE OR REPLACE FUNCTION seed_default_expense_categories() RETURNS trigger AS $$
BEGIN
    INSERT INTO expense_categories (user_id, name)
    SELECT NEW.id, name
    FROM unnest(ARRAY['Food', 'Transport', 'Housing', 'Utilities', 'Other']) AS name
    ON CONFLICT (user_id, lower(name)) DO NOTHING;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER users_seed_default_expense_categories
    AFTER INSERT ON users
    FOR EACH ROW EXECUTE FUNCTION seed_default_expense_categories();

-- The users who already exist. The trigger only fires on inserts from here on,
-- so without this the accounts that predate Phase 9 would be the only ones
-- without categories.
INSERT INTO expense_categories (user_id, name)
SELECT u.id, c.name
FROM users u
CROSS JOIN unnest(ARRAY['Food', 'Transport', 'Housing', 'Utilities', 'Other']) AS c(name)
ON CONFLICT (user_id, lower(name)) DO NOTHING;

-- --- the knowledge graph -------------------------------------------------------
--
-- An expense is a thing the user has, so it gets a node like a task, a goal, a
-- note, a document or an event. Categories do not: a category is a label on
-- other rows, not a thing that happened, and a node per category would add five
-- nodes to every account that the mention scan would then match on the word
-- "food".
--
-- Both allow-lists in 000006 are closed sets, so both have to be widened before
-- a node can be written -- which is the point of them: a new mirrored table is
-- a migration, not a string in the Go code.

ALTER TABLE knowledge_nodes DROP CONSTRAINT knowledge_nodes_type_check;
ALTER TABLE knowledge_nodes
    ADD CONSTRAINT knowledge_nodes_type_check
        CHECK (type IN ('task', 'goal', 'note', 'document', 'event', 'expense',
                        'skill', 'person', 'project'));

ALTER TABLE knowledge_nodes DROP CONSTRAINT knowledge_nodes_ref_table_check;
ALTER TABLE knowledge_nodes
    ADD CONSTRAINT knowledge_nodes_ref_table_check
        CHECK (ref_table IS NULL
               OR ref_table IN ('tasks', 'goals', 'notes', 'documents',
                                'calendar_events', 'expenses'));

-- The same trigger the five mirrored tables carry, for the same reason: an
-- expense can be deleted by a path the finance service never sees -- the user
-- being deleted, a psql session -- and a rule that only fires where somebody
-- remembered to call it is a rule that leaves orphans.
CREATE TRIGGER expenses_drop_knowledge_node
    AFTER DELETE ON expenses
    FOR EACH ROW EXECUTE FUNCTION drop_knowledge_node('expenses');
