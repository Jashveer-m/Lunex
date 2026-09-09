-- Reverse order of the up migration. The `vector` extension is deliberately
-- left installed: dropping it would take any other schema's vector columns
-- with it, and installing an extension needs privileges a rollback may not
-- have.
DROP TABLE IF EXISTS document_chunks;
DROP TABLE IF EXISTS documents;
