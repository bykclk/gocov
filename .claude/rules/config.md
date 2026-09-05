---
paths:
  - "internal/config/**"
  - "cmd/**"
  - "docs/configuration.md"
---

# Configuration: every environment variable is a tagged struct field

Every environment variable each binary reads is declared as a tagged struct field in `internal/config` (`Server`, `CLI`, `Preview`) — that package is the authoritative list, and `TestConfigurationDocIsInSync` fails the build if `docs/configuration.md` drifts from it (added, removed or re-defaulted variables). Add new variables there, never as a fresh `os.Getenv` at the point of use; `main` parses and validates once at boot (`config.LoadServer`) and then only touches the struct.

Tags cover reading, defaults, types and presence (`DATABASE_URL` is `required,notEmpty` — `required` alone would admit an empty string). What the tag vocabulary cannot say lives beside them: `validate` for fatal rules (secret-key shape, mode/provider interlock) and `Warnings` for survivable ones (half a credential pair). `LoadServerFrom` takes an explicit environment map, so the whole contract is unit-testable.

The `Hosted` config flag switches the instance to self-service mode (any forge account may sign in and register workspaces); the default private mode restricts sign-in to members of tracked workspaces.

When a variable is added, removed or re-defaulted, update `docs/configuration.md` in the same change — the self-hosting section is the only place environment variables are documented for users.
