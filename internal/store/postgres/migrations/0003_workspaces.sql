CREATE TABLE workspaces (
    id             BIGSERIAL PRIMARY KEY,
    forge          TEXT        NOT NULL DEFAULT 'bitbucket',
    prefix         TEXT        NOT NULL UNIQUE,
    token          TEXT        NOT NULL UNIQUE,
    default_branch TEXT        NOT NULL DEFAULT 'main',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
