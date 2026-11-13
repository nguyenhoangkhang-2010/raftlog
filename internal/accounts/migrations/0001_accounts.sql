-- Accounts of the kitchen system: who can sign in, with which roles, and what happened to them.
--
-- A user's id is their login name and also the name that orders and the audit trail carry as
-- "who", so it never changes: an account is disabled, not renamed or deleted.

CREATE TABLE users (
    id                   text        PRIMARY KEY CHECK (id ~ '^[a-z0-9][a-z0-9._-]{2,31}$'),
    display_name         text        NOT NULL CHECK (char_length(display_name) BETWEEN 1 AND 60),
    password_hash        text        NOT NULL,
    roles                text[]      NOT NULL CHECK (
                             cardinality(roles) > 0
                             AND roles <@ ARRAY['customer','cashier','kitchen','runner','manager','storekeeper','admin']::text[]),
    -- the kitchen station of a kitchen worker, and only of a kitchen worker
    station              text        NOT NULL DEFAULT '' CHECK ((station <> '') = ('kitchen' = ANY (roles))),
    disabled             boolean     NOT NULL DEFAULT false,
    must_change_password boolean     NOT NULL DEFAULT false,
    failed_attempts      integer     NOT NULL DEFAULT 0 CHECK (failed_attempts >= 0),
    locked_until         timestamptz,
    created_at           timestamptz NOT NULL,
    created_by           text        NOT NULL,
    updated_at           timestamptz NOT NULL,
    last_login_at        timestamptz,
    password_changed_at  timestamptz NOT NULL
);

-- A session is one signed-in browser. Only a hash of its token is kept, so a copy of this table
-- cannot be used to sign in.
CREATE TABLE sessions (
    id                  bigserial   PRIMARY KEY,
    user_id             text        NOT NULL REFERENCES users (id),
    token_hash          bytea       NOT NULL UNIQUE,
    created_at          timestamptz NOT NULL,
    last_seen_at        timestamptz NOT NULL,
    idle_expires_at     timestamptz NOT NULL,
    absolute_expires_at timestamptz NOT NULL,
    revoked_at          timestamptz,
    ip                  text        NOT NULL DEFAULT '',
    user_agent          text        NOT NULL DEFAULT ''
);
CREATE INDEX sessions_live ON sessions (user_id) WHERE revoked_at IS NULL;

-- The trail of what happened to accounts: sign-ins that worked and did not, locks, password
-- changes, who made whom what. It only grows.
CREATE TABLE account_events (
    id      bigserial   PRIMARY KEY,
    at      timestamptz NOT NULL,
    actor   text        NOT NULL,              -- who did it; empty when nobody was signed in
    action  text        NOT NULL,
    subject text        NOT NULL DEFAULT '',   -- the account it was done to
    detail  jsonb       NOT NULL DEFAULT '{}',
    ip      text        NOT NULL DEFAULT ''
);
CREATE INDEX account_events_subject ON account_events (subject, id DESC);

CREATE FUNCTION account_events_are_kept() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'account_events is append-only';
END
$$;

CREATE TRIGGER account_events_no_change BEFORE UPDATE OR DELETE ON account_events
    FOR EACH ROW EXECUTE FUNCTION account_events_are_kept();
CREATE TRIGGER account_events_no_truncate BEFORE TRUNCATE ON account_events
    FOR EACH STATEMENT EXECUTE FUNCTION account_events_are_kept();
