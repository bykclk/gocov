-- Multi-tenancy (M2): a user's access mirrors their forge workspace
-- membership. workspace_members records that mapping, synced on every
-- sign-in. Both sides cascade, so deleting a user or workspace clears its
-- memberships.

CREATE TABLE workspace_members (
    workspace_id BIGINT      NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    user_id      BIGINT      NOT NULL REFERENCES users (id)      ON DELETE CASCADE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, user_id)
);

-- Membership lookups run per-request keyed by user; index that direction.
CREATE INDEX workspace_members_user_idx ON workspace_members (user_id);

-- D6 backfill: every repo must belong to a workspace. Repos added via
-- `repo add` before M2 can lack a workspace row for their slug prefix;
-- create one per orphan prefix, copying the forge and default branch from
-- the repo and generating a random token in the same 24-byte hex shape as
-- the application's token generator (collision-safe by construction).
CREATE EXTENSION IF NOT EXISTS pgcrypto;

INSERT INTO workspaces (forge, prefix, token, default_branch)
SELECT DISTINCT ON (split_part(r.slug, '/', 1))
       r.forge,
       split_part(r.slug, '/', 1),
       encode(gen_random_bytes(24), 'hex'),
       r.default_branch
FROM repos r
WHERE position('/' IN r.slug) > 0
  AND NOT EXISTS (
      SELECT 1 FROM workspaces w
      WHERE w.prefix = split_part(r.slug, '/', 1)
  )
ORDER BY split_part(r.slug, '/', 1), r.id;
