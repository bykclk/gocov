#!/bin/bash
# SessionStart: export GOCOV_TEST_DATABASE_URL for the session only when the
# local test Postgres (see CLAUDE.md) is actually listening, so the store
# integration tests run when they can and keep skipping when they cannot.
url="postgres://gocov:gocov@localhost:5433/gocov"
if [ -n "$CLAUDE_ENV_FILE" ] && nc -z -w 1 localhost 5433 2>/dev/null; then
  echo "export GOCOV_TEST_DATABASE_URL=$url" >> "$CLAUDE_ENV_FILE"
  echo "Test Postgres on :5433 is up; GOCOV_TEST_DATABASE_URL set, store integration tests will run."
fi
exit 0
