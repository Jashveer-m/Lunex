-- Reverse of the up migration. set_updated_at() and drop_knowledge_node()
-- belong to 000001 and 000006 and are deliberately left in place;
-- seed_default_expense_categories() belongs to this migration and goes.

DROP TRIGGER IF EXISTS expenses_drop_knowledge_node ON expenses;
DROP TRIGGER IF EXISTS users_seed_default_expense_categories ON users;
DROP FUNCTION IF EXISTS seed_default_expense_categories();

-- expenses first: it has the foreign key to expense_categories.
DROP TABLE IF EXISTS expenses;
DROP TABLE IF EXISTS expense_categories;

-- The expense nodes go before the allow-lists are narrowed again. DROP TABLE
-- fires no row triggers, so the nodes mirroring the expenses are still here --
-- and ADD CONSTRAINT validates the rows that exist, so leaving them would make
-- this migration fail rather than merely leave orphans.
DELETE FROM knowledge_edges
    WHERE from_node_id IN (SELECT id FROM knowledge_nodes WHERE ref_table = 'expenses')
       OR to_node_id IN (SELECT id FROM knowledge_nodes WHERE ref_table = 'expenses');
DELETE FROM knowledge_nodes WHERE ref_table = 'expenses';

ALTER TABLE knowledge_nodes DROP CONSTRAINT knowledge_nodes_ref_table_check;
ALTER TABLE knowledge_nodes
    ADD CONSTRAINT knowledge_nodes_ref_table_check
        CHECK (ref_table IS NULL
               OR ref_table IN ('tasks', 'goals', 'notes', 'documents', 'calendar_events'));

ALTER TABLE knowledge_nodes DROP CONSTRAINT knowledge_nodes_type_check;
ALTER TABLE knowledge_nodes
    ADD CONSTRAINT knowledge_nodes_type_check
        CHECK (type IN ('task', 'goal', 'note', 'document', 'event',
                        'skill', 'person', 'project'));
