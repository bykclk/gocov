-- Web UI authentication (M1): forge-identified users, JIT-provisioned on
-- first authorized login, with server-side sessions. No passwords stored;
-- sessions keep only a hash of the cookie token.

CREATE TABLE users (
    id            BIGSERIAL   PRIMARY KEY,
    forge         TEXT        NOT NULL,
    -- The forge's stable account identifier (survives renames).
    forge_uuid    TEXT        NOT NULL,
    email         TEXT        NOT NULL DEFAULT '',
    display_name  TEXT        NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_login_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Identity is per forge+UUID, so a second forge reusing the same UUID
    -- would not collide.
    UNIQUE (forge, forge_uuid)
);

CREATE TABLE sessions (
    token_hash TEXT        PRIMARY KEY,
    user_id    BIGINT      NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ NOT NULL
);
