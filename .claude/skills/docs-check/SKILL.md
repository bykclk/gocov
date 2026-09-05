---
name: docs-check
description: Build the docs site exactly as CI does (zensical build --strict) and fix any broken link before it reaches main.
allowed-tools: Bash, Read, Grep, Glob, Edit
---

# Check the docs site

`docs/` is also the site at docs.gocov.dev; CI builds it with `zensical build --strict`
and a link to a page that no longer exists fails the build. Run the same build here
after any change under `docs/`, `overrides/`, or `zensical.toml`, before pushing.

Zensical is not on PATH. Use the repo-local virtualenv (`.venv/` and `site/` are both
gitignored); create it on first use:

```sh
[ -x .venv/bin/zensical ] || { python3 -m venv .venv && .venv/bin/pip install -q -r requirements-docs.txt; }
.venv/bin/zensical build --strict
```

A clean run prints `No issues found`. On failure, each warning names the source page
and the target it could not resolve. For each one:

- A page was renamed or removed: point the link at the page that replaced it. Never
  add a stub page just to satisfy the build.
- A new page is missing from the nav: add it in `zensical.toml`.
- Keep `docs/` plain Markdown: no frontmatter, no site-only files, because the same
  files are read on GitHub. `docs/sign-in.md` and `docs/configuration.md` keep their
  filenames; the app and a test link to them.
- `overrides/assets/gocov*.svg` must stay in step with `internal/server/static/favicon.svg`.

Re-run until clean, then report the pages you touched. If nothing was broken, say so in
one line.
