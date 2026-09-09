-- Phase 1: users, sessions, user_profiles.
-- pgcrypto gives us gen_random_uuid() on PG < 13; it is a no-op on newer servers.
CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- Keeps updated_at honest even for writes that bypass the application layer.
CREATE OR REPLACE FUNCTION set_updated_at() RETURNS trigger AS $$
BEGIN
    NEW.updated_at = now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TABLE users (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    email         text        NOT NULL,
    password_hash text        NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);

-- Emails are compared case-insensitively; the app also lowercases on write.
CREATE UNIQUE INDEX users_email_key ON users (lower(email));

CREATE TRIGGER users_set_updated_at
    BEFORE UPDATE ON users
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE user_profiles (
    user_id    uuid PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    name       text        NOT NULL DEFAULT '',
    timezone   text        NOT NULL DEFAULT 'UTC',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TRIGGER user_profiles_set_updated_at
    BEFORE UPDATE ON user_profiles
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE sessions (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id            uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    refresh_token_hash text        NOT NULL,
    expires_at         timestamptz NOT NULL,
    device_info        text,
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now()
);

-- Lookup on refresh is by hash, so it must be unique and indexed.
CREATE UNIQUE INDEX sessions_refresh_token_hash_key ON sessions (refresh_token_hash);
CREATE INDEX sessions_user_id_idx ON sessions (user_id);
-- Supports the expired-session sweeper.
CREATE INDEX sessions_expires_at_idx ON sessions (expires_at);

CREATE TRIGGER sessions_set_updated_at
    BEFORE UPDATE ON sessions
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
