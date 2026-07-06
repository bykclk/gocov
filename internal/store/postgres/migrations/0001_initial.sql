CREATE TABLE repos (
    id                BIGSERIAL PRIMARY KEY,
    forge             TEXT        NOT NULL DEFAULT 'bitbucket',
    slug              TEXT        NOT NULL UNIQUE,
    token             TEXT        NOT NULL UNIQUE,
    default_branch    TEXT        NOT NULL DEFAULT 'main',
    forge_credentials JSONB,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE uploads (
    id            BIGSERIAL PRIMARY KEY,
    repo_id       BIGINT           NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    commit_sha    TEXT             NOT NULL,
    branch        TEXT             NOT NULL,
    pr_id         TEXT             NOT NULL DEFAULT '',
    format        TEXT             NOT NULL DEFAULT 'go',
    total_pct     DOUBLE PRECISION NOT NULL,
    covered_stmts BIGINT           NOT NULL,
    total_stmts   BIGINT           NOT NULL,
    raw_blob_key  TEXT             NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ      NOT NULL DEFAULT now()
);

CREATE INDEX uploads_repo_branch_idx ON uploads (repo_id, branch, id DESC);

CREATE TABLE upload_files (
    upload_id     BIGINT           NOT NULL REFERENCES uploads(id) ON DELETE CASCADE,
    path          TEXT             NOT NULL,
    pct           DOUBLE PRECISION NOT NULL,
    covered_stmts BIGINT           NOT NULL,
    total_stmts   BIGINT           NOT NULL,
    blocks        JSONB            NOT NULL,
    PRIMARY KEY (upload_id, path)
);

CREATE TABLE blobs (
    key        TEXT        PRIMARY KEY,
    data       BYTEA       NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
