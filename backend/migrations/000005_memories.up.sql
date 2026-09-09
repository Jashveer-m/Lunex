-- Phase 5: the AI memory system -- durable facts extracted from conversations.
-- set_updated_at() is defined in 000001 and reused here, not redefined. The
-- `vector` extension is installed by 000003.

CREATE TABLE memories (
    id                     uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id                uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    -- episodic/semantic/preference/project/goal
    type                   text        NOT NULL,
    -- The fact itself, one sentence.
    content                text        NOT NULL,
    -- The model's own scores: how much this is worth keeping, and how sure it
    -- is that the extraction is accurate. Both 0.0-1.0.
    importance             real        NOT NULL DEFAULT 0.5,
    confidence             real        NOT NULL DEFAULT 0.5,
    -- Where the fact came from. SET NULL rather than CASCADE: deleting a
    -- conversation must not silently delete what the user learned in it, and
    -- the memory stays readable with its provenance simply unknown.
    source_conversation_id uuid        REFERENCES conversations (id) ON DELETE SET NULL,
    embedding              vector(768),
    -- The user can switch a memory off without losing it. Disabled memories
    -- are never retrieved; they are still listed and can be re-enabled.
    enabled                boolean     NOT NULL DEFAULT true,
    -- For a fact that should not outlive its moment. Nothing writes it in this
    -- phase; retrieval already honours it, so a later phase can populate it
    -- without a second migration. See docs/decisions.md.
    expires_at             timestamptz,
    created_at             timestamptz NOT NULL DEFAULT now(),
    updated_at             timestamptz NOT NULL DEFAULT now()
);

-- Only the five types the spec names are storable; a typo in the Go code
-- becomes a write error rather than a memory nothing ever lists by type.
ALTER TABLE memories
    ADD CONSTRAINT memories_type_check
        CHECK (type IN ('episodic', 'semantic', 'preference', 'project', 'goal'));

-- The scores are read back over the API and fed into ordering decisions, so a
-- value outside the range is a bug worth failing on rather than storing.
ALTER TABLE memories
    ADD CONSTRAINT memories_importance_check CHECK (importance >= 0 AND importance <= 1);
ALTER TABLE memories
    ADD CONSTRAINT memories_confidence_check CHECK (confidence >= 0 AND confidence <= 1);

-- Reads are always scoped to the owner, so user_id leads; a composite on
-- (user_id, ...) also serves a plain user_id lookup.
CREATE INDEX memories_user_id_created_at_idx ON memories (user_id, created_at DESC);

-- The list's ?type= filter, partial because the disabled ones are a small
-- minority and the common list is of live memories.
CREATE INDEX memories_user_id_type_idx ON memories (user_id, type) WHERE enabled;

-- The FK to conversations has no index of its own; the ON DELETE SET NULL
-- lookup needs one.
CREATE INDEX memories_source_conversation_id_idx ON memories (source_conversation_id);

-- HNSW with vector_cosine_ops, matching document_chunks in 000003 -- the same
-- embedding model produces both, the same `<=>` operator searches both, and
-- consistency here is worth more than re-deciding. See that migration for why
-- HNSW rather than IVFFlat.
--
-- Partial on `enabled` because a disabled memory is never retrieved: keeping
-- it out of the graph makes the index smaller and means switching a memory off
-- removes it from the search structure rather than merely filtering it after
-- the walk.
CREATE INDEX memories_embedding_idx
    ON memories USING hnsw (embedding vector_cosine_ops) WHERE enabled;

CREATE TRIGGER memories_set_updated_at
    BEFORE UPDATE ON memories
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
