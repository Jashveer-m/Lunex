-- Phase 3: document upload + retrieval (RAG).
-- set_updated_at() is defined in 000001 and reused here, not redefined.

-- pgvector supplies the `vector` column type and the distance operators the
-- similarity search uses. IF NOT EXISTS keeps the migration idempotent on a
-- database where an operator already enabled it.
CREATE EXTENSION IF NOT EXISTS vector;

CREATE TABLE documents (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id        uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    filename       text        NOT NULL,
    file_type      text        NOT NULL,                   -- pdf/txt/md
    status         text        NOT NULL DEFAULT 'processing', -- processing/ready/failed
    error_message  text,
    -- The extracted plain text, not the uploaded bytes: there is no object
    -- store in this phase, so re-downloading the original file is impossible
    -- by construction. See docs/decisions.md.
    extracted_text text,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now()
);

-- Only the three states the pipeline can produce are storable; a typo in the
-- Go code becomes a write error rather than a document nothing ever lists.
ALTER TABLE documents
    ADD CONSTRAINT documents_status_check CHECK (status IN ('processing', 'ready', 'failed'));

-- Reads are always scoped to the owner, so user_id leads; a composite on
-- (user_id, ...) also serves a plain user_id lookup.
CREATE INDEX documents_user_id_created_at_idx ON documents (user_id, created_at DESC);
CREATE INDEX documents_user_id_status_idx ON documents (user_id, status);

CREATE TRIGGER documents_set_updated_at
    BEFORE UPDATE ON documents
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE document_chunks (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    document_id uuid        NOT NULL REFERENCES documents (id) ON DELETE CASCADE,
    -- Denormalized so the similarity search filters on one table: the ORDER BY
    -- is an index scan over document_chunks, and joining documents just to
    -- learn the owner would force that filter to happen after the scan.
    user_id     uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    chunk_index int         NOT NULL,
    content     text        NOT NULL,
    embedding   vector(768),
    created_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (document_id, chunk_index)
);

CREATE INDEX document_chunks_user_id_idx ON document_chunks (user_id);
-- The FK to documents has no index of its own; the cascade delete and
-- "chunks of this document" both need one.
CREATE INDEX document_chunks_document_id_idx ON document_chunks (document_id);

-- HNSW rather than IVFFlat.
--
-- IVFFlat clusters the existing rows into `lists` centroids at build time, so
-- an index created on an empty table -- which is exactly what a migration
-- does -- has no useful clustering and has to be rebuilt once real data
-- arrives. HNSW builds its graph incrementally as rows are inserted, needs no
-- training pass and no REINDEX, and gives better recall at the same speed.
--
-- The costs are real but the wrong ones to optimize here: HNSW uses more
-- memory and inserts are slower than IVFFlat's. Inserts happen once per
-- uploaded document, searches happen on every query.
--
-- vector_cosine_ops matches the `<=>` operator the search uses. nomic-embed-text
-- does not return unit-length vectors, so cosine -- not L2 or inner product --
-- is the distance that means "similar text".
CREATE INDEX document_chunks_embedding_idx
    ON document_chunks USING hnsw (embedding vector_cosine_ops);
