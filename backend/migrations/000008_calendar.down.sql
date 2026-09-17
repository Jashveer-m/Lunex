-- Reverse of the up migration. set_updated_at() and drop_knowledge_node()
-- belong to 000001 and 000006 and are deliberately left in place.

DROP TRIGGER IF EXISTS calendar_events_drop_knowledge_node ON calendar_events;
DROP TABLE IF EXISTS calendar_events;

-- The event nodes go before the allow-lists are narrowed again. DROP TABLE
-- fires no row triggers, so the nodes mirroring the events are still here --
-- and ADD CONSTRAINT validates the rows that exist, so leaving them would make
-- this migration fail rather than merely leave orphans.
DELETE FROM knowledge_edges
    WHERE from_node_id IN (SELECT id FROM knowledge_nodes WHERE ref_table = 'calendar_events')
       OR to_node_id IN (SELECT id FROM knowledge_nodes WHERE ref_table = 'calendar_events');
DELETE FROM knowledge_nodes WHERE ref_table = 'calendar_events';

ALTER TABLE knowledge_nodes DROP CONSTRAINT knowledge_nodes_ref_table_check;
ALTER TABLE knowledge_nodes
    ADD CONSTRAINT knowledge_nodes_ref_table_check
        CHECK (ref_table IS NULL OR ref_table IN ('tasks', 'goals', 'notes', 'documents'));

ALTER TABLE knowledge_nodes DROP CONSTRAINT knowledge_nodes_type_check;
ALTER TABLE knowledge_nodes
    ADD CONSTRAINT knowledge_nodes_type_check
        CHECK (type IN ('task', 'goal', 'note', 'document', 'skill', 'person', 'project'));
