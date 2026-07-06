// gocov-server runs the gocov API + web UI, and provides repo administration:
//
//	gocov-server serve                # default when no subcommand given
//	gocov-server repo add -slug workspace/repo [flags]
//
// Configuration via environment: DATABASE_URL (required),
// GOCOV_ADDR (default :8080), GOCOV_BASE_URL (default http://localhost:8080).
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	blobpg "github.com/bykclk/gocov/internal/blobstore/postgres"
	"github.com/bykclk/gocov/internal/forge"
	"github.com/bykclk/gocov/internal/forge/bitbucket"
	"github.com/bykclk/gocov/internal/profile"
	"github.com/bykclk/gocov/internal/server"
	"github.com/bykclk/gocov/internal/store"
	storepg "github.com/bykclk/gocov/internal/store/postgres"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "gocov-server:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return serve()
	}
	switch args[0] {
	case "serve":
		return serve()
	case "repo":
		if len(args) >= 2 && args[1] == "add" {
			return repoAdd(args[2:])
		}
		return fmt.Errorf("usage: gocov-server repo add -slug workspace/repo")
	default:
		return fmt.Errorf("unknown command %q (want serve|repo)", args[0])
	}
}

func connect(ctx context.Context) (*storepg.Store, error) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return nil, fmt.Errorf("DATABASE_URL is not set")
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("connecting to postgres: %w", err)
	}
	st := storepg.New(pool)
	if err := st.Migrate(ctx); err != nil {
		return nil, fmt.Errorf("applying migrations: %w", err)
	}
	return st, nil
}

func serve() error {
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	st, err := connect(ctx)
	if err != nil {
		return err
	}
	defer st.Pool().Close()

	addr := envOr("GOCOV_ADDR", ":8080")
	baseURL := envOr("GOCOV_BASE_URL", "http://localhost:8080")

	srv := server.New(server.Config{
		Store:   st,
		Blobs:   blobpg.New(st.Pool()),
		Parsers: map[string]profile.Parser{"go": profile.GoParser{}},
		Forges:  map[string]forge.Factory{"bitbucket": bitbucket.Factory},
		BaseURL: baseURL,
		Logger:  log,
	})

	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           srv,
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Info("listening", "addr", addr, "base_url", baseURL)
	return httpSrv.ListenAndServe()
}

func repoAdd(args []string) error {
	fs := flag.NewFlagSet("repo add", flag.ExitOnError)
	slug := fs.String("slug", "", "repo slug, namespaced: workspace/repo (required)")
	forgeName := fs.String("forge", "bitbucket", "forge hosting the repo")
	defaultBranch := fs.String("default-branch", "main", "default branch")
	bbUser := fs.String("bb-username", "", "Bitbucket username for build status pushes (optional)")
	bbPassword := fs.String("bb-app-password", "", "Bitbucket app password (optional)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *slug == "" {
		return fmt.Errorf("-slug is required")
	}

	ctx := context.Background()
	st, err := connect(ctx)
	if err != nil {
		return err
	}
	defer st.Pool().Close()

	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return err
	}
	var creds map[string]string
	if *bbUser != "" || *bbPassword != "" {
		if *bbUser == "" || *bbPassword == "" {
			return fmt.Errorf("-bb-username and -bb-app-password must be set together")
		}
		creds = map[string]string{"username": *bbUser, "app_password": *bbPassword}
	}

	r := &store.Repo{
		Forge:            *forgeName,
		Slug:             *slug,
		Token:            hex.EncodeToString(buf),
		DefaultBranch:    *defaultBranch,
		ForgeCredentials: creds,
	}
	if err := st.CreateRepo(ctx, r); err != nil {
		return fmt.Errorf("creating repo: %w", err)
	}
	fmt.Printf("repo %s added\nupload token: %s\n", *slug, r.Token)
	return nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
