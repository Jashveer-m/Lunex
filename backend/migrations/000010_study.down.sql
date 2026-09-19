-- Reverse of the up migration. set_updated_at() and drop_knowledge_node()
-- belong to 000001 and 000006 and are deliberately left in place.

DROP TRIGGER IF EXISTS study_plans_drop_knowledge_node ON study_plans;

-- flashcards first: it has the foreign key to study_plans.
DROP TABLE IF EXISTS flashcards;
DROP TABLE IF EXISTS study_plans;

-- The plan nodes go before the allow-list is narrowed again. DROP TABLE fires
-- no row triggers, so the nodes mirroring the plans are still here -- and ADD
-- CONSTRAINT validates the rows that exist, so leaving them would make this
-- migration fail rather than merely leave orphans.
DELETE FROM knowledge_edges
    WHERE from_node_id IN (SELECT id FROM knowledge_nodes WHERE ref_table = 'study_plans')
       OR to_node_id IN (SELECT id FROM knowledge_nodes WHERE ref_table = 'study_plans');
DELETE FROM knowledge_nodes WHERE ref_table = 'study_plans';

ALTER TABLE knowledge_nodes DROP CONSTRAINT knowledge_nodes_ref_table_check;
ALTER TABLE knowledge_nodes
    ADD CONSTRAINT knowledge_nodes_ref_table_check
        CHECK (ref_table IS NULL
               OR ref_table IN ('tasks', 'goals', 'notes', 'documents',
                                'calendar_events', 'expenses'));

-- The `project` node type stays: it was introduced by 000006 for extracted
-- nodes and is still in use by them.
