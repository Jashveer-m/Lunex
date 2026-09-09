-- Phase 2: tasks, goals, notes.
-- set_updated_at() is defined in 000001 and reused here, not redefined.

CREATE TABLE tasks (
    id                       uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id                  uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    title                    text        NOT NULL,
    description              text,
    priority                 text        NOT NULL DEFAULT 'medium',  -- low/medium/high
    status                   text        NOT NULL DEFAULT 'pending', -- pending/in_progress/completed
    category                 text,
    tags                     text[]      NOT NULL DEFAULT '{}',
    deadline                 timestamptz,
    parent_task_id           uuid REFERENCES tasks (id) ON DELETE CASCADE,
    estimated_effort_minutes int,
    actual_effort_minutes    int,
    created_at               timestamptz NOT NULL DEFAULT now(),
    updated_at               timestamptz NOT NULL DEFAULT now()
);

-- Every read is scoped to the owner, so user_id leads each index; a composite
-- on (user_id, ...) also serves a plain user_id lookup, which is why there is
-- no standalone user_id index here.
CREATE INDEX tasks_user_id_created_at_idx ON tasks (user_id, created_at DESC);
CREATE INDEX tasks_user_id_status_idx ON tasks (user_id, status);
-- Partial: most tasks carry no deadline, and "upcoming" never wants those.
CREATE INDEX tasks_user_id_deadline_idx ON tasks (user_id, deadline) WHERE deadline IS NOT NULL;
CREATE INDEX tasks_parent_task_id_idx ON tasks (parent_task_id) WHERE parent_task_id IS NOT NULL;
-- Tag filtering is `tags @> ARRAY[$1]`, which needs GIN; `= ANY(tags)` cannot
-- use an index at all, so the query is written as containment.
CREATE INDEX tasks_tags_idx ON tasks USING gin (tags);

CREATE TRIGGER tasks_set_updated_at
    BEFORE UPDATE ON tasks
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE task_dependencies (
    task_id            uuid NOT NULL REFERENCES tasks (id) ON DELETE CASCADE,
    depends_on_task_id uuid NOT NULL REFERENCES tasks (id) ON DELETE CASCADE,
    PRIMARY KEY (task_id, depends_on_task_id),
    CHECK (task_id != depends_on_task_id)
);

-- The primary key indexes task_id; the reverse direction needs its own index
-- for "what does this task block" and for the ON DELETE CASCADE lookup.
CREATE INDEX task_dependencies_depends_on_task_id_idx ON task_dependencies (depends_on_task_id);

CREATE TABLE goals (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    title       text        NOT NULL,
    description text,
    -- short_term/long_term/career/education/financial/personal/project
    type        text        NOT NULL,
    status      text        NOT NULL DEFAULT 'active', -- active/completed/abandoned
    deadline    timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX goals_user_id_created_at_idx ON goals (user_id, created_at DESC);
CREATE INDEX goals_user_id_status_idx ON goals (user_id, status);
CREATE INDEX goals_user_id_deadline_idx ON goals (user_id, deadline) WHERE deadline IS NOT NULL;

CREATE TRIGGER goals_set_updated_at
    BEFORE UPDATE ON goals
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE goal_milestones (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    goal_id     uuid        NOT NULL REFERENCES goals (id) ON DELETE CASCADE,
    title       text        NOT NULL,
    target_date timestamptz,
    completed   boolean     NOT NULL DEFAULT false,
    created_at  timestamptz NOT NULL DEFAULT now()
);

-- Milestones are always loaded for one goal, or batch-loaded for a page of
-- goals; both are `goal_id = ANY(...)` ordered by target_date.
CREATE INDEX goal_milestones_goal_id_idx ON goal_milestones (goal_id);

CREATE TABLE notes (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    title      text        NOT NULL,
    content    text        NOT NULL DEFAULT '',
    tags       text[]      NOT NULL DEFAULT '{}',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX notes_user_id_created_at_idx ON notes (user_id, created_at DESC);
CREATE INDEX notes_tags_idx ON notes USING gin (tags);

CREATE TRIGGER notes_set_updated_at
    BEFORE UPDATE ON notes
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
