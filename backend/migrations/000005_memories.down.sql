-- Reverse of the up migration. set_updated_at() belongs to 000001 and the
-- `vector` extension to 000003; both are deliberately left in place.
DROP TABLE IF EXISTS memories;
