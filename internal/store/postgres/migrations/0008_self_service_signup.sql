-- Self-service signup (M3): the registration page renders from the forge
-- workspace list captured at login (M1 discards OAuth tokens, so it cannot
-- be re-fetched later), and hosted workspaces carry their own bot
-- credential because the operator's global bot cannot reach customers'
-- private repos.

-- The workspace slugs the forge reported at the user's last sign-in,
-- refreshed on every login. NULL until the first post-M3 login.
ALTER TABLE users ADD COLUMN forge_workspaces JSONB;

-- Workspace-level forge credentials (same shape as repos.forge_credentials),
-- the middle link of the repo > workspace > global precedence chain.
ALTER TABLE workspaces ADD COLUMN forge_credentials JSONB;
