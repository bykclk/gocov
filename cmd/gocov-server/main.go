// gocov-server runs the gocov API + web UI, and provides repo administration:
//
//	gocov-server serve                # default when no subcommand given
//	gocov-server repo add -slug workspace/repo [flags]
//	gocov-server repo list
//	gocov-server repo rotate-token -slug workspace/repo
//	gocov-server repo update -slug workspace/repo [flags]
//	gocov-server repo remove -slug workspace/repo -force
//	gocov-server workspace add -prefix workspace [flags]
//	gocov-server workspace list|rotate-token|update|remove
//	gocov-server user list
//	gocov-server user remove -email someone@example.com
//
// Configuration via environment: DATABASE_URL (required), GOCOV_ADDR
// (default :8080), GOCOV_BASE_URL (default http://localhost:8080), and
// optionally GOCOV_BITBUCKET_USERNAME / GOCOV_BITBUCKET_APP_PASSWORD
// and/or GOCOV_GITHUB_TOKEN for global bot credentials used by repos
// without their own.
// Setting GOCOV_OAUTH_BITBUCKET_KEY / GOCOV_OAUTH_BITBUCKET_SECRET (an
// OAuth consumer) enables — and from then on requires — Bitbucket sign-in
// for the web UI; GOCOV_ALLOWED_WORKSPACES (comma-separated) optionally
// overrides which workspace members may sign in.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bykclk/gocov/internal/auth"
	authbb "github.com/bykclk/gocov/internal/auth/bitbucket"
	blobpg "github.com/bykclk/gocov/internal/blobstore/postgres"
	"github.com/bykclk/gocov/internal/forge"
	"github.com/bykclk/gocov/internal/forge/bitbucket"
	"github.com/bykclk/gocov/internal/forge/github"
	"github.com/bykclk/gocov/internal/profile"
	"github.com/bykclk/gocov/internal/server"
	storepg "github.com/bykclk/gocov/internal/store/postgres"
)

// version is stamped by the release build via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		if !errors.Is(err, errPrinted) {
			fmt.Fprintln(os.Stderr, "gocov-server:", err)
		}
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return serve()
	}
	switch args[0] {
	case "version":
		fmt.Println("gocov-server", version)
		return nil
	case "serve":
		return serve()
	case "repo":
		ctx := context.Background()
		st, err := connect(ctx)
		if err != nil {
			return err
		}
		defer st.Pool().Close()
		return repoCmd(ctx, st, blobpg.New(st.Pool()), args[1:], os.Stdout)
	case "workspace":
		ctx := context.Background()
		st, err := connect(ctx)
		if err != nil {
			return err
		}
		defer st.Pool().Close()
		return workspaceCmd(ctx, st, args[1:], os.Stdout)
	case "user":
		ctx := context.Background()
		st, err := connect(ctx)
		if err != nil {
			return err
		}
		defer st.Pool().Close()
		return userCmd(ctx, st, args[1:], os.Stdout)
	default:
		return fmt.Errorf("unknown command %q (want serve|repo|workspace|user)", args[0])
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
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	st, err := connect(ctx)
	if err != nil {
		return err
	}
	defer st.Pool().Close()

	addr := envOr("GOCOV_ADDR", ":8080")
	baseURL := envOr("GOCOV_BASE_URL", "http://localhost:8080")

	defaultCreds := map[string]map[string]string{}
	bbUser, bbPassword := os.Getenv("GOCOV_BITBUCKET_USERNAME"), os.Getenv("GOCOV_BITBUCKET_APP_PASSWORD")
	switch {
	case bbUser != "" && bbPassword != "":
		defaultCreds["bitbucket"] = map[string]string{"username": bbUser, "app_password": bbPassword}
		log.Info("global bitbucket credentials configured", "username", bbUser)
	case bbUser != "" || bbPassword != "":
		log.Warn("GOCOV_BITBUCKET_USERNAME and GOCOV_BITBUCKET_APP_PASSWORD must both be set; ignoring")
	}
	if ghToken := os.Getenv("GOCOV_GITHUB_TOKEN"); ghToken != "" {
		defaultCreds["github"] = map[string]string{"token": ghToken}
		log.Info("global github credentials configured")
	}

	// Configuring an OAuth consumer is the switch that turns sign-in on;
	// without one the UI stays open and shows a banner saying so.
	var authProvider auth.Provider
	oauthKey, oauthSecret := os.Getenv("GOCOV_OAUTH_BITBUCKET_KEY"), os.Getenv("GOCOV_OAUTH_BITBUCKET_SECRET")
	switch {
	case oauthKey != "" && oauthSecret != "":
		authProvider = authbb.New(oauthKey, oauthSecret)
		log.Info("bitbucket sign-in enabled", "callback", strings.TrimSuffix(baseURL, "/")+"/oauth/bitbucket/callback")
	case oauthKey != "" || oauthSecret != "":
		log.Warn("GOCOV_OAUTH_BITBUCKET_KEY and GOCOV_OAUTH_BITBUCKET_SECRET must both be set; web UI stays open")
	}
	var allowedWorkspaces []string
	for _, ws := range strings.Split(os.Getenv("GOCOV_ALLOWED_WORKSPACES"), ",") {
		if ws = strings.TrimSpace(ws); ws != "" {
			allowedWorkspaces = append(allowedWorkspaces, ws)
		}
	}

	srv := server.New(server.Config{
		Store: st,
		Blobs: blobpg.New(st.Pool()),
		Parsers: map[string]profile.Parser{
			"go":        profile.GoParser{},
			"lcov":      profile.LCOVParser{},
			"jacoco":    profile.JaCoCoParser{},
			"cobertura": profile.CoberturaParser{},
		},
		Forges: map[string]forge.Factory{
			"bitbucket": bitbucket.Factory,
			"github":    github.Factory,
		},
		BaseURL: baseURL,
		Logger:  log,
		Health:  st.Pool().Ping,

		DefaultForgeCredentials: defaultCreds,
		Auth:                    authProvider,
		AllowedWorkspaces:       allowedWorkspaces,
	})

	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           srv,
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() { errCh <- httpSrv.ListenAndServe() }()
	log.Info("listening", "addr", addr, "base_url", baseURL)

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		// SIGINT/SIGTERM: finish in-flight requests, then exit cleanly.
		// Releasing the signal handler first lets a second signal kill the
		// process the default way if shutdown hangs.
		stop()
		log.Info("shutting down")
		shutCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := httpSrv.Shutdown(shutCtx); err != nil {
			return fmt.Errorf("shutdown: %w", err)
		}
		return nil
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
