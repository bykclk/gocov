package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/gocov/gocov/internal/auth"
	authbb "github.com/gocov/gocov/internal/auth/bitbucket"
	authgh "github.com/gocov/gocov/internal/auth/github"
	authgl "github.com/gocov/gocov/internal/auth/gitlab"
	"github.com/gocov/gocov/internal/blobstore"
	"github.com/gocov/gocov/internal/config"
	"github.com/gocov/gocov/internal/forge/bitbucket"
	"github.com/gocov/gocov/internal/forge/github"
	"github.com/gocov/gocov/internal/forge/gitlab"
	"github.com/gocov/gocov/internal/profile"
	"github.com/gocov/gocov/internal/server"
	"github.com/gocov/gocov/internal/store"
)

// deps are the things buildServerConfig is handed rather than building
// for itself: the database-backed dependencies, whose construction is
// the one part of start-up that needs a live Postgres. Taking them as
// interfaces is what lets the wiring below be exercised in a test with
// the in-memory doubles.
type deps struct {
	Store  store.Store
	Blobs  blobstore.Store
	Health func(ctx context.Context) error
}

// buildServerConfig turns the validated environment into the server's
// wiring: which forges can sign users in, whether the GitHub App and the
// workspace-connect grants are configured, and which optional endpoints
// come with them. Each decision is logged as it is made, so a boot log
// reads as the list of features this deployment actually has.
//
// The only fatal case here is a GitHub App private key that is present
// but unusable; everything else that is half-configured has already been
// reported by config.Server.Warnings and simply stays off.
func buildServerConfig(cfg config.Server, d deps, log *slog.Logger) (server.Config, error) {
	app, err := githubApp(cfg, log)
	if err != nil {
		return server.Config{}, err
	}

	srvCfg := server.Config{
		Store:   d.Store,
		Blobs:   d.Blobs,
		Parsers: parsers(),
		BaseURL: cfg.BaseURL,
		Logger:  log,
		Health:  d.Health,

		Auths:             authProviders(cfg, log),
		AllowedWorkspaces: cfg.AllowedWorkspaces,
		Hosted:            cfg.Hosted(),
		PublicReports:     cfg.PublicReportsEnabled(),
		OIDCIssuers:       cfg.OIDCIssuers,
	}
	if cfg.Hosted() {
		log.Info("hosted mode: self-service workspace registration enabled")
	}
	if !cfg.PublicReportsEnabled() {
		log.Info("public report pages disabled (GOCOV_PUBLIC_REPORTS=off)")
	}
	if cfg.PostHog.Configured() {
		srvCfg.PostHog = server.PostHog{Key: cfg.PostHog.Key, Host: cfg.PostHog.Host}
		log.Info("posthog analytics enabled", "host", cfg.PostHog.Host)
	}
	// Assigned conditionally: a typed-nil *github.App in the interface
	// field would read as "configured".
	if app != nil {
		srvCfg.GitHubApp = app
	}
	if cfg.GitHubWebhookSecret != "" {
		srvCfg.GitHubWebhookSecret = cfg.GitHubWebhookSecret
		log.Info("github webhook endpoint enabled")
	}
	switch {
	case cfg.BitbucketConnectEnabled():
		srvCfg.BitbucketConnect = &bitbucket.Consumer{Key: cfg.Bitbucket.Key, Secret: cfg.Bitbucket.Secret}
		log.Info("bitbucket workspace connect enabled")
	case cfg.Bitbucket.Configured():
		log.Info("GOCOV_SECRET_KEY not set; Bitbucket workspace connect stays disabled")
	}
	switch {
	case cfg.GitLabConnectEnabled():
		srvCfg.GitLabConnect = &gitlab.Application{Key: cfg.GitLab.Key, Secret: cfg.GitLab.Secret}
		log.Info("gitlab workspace connect enabled")
	case cfg.GitLab.Configured():
		log.Info("GOCOV_SECRET_KEY not set; GitLab workspace connect stays disabled")
	}
	return srvCfg, nil
}

// parsers is the set of coverage formats this build understands, keyed
// by the format name the upload API and CLI use.
func parsers() map[string]profile.Parser {
	return map[string]profile.Parser{
		"go":        profile.GoParser{},
		"lcov":      profile.LCOVParser{},
		"jacoco":    profile.JaCoCoParser{},
		"cobertura": profile.CoberturaParser{},
		"clover":    profile.CloverParser{},
		"simplecov": profile.SimpleCovParser{},
	}
}

// authProviders builds one sign-in provider per forge with a complete
// OAuth pair, logging the callback URL each one has to be registered
// with on the forge side — the setting operators most often get wrong,
// and the one they cannot discover from the UI.
func authProviders(cfg config.Server, log *slog.Logger) []auth.Provider {
	callback := func(forge string) string {
		return strings.TrimSuffix(cfg.BaseURL, "/") + "/oauth/" + forge + "/callback"
	}
	var providers []auth.Provider
	for _, p := range []struct {
		forge string
		app   config.OAuthApp
		new   func(key, secret string) auth.Provider
	}{
		{"bitbucket", cfg.Bitbucket, func(k, s string) auth.Provider { return authbb.New(k, s) }},
		{"github", cfg.GitHub, func(k, s string) auth.Provider { return authgh.New(k, s) }},
		{"gitlab", cfg.GitLab, func(k, s string) auth.Provider { return authgl.New(k, s) }},
	} {
		if p.app.Configured() {
			providers = append(providers, p.new(p.app.Key, p.app.Secret))
			log.Info(p.forge+" sign-in enabled", "callback", callback(p.forge))
		}
	}
	if len(providers) == 0 {
		log.Info("no sign-in provider configured; web UI stays open")
	}
	return providers
}

// githubApp builds the deployment's GitHub App identity from the
// configured id and private key. The key holds either the PEM itself or a
// path to a PEM file — key files are how GitHub hands the secret out, but
// container setups often prefer the content in the environment. A
// half-configured pair is already reported by config.Server.Warnings, so
// it just leaves the integration off here.
func githubApp(cfg config.Server, log *slog.Logger) (*github.App, error) {
	if !cfg.GitHubAppEnabled() {
		return nil, nil
	}
	pemData := []byte(cfg.GitHubAppPrivateKey)
	if !strings.Contains(cfg.GitHubAppPrivateKey, "-----BEGIN") {
		data, err := os.ReadFile(cfg.GitHubAppPrivateKey)
		if err != nil {
			return nil, fmt.Errorf("reading GOCOV_GITHUB_APP_PRIVATE_KEY file: %w", err)
		}
		pemData = data
	}
	app, err := github.NewApp(cfg.GitHubAppID, pemData)
	if err != nil {
		return nil, fmt.Errorf("GOCOV_GITHUB_APP_PRIVATE_KEY: %w", err)
	}
	log.Info("github app configured", "app_id", cfg.GitHubAppID)
	return app, nil
}
