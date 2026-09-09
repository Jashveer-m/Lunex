-- Reverse order of the up migration. set_updated_at() belongs to 000001 and is
-- deliberately left in place: 000001's down migration owns it.
DROP TABLE IF EXISTS notes;
DROP TABLE IF EXISTS goal_milestones;
DROP TABLE IF EXISTS goals;
DROP TABLE IF EXISTS task_dependencies;
DROP TABLE IF EXISTS tasks;
