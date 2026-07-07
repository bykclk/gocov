// Package testpg provides disposable Postgres databases for integration
// tests. Tests are skipped unless GOCOV_TEST_DATABASE_URL points at a
// Postgres server the tests may create databases on, e.g.:
//
//	docker run --rm -d -p 5433:5432 -e POSTGRES_USER=gocov \
//	  -e POSTGRES_PASSWORD=gocov -e POSTGRES_DB=gocov postgres:16-alpine
//	GOCOV_TEST_DATABASE_URL=postgres://gocov:gocov@localhost:5433/gocov go test ./...
package testpg

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// EnvVar names the connection string consulted by Pool.
const EnvVar = "GOCOV_TEST_DATABASE_URL"

// Pool creates a uniquely named database on the configured server and
// returns a pool connected to it. The database is dropped on test cleanup.
// Skips the test when EnvVar is unset.
func Pool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv(EnvVar)
	if dsn == "" {
		t.Skipf("%s not set; skipping Postgres integration test", EnvVar)
	}
	ctx := context.Background()

	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connecting to %s: %v", EnvVar, err)
	}

	buf := make([]byte, 6)
	if _, err := rand.Read(buf); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	name := "gocov_test_" + hex.EncodeToString(buf)
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		admin.Close()
		t.Fatalf("creating test database: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP DATABASE "+name+" WITH (FORCE)")
		admin.Close()
	})

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.Database = name
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connecting to test database: %v", err)
	}
	t.Cleanup(pool.Close) // runs before the DROP DATABASE cleanup
	return pool
}
