---
paths:
  - "docs/**"
  - "zensical.toml"
  - "overrides/**"
  - "wrangler.jsonc"
  - "requirements-docs.txt"
---

# docs/ — user docs and the docs.gocov.dev site

User-facing docs are in `docs/`, organized user-first: getting-started, per-CI upload recipes (github-actions, gitlab-ci, bitbucket-pipelines, ci-other) plus languages, feature pages (pull-requests, connecting, coverage-gate, parts, ignoring-files, coverage-changed), reference (cli, api), and a self-hosting section (self-hosting, sign-in, forge-connections for the operator side of connections, configuration, development). User pages never mention environment variables or deployment; that material stays in the self-hosting section. Update the relevant page when changing user-visible behavior.

Fixed filenames: `docs/sign-in.md` and `docs/configuration.md` keep their names (linked from the app's layout template and `TestConfigurationDocIsInSync` respectively). The pinned-version snippets live in gitlab-ci.md, ci-other.md and self-hosting.md between release-please markers; `TestPinnedCLIVersionIsInSync` names the files and release-please bumps them. Change a version only inside the markers, never the markers themselves. `/check-pins` verifies the copies agree.

The same files are the docs site at docs.gocov.dev: `zensical.toml` at the root points at `docs/`, `overrides/` carries the one theme tweak plus a copy of the app's mark (`assets/gocov*.svg` — keep it in step with `internal/server/static/favicon.svg`) and the `_headers` file Cloudflare parses. CI builds with `zensical build --strict`, so a link to a page that no longer exists fails the build; run `/docs-check` before pushing a docs change. Cloudflare runs the same build on every push to `main` and uploads `site/` as static assets per `wrangler.jsonc` — no Worker script, so the file is deploy config only. `docs/` stays plain Markdown — no frontmatter, no site-only files — because it is read on GitHub too, and `docs/README.md` is the site's home page.
