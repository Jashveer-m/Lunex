-- Reverse of the up migration. set_updated_at() belongs to 000001 and is
-- deliberately left in place.
--
-- The triggers are dropped explicitly, before the function they call: they sit
-- on the Phase 2 and Phase 3 tables, which this migration does not own and
-- must leave exactly as it found them.
DROP TRIGGER IF EXISTS tasks_drop_knowledge_node ON tasks;
DROP TRIGGER IF EXISTS goals_drop_knowledge_node ON goals;
DROP TRIGGER IF EXISTS notes_drop_knowledge_node ON notes;
DROP TRIGGER IF EXISTS documents_drop_knowledge_node ON documents;
DROP FUNCTION IF EXISTS drop_knowledge_node();

-- Edges first: they reference nodes.
DROP TABLE IF EXISTS knowledge_edges;
DROP TABLE IF EXISTS knowledge_nodes;
