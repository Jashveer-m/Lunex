# Lunex — Phase 3 Brief (for Claude Code)

## Goal
Build document upload + RAG only: upload a file, extract text, chunk it, embed it, store in pgvector, and retrieve relevant chunks for a query with citations. No AI chat UI, no agents, no memory, no knowledge graph yet — this phase produces a retrieval function other phases will call.

## What exists already (Phase 1 + 2)
- Go backend (chi router), Postgres, golang-migrate, Argon2id auth, JWT + refresh
- `tasks`, `goals`, `notes` modules — transport → service → repository layering, `RequireAuth`, per-user isolation, 404-not-403 pattern
- Follow the same layering and testing pattern for this module

## Stack additions this phase
- **pgvector** extension in Postgres (`CREATE EXTENSION IF NOT EXISTS vector;`)
- **Ollama** for embeddings — assume it's running locally at `http://localhost:11434`. Use the `nomic-embed-text` model (768 dimensions) unless you have a strong reason to pick differently — explain if you deviate.
- **No object storage this phase.** Store extracted text in Postgres, not the original file bytes. Flag clearly in your response that original-file retrieval (re-downloading the PDF itself) is not possible until MinIO is added in a later phase — don't silently drop this capability.
- Text extraction: PDF and TXT/Markdown only this phase (DOCX, images/OCR, CSV are later — do not build them now, note them as deferred)

## Database schema (Phase 3 only)

```sql
CREATE EXTENSION IF NOT EXISTS vector;

CREATE TABLE documents (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id       uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    filename      text NOT NULL,
    file_type     text NOT NULL,        -- pdf/txt/md
    status        text NOT NULL DEFAULT 'processing',  -- processing/ready/failed
    error_message text,
    extracted_text text,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE document_chunks (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    document_id  uuid NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
    user_id      uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,  -- denormalized for scoped queries without a join
    chunk_index  int NOT NULL,
    content      text NOT NULL,
    embedding    vector(768),
    created_at   timestamptz NOT NULL DEFAULT now()
);
```

Add an ivfflat or hnsw index on `document_chunks.embedding` (your call which — explain the tradeoff briefly) and standard user_id/document_id indexes. Reuse `set_updated_at` from migration 000001.

## Processing pipeline
Upload is synchronous for this phase (no background job queue yet — that's Phase-later per the master spec, don't build it now):

```
UPLOAD → validate (size limit, type) → extract text → chunk (~500 tokens, ~50 token overlap)
  → embed each chunk via Ollama → store chunks + embeddings → mark document 'ready'
```

If extraction or embedding fails partway, mark the document `failed` with `error_message` set — don't leave it stuck in `processing`. Set a reasonable file size limit (e.g. 10MB) and reject anything larger before doing any work.

## API contract

All under `RequireAuth`, scoped to the caller (same isolation pattern as Phase 2 — write the cross-user isolation test again for this module).

- `POST /api/v1/documents` — multipart upload, returns document with status
- `GET /api/v1/documents` — list user's documents
- `GET /api/v1/documents/:id` — single document status/metadata
- `DELETE /api/v1/documents/:id` — cascades to chunks
- `POST /api/v1/documents/search` — body: `{"query": "...", "limit": 5}` → returns matching chunks with document filename, chunk content, and similarity score. This is the retrieval function later phases (AI chat) will call — build it as a clean service-layer function too, not just an HTTP handler, so Phase 6 can call it directly.

Same error shape and strict decoding as Phase 1/2.

## What NOT to do
- No AI chat, no LLM calls beyond embeddings, no agents, no orchestrator
- No object storage (MinIO/S3) — extracted text only, flagged as a known gap
- No DOCX, image/OCR, or CSV parsing — PDF and TXT/MD only
- No background job queue — synchronous processing is fine for this phase
- No reranking — plain vector similarity search is enough for now

## Response format
Same as Phase 1/2: architecture, files, migration SQL, API contract, implementation, tests (including cross-user isolation), verification commands. In verification, include a real end-to-end check: upload a small text file, then search for a phrase from it, and confirm the chunk comes back.
