-- Phase 7: the action engine -- every tool call the assistant makes on a
-- user's behalf, and the approval state of the ones that would change data.
--
-- set_updated_at() is defined in 000001 and reused here, not redefined.

CREATE TABLE actions (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id         uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    -- Where the assistant proposed or ran it. SET NULL rather than CASCADE,
    -- matching memories and knowledge_edges: deleting a conversation must not
    -- silently delete the record of what was done to the user's data in it.
    conversation_id uuid REFERENCES conversations (id) ON DELETE SET NULL,
    tool_name       text NOT NULL,
    -- The canonical, validated input -- not what the model wrote. A write is
    -- executed from this column and nothing else, so what the user approved is
    -- exactly what runs.
    input           jsonb NOT NULL,
    permission_level text NOT NULL, -- read/write
    status          text NOT NULL DEFAULT 'proposed', -- proposed/approved/rejected/executed/failed
    result          jsonb,
    error_message   text,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);

ALTER TABLE actions
    ADD CONSTRAINT actions_permission_level_check
        CHECK (permission_level IN ('read', 'write'));

ALTER TABLE actions
    ADD CONSTRAINT actions_status_check
        CHECK (status IN ('proposed', 'approved', 'rejected', 'executed', 'failed'));

-- Only a write goes through approval. A read runs when the assistant decides
-- to run it, so it is recorded already finished and can never be pending,
-- approved or rejected. This is the half of the approval rule the schema can
-- say on its own; the other half -- a write is only ever *inserted* as
-- proposed, and only leaves that state through an atomic approve -- lives in
-- internal/actions, because a CHECK sees one row, not a transition.
ALTER TABLE actions
    ADD CONSTRAINT actions_read_is_never_pending_check
        CHECK (permission_level = 'write' OR status IN ('executed', 'failed'));

-- A tool takes named arguments; anything but an object is a bug in the code
-- that built the row.
ALTER TABLE actions
    ADD CONSTRAINT actions_input_is_object_check
        CHECK (jsonb_typeof(input) = 'object');

-- The outcome columns follow the status: an executed action has a result and
-- a failed one says why, and nothing else carries either. Without this a
-- reader would have to decide what `status = 'executed'` with no result means.
ALTER TABLE actions
    ADD CONSTRAINT actions_result_check
        CHECK ((status = 'executed') = (result IS NOT NULL));

ALTER TABLE actions
    ADD CONSTRAINT actions_error_message_check
        CHECK ((status = 'failed') = (error_message IS NOT NULL));

-- Every read is scoped to the owner, so user_id leads; the composite also
-- serves a plain user_id lookup, which is why there is no standalone index on
-- it. Status is second because "what is waiting for my approval" is the query
-- a client makes most.
CREATE INDEX actions_user_id_status_idx ON actions (user_id, status, created_at DESC);

-- The conversation's own actions, which the orchestrator reads back on every
-- turn so the model knows what became of what it proposed -- and the lookup
-- the ON DELETE SET NULL needs, which a foreign key does not index by itself.
CREATE INDEX actions_conversation_id_idx ON actions (conversation_id, created_at DESC);

CREATE TRIGGER actions_set_updated_at
    BEFORE UPDATE ON actions
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
