-- Phase 6: the personal knowledge graph -- the things a user has, and the
-- edges between them.
--
-- set_updated_at() is defined in 000001 and reused here, not redefined. There
-- is no graph database: this is two ordinary tables, and every query in
-- internal/graph is one join deep.

CREATE TABLE knowledge_nodes (
    id      uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    -- task/goal/note/document/skill/person/project
    type    text NOT NULL,
    -- The display name: a task's title, or the name a conversation used for a
    -- skill, a person or a project.
    label   text NOT NULL,
    -- The row this node mirrors, or NULL for a node that exists only because a
    -- conversation named it. The two columns are one fact and move together.
    ref_table  text,
    ref_id     uuid,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- Only the seven node types the spec scopes to this phase are storable; a typo
-- in the Go code becomes a write error rather than a node no client can render.
ALTER TABLE knowledge_nodes
    ADD CONSTRAINT knowledge_nodes_type_check
        CHECK (type IN ('task', 'goal', 'note', 'document', 'skill', 'person', 'project'));

-- A node either mirrors a row somewhere or it does not. Half a reference --
-- a table with no id, an id with no table -- is not a state any reader should
-- have to handle, and it is the shape a bug in the sync path would produce.
ALTER TABLE knowledge_nodes
    ADD CONSTRAINT knowledge_nodes_ref_check
        CHECK ((ref_table IS NULL) = (ref_id IS NULL));

-- ref_table is not a foreign key -- it cannot be, it names four different
-- tables -- so the allow-list is what stops it becoming free text. Deletion
-- still cascades correctly because the *node* is removed when its source row
-- goes, by the sync path in internal/graph and, for the user, by the FK above.
ALTER TABLE knowledge_nodes
    ADD CONSTRAINT knowledge_nodes_ref_table_check
        CHECK (ref_table IS NULL OR ref_table IN ('tasks', 'goals', 'notes', 'documents'));

-- One node per source row. Sync-on-write runs on every create *and* every
-- update, so it is called repeatedly for the same task on purpose; this is
-- what makes the repeat an upsert rather than a duplicate.
CREATE UNIQUE INDEX knowledge_nodes_ref_key
    ON knowledge_nodes (user_id, ref_table, ref_id) WHERE ref_table IS NOT NULL;

-- And one node per extracted name. Matching happens in the application too
-- (see internal/graph.Service.resolve), but two turns extracted concurrently
-- would both find nothing and both insert; the constraint is what makes "Go
-- mentioned twice is one node" a property of the database rather than of a
-- lucky interleaving. lower(label) because the match is case-insensitive.
CREATE UNIQUE INDEX knowledge_nodes_extracted_label_key
    ON knowledge_nodes (user_id, type, lower(label)) WHERE ref_table IS NULL;

-- Every read is scoped to the owner, so user_id leads each index; a composite
-- on (user_id, ...) also serves a plain user_id lookup, which is why there is
-- no standalone user_id index here.
CREATE INDEX knowledge_nodes_user_id_type_idx ON knowledge_nodes (user_id, type);
-- Resolving an extracted label, and finding the nodes a chat message mentions,
-- are both lookups on the folded label.
CREATE INDEX knowledge_nodes_user_id_label_idx ON knowledge_nodes (user_id, lower(label));

CREATE TRIGGER knowledge_nodes_set_updated_at
    BEFORE UPDATE ON knowledge_nodes
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE knowledge_edges (
    id           uuid NOT NULL PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    from_node_id uuid NOT NULL REFERENCES knowledge_nodes (id) ON DELETE CASCADE,
    to_node_id   uuid NOT NULL REFERENCES knowledge_nodes (id) ON DELETE CASCADE,
    relationship text NOT NULL,
    -- The model's own estimate that it read the relationship rather than
    -- inferred it, 0.0-1.0. Recorded rather than trusted: it is shown to the
    -- model on retrieval and filtered on at extraction, and nothing ranks by it.
    confidence   real NOT NULL DEFAULT 0.5,
    -- Where the edge came from. SET NULL rather than CASCADE, matching
    -- memories in 000005: deleting a conversation must not silently delete
    -- what was learned in it.
    source_conversation_id uuid REFERENCES conversations (id) ON DELETE SET NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    -- A node related to itself is not a relationship.
    CHECK (from_node_id != to_node_id)
);

ALTER TABLE knowledge_edges
    ADD CONSTRAINT knowledge_edges_relationship_check
        CHECK (relationship IN ('RELATED_TO', 'REQUIRES', 'DEPENDS_ON', 'WORKS_ON',
                                'KNOWS', 'INTERESTED_IN', 'STUDIES', 'COMPLETED', 'GOAL_OF'));

ALTER TABLE knowledge_edges
    ADD CONSTRAINT knowledge_edges_confidence_check
        CHECK (confidence >= 0 AND confidence <= 1);

-- The same relationship between the same two things, stated in two
-- conversations, is one edge. Without this the graph would grow a parallel
-- edge every time the user mentioned the same pair again, and the 1-hop
-- lookup would hand the model the same line five times. The insert is an
-- upsert that keeps the higher confidence; see internal/graph.Repository.
CREATE UNIQUE INDEX knowledge_edges_triple_key
    ON knowledge_edges (user_id, from_node_id, to_node_id, relationship);

-- The triple index leads with user_id, so it serves the owner-scoped list on
-- its own. These two are the endpoint lookups the 1-hop query makes in both
-- directions, and they are what the ON DELETE CASCADE from knowledge_nodes
-- needs -- a foreign key does not index itself.
CREATE INDEX knowledge_edges_from_node_id_idx ON knowledge_edges (from_node_id);
CREATE INDEX knowledge_edges_to_node_id_idx ON knowledge_edges (to_node_id);
-- Same story for the FK to conversations and its ON DELETE SET NULL.
CREATE INDEX knowledge_edges_source_conversation_id_idx ON knowledge_edges (source_conversation_id);

-- Removing a node when the row it mirrors goes away.
--
-- The brief called for this to happen "automatically via FK", and it cannot:
-- ref_id names four different tables, so there is no foreign key to hang a
-- cascade on. A trigger is the next thing that is still automatic. It is not
-- an application call, and that distinction is the whole point -- a task can
-- be deleted by a path the task service never sees (a parent task cascading to
-- its subtasks, a user being deleted, a psql session, whatever a later phase
-- adds), and a rule that only fires where somebody remembered to call it is a
-- rule that leaves orphans. Triggers fire on cascaded deletes too.
--
-- The edges go with the node through knowledge_edges' real foreign keys.
CREATE OR REPLACE FUNCTION drop_knowledge_node() RETURNS trigger AS $$
BEGIN
    DELETE FROM knowledge_nodes
    WHERE user_id = OLD.user_id AND ref_table = TG_ARGV[0] AND ref_id = OLD.id;
    RETURN OLD;
END;
$$ LANGUAGE plpgsql;

-- The delete is a lookup on knowledge_nodes_ref_key, so it costs one index
-- probe per deleted row.
CREATE TRIGGER tasks_drop_knowledge_node
    AFTER DELETE ON tasks
    FOR EACH ROW EXECUTE FUNCTION drop_knowledge_node('tasks');

CREATE TRIGGER goals_drop_knowledge_node
    AFTER DELETE ON goals
    FOR EACH ROW EXECUTE FUNCTION drop_knowledge_node('goals');

CREATE TRIGGER notes_drop_knowledge_node
    AFTER DELETE ON notes
    FOR EACH ROW EXECUTE FUNCTION drop_knowledge_node('notes');

CREATE TRIGGER documents_drop_knowledge_node
    AFTER DELETE ON documents
    FOR EACH ROW EXECUTE FUNCTION drop_knowledge_node('documents');
