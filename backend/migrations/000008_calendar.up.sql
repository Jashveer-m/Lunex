-- Phase 8: the calendar -- the events a user has, and their place in the
-- knowledge graph.
--
-- set_updated_at() is defined in 000001 and reused here, not redefined.
--
-- There is no recurrence expansion. recurrence_rule is stored as the opaque
-- string it arrives as; nothing in this phase reads it, and no row here stands
-- for a repeated instance. See docs/decisions.md.

CREATE TABLE calendar_events (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    title       text NOT NULL,
    description text,
    -- Both ends are stored, always. An all-day event is a row whose ends are
    -- midnight and midnight the next day with all_day set, rather than a row
    -- with a null end: a range query that has to special-case a missing end is
    -- a range query that gets it wrong somewhere.
    start_time  timestamptz NOT NULL,
    end_time    timestamptz NOT NULL,
    all_day     boolean NOT NULL DEFAULT false,
    location    text,
    -- An RRULE string, or anything else a client wants to keep. Opaque.
    recurrence_rule text,
    -- What the event is for, when it is for something. SET NULL rather than
    -- CASCADE: deleting the task you booked the time for should not silently
    -- delete the hour in your calendar.
    related_task_id uuid REFERENCES tasks (id) ON DELETE SET NULL,
    related_goal_id uuid REFERENCES goals (id) ON DELETE SET NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    -- An event that ends before it starts is not an event. The service checks
    -- this too, so the API answers a field-level 400 rather than a 500 -- this
    -- is what makes the rule true of the table whatever writes to it.
    CHECK (end_time >= start_time)
);

-- The only read this table has is "what is on between X and Y, for me", so the
-- one index that matters leads with the owner and then the start.
--
-- It also serves a plain user_id lookup on its own -- including the one the
-- ON DELETE CASCADE from users makes -- which is why there is no standalone
-- user_id index beside it, the same reasoning as knowledge_nodes in 000006.
CREATE INDEX calendar_events_user_id_start_time_idx
    ON calendar_events (user_id, start_time);

-- A foreign key does not index itself, and both of these are ON DELETE SET
-- NULL: without the indexes, deleting one task scans every event.
CREATE INDEX calendar_events_related_task_id_idx ON calendar_events (related_task_id);
CREATE INDEX calendar_events_related_goal_id_idx ON calendar_events (related_goal_id);

CREATE TRIGGER calendar_events_set_updated_at
    BEFORE UPDATE ON calendar_events
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- --- the knowledge graph -----------------------------------------------------
--
-- An event is a thing the user has, so it gets a node like a task, a goal, a
-- note or a document. Both allow-lists in 000006 are closed sets, so both have
-- to be widened before a node can be written -- which is the point of them: a
-- new mirrored table is a migration, not a string in the Go code.

ALTER TABLE knowledge_nodes DROP CONSTRAINT knowledge_nodes_type_check;
ALTER TABLE knowledge_nodes
    ADD CONSTRAINT knowledge_nodes_type_check
        CHECK (type IN ('task', 'goal', 'note', 'document', 'event',
                        'skill', 'person', 'project'));

ALTER TABLE knowledge_nodes DROP CONSTRAINT knowledge_nodes_ref_table_check;
ALTER TABLE knowledge_nodes
    ADD CONSTRAINT knowledge_nodes_ref_table_check
        CHECK (ref_table IS NULL
               OR ref_table IN ('tasks', 'goals', 'notes', 'documents', 'calendar_events'));

-- The same trigger the four Phase 6 tables carry, for the same reason: an
-- event can be deleted by a path the calendar service never sees -- the user
-- being deleted, a psql session -- and a rule that only fires where somebody
-- remembered to call it is a rule that leaves orphans.
CREATE TRIGGER calendar_events_drop_knowledge_node
    AFTER DELETE ON calendar_events
    FOR EACH ROW EXECUTE FUNCTION drop_knowledge_node('calendar_events');
