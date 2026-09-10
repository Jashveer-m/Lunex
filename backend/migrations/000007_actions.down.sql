-- Reverse of the up migration. set_updated_at() belongs to 000001 and is
-- deliberately left in place.
DROP TABLE IF EXISTS actions;
