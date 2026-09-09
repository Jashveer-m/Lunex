-- Phase 4: AI chat -- conversations and the messages in them.
-- set_updated_at() is defined in 000001 and reused here, not redefined.

CREATE TABLE conversations (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    title      text        NOT NULL DEFAULT 'New conversation',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- Every read is scoped to the owner, so user_id leads; a composite on
-- (user_id, ...) also serves a plain user_id lookup. updated_at DESC is the
-- default list order -- a conversation list is a list of things to continue.
CREATE INDEX conversations_user_id_updated_at_idx ON conversations (user_id, updated_at DESC);

CREATE TRIGGER conversations_set_updated_at
    BEFORE UPDATE ON conversations
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE messages (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    conversation_id uuid        NOT NULL REFERENCES conversations (id) ON DELETE CASCADE,
    role            text        NOT NULL, -- user/assistant/system
    -- The document/task/goal/note references that grounded this answer, with
    -- the label the prompt showed the model and whether the answer actually
    -- cited it. NULL, not '[]', when retrieval never ran -- as on every user
    -- message -- so "grounded in nothing" stays distinguishable from "not an
    -- answer".
    sources         jsonb,
    content         text        NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now()
);

-- Only the three roles the API can produce are storable; a typo in the Go code
-- becomes a write error rather than a message no client knows how to render.
ALTER TABLE messages
    ADD CONSTRAINT messages_role_check CHECK (role IN ('user', 'assistant', 'system'));

-- messages has no user_id: ownership hangs off the conversation, so every
-- statement joins conversations and filters user_id there. This index is what
-- makes that join and the "last N messages" read one scan, and it also serves
-- the ON DELETE CASCADE lookup, which the foreign key does not index by itself.
CREATE INDEX messages_conversation_id_created_at_idx ON messages (conversation_id, created_at, id);
