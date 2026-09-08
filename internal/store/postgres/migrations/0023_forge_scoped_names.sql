-- Repo slugs and workspace prefixes are unique per forge, not globally:
-- the GitHub org "acme" and the GitLab group "acme" are different
-- tenants, and one project mirrored to two forges is two repos. Every
-- lookup by name carries the forge from now on; the URLs do too.

ALTER TABLE repos DROP CONSTRAINT repos_slug_key;
ALTER TABLE repos ADD CONSTRAINT repos_forge_slug_key UNIQUE (forge, slug);

ALTER TABLE workspaces DROP CONSTRAINT workspaces_prefix_key;
ALTER TABLE workspaces ADD CONSTRAINT workspaces_forge_prefix_key UNIQUE (forge, prefix);
