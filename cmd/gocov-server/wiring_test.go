package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"io"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	blobmem "github.com/gocov/gocov/internal/blobstore/memory"
	"github.com/gocov/gocov/internal/config"
	"github.com/gocov/gocov/internal/server"
	storemem "github.com/gocov/gocov/internal/store/memory"
)

// testKey is a valid GOCOV_SECRET_KEY: 64 hex characters.
const testKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// build loads the configuration from environ exactly as boot does, then
// wires it. Going through config.LoadServerFrom rather than filling a
// config.Server literal keeps these tests honest about the variable
// names and defaults operators actually type.
func build(t *testing.T, environ map[string]string) server.Config {
	t.Helper()
	srvCfg, err := buildOrErr(t, environ)
	if err != nil {
		t.Fatalf("buildServerConfig: %v", err)
	}
	return srvCfg
}

func buildOrErr(t *testing.T, environ map[string]string) (server.Config, error) {
	t.Helper()
	if _, ok := environ["DATABASE_URL"]; !ok {
		environ["DATABASE_URL"] = "postgres://localhost/gocov"
	}
	cfg, err := config.LoadServerFrom(environ)
	if err != nil {
		t.Fatalf("LoadServerFrom(%v): %v", environ, err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return buildServerConfig(cfg, deps{Store: storemem.New(), Blobs: blobmem.New()}, log)
}

func TestAuthProvidersFollowConfiguredPairs(t *testing.T) {
	for _, tc := range []struct {
		name    string
		environ map[string]string
		want    []string
	}{
		{
			name:    "none configured",
			environ: map[string]string{},
			want:    nil,
		},
		{
			name: "one forge",
			environ: map[string]string{
				"GOCOV_OAUTH_GITHUB_KEY":    "id",
				"GOCOV_OAUTH_GITHUB_SECRET": "shh",
			},
			want: []string{"github"},
		},
		{
			// Half a pair is a warning at load time, not a provider.
			name: "half a pair stays off",
			environ: map[string]string{
				"GOCOV_OAUTH_GITLAB_KEY": "id",
			},
			want: nil,
		},
		{
			name: "every forge",
			environ: map[string]string{
				"GOCOV_OAUTH_BITBUCKET_KEY":    "id",
				"GOCOV_OAUTH_BITBUCKET_SECRET": "shh",
				"GOCOV_OAUTH_GITHUB_KEY":       "id",
				"GOCOV_OAUTH_GITHUB_SECRET":    "shh",
				"GOCOV_OAUTH_GITLAB_KEY":       "id",
				"GOCOV_OAUTH_GITLAB_SECRET":    "shh",
			},
			want: []string{"bitbucket", "github", "gitlab"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srvCfg := build(t, tc.environ)
			var got []string
			for _, provider := range srvCfg.Auths {
				got = append(got, provider.Name())
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("providers = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestWorkspaceConnectNeedsSecretKey(t *testing.T) {
	pairs := map[string]string{
		"GOCOV_OAUTH_BITBUCKET_KEY":    "id",
		"GOCOV_OAUTH_BITBUCKET_SECRET": "shh",
		"GOCOV_OAUTH_GITLAB_KEY":       "id",
		"GOCOV_OAUTH_GITLAB_SECRET":    "shh",
	}
	environ := map[string]string{}
	maps.Copy(environ, pairs)

	// Sign-in on, but nothing to encrypt the stored refresh tokens with:
	// the grants must stay off rather than persist them in the clear.
	srvCfg := build(t, environ)
	if srvCfg.BitbucketConnect != nil {
		t.Error("BitbucketConnect enabled without GOCOV_SECRET_KEY")
	}
	if srvCfg.GitLabConnect != nil {
		t.Error("GitLabConnect enabled without GOCOV_SECRET_KEY")
	}

	environ["GOCOV_SECRET_KEY"] = testKey
	srvCfg = build(t, environ)
	if srvCfg.BitbucketConnect == nil {
		t.Error("BitbucketConnect disabled with a full consumer and secret key")
	}
	if srvCfg.GitLabConnect == nil {
		t.Error("GitLabConnect disabled with a full application and secret key")
	}
}

func TestGitHubAppUnconfiguredLeavesNilInterface(t *testing.T) {
	// Regression guard for the typed-nil trap: assigning a nil
	// *github.App into the interface field would make every
	// "app == nil" check in the server read as "configured".
	srvCfg := build(t, map[string]string{})
	if srvCfg.GitHubApp != nil {
		t.Errorf("GitHubApp = %#v, want an untyped nil interface", srvCfg.GitHubApp)
	}
}

func TestGitHubAppFromPEMAndFromFile(t *testing.T) {
	pemKey := testPEM(t)
	path := filepath.Join(t.TempDir(), "app.pem")
	if err := os.WriteFile(path, pemKey, 0o600); err != nil {
		t.Fatal(err)
	}

	// The same key, handed over the two ways GitHub App deployments do
	// it: the PEM inline in the environment, or a path to the file
	// GitHub downloaded.
	for _, tc := range []struct {
		name  string
		value string
	}{
		{"inline pem", string(pemKey)},
		{"path to pem file", path},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srvCfg := build(t, map[string]string{
				"GOCOV_GITHUB_APP_ID":          "1234",
				"GOCOV_GITHUB_APP_PRIVATE_KEY": tc.value,
			})
			if srvCfg.GitHubApp == nil {
				t.Fatal("GitHubApp = nil, want the configured app")
			}
		})
	}
}

func TestGitHubAppBadKeyIsFatal(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  string
		want string
	}{
		{"missing file", filepath.Join(t.TempDir(), "absent.pem"), "reading GOCOV_GITHUB_APP_PRIVATE_KEY file"},
		{"not a pem", "-----BEGIN NONSENSE-----", "GOCOV_GITHUB_APP_PRIVATE_KEY"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := buildOrErr(t, map[string]string{
				"GOCOV_GITHUB_APP_ID":          "1234",
				"GOCOV_GITHUB_APP_PRIVATE_KEY": tc.key,
			})
			if err == nil {
				t.Fatal("buildServerConfig succeeded, want an error naming the variable")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestWebhookSecretAndHostedModeReachTheServer(t *testing.T) {
	srvCfg := build(t, map[string]string{
		"GOCOV_MODE":                  "hosted",
		"GOCOV_OAUTH_GITHUB_KEY":      "id",
		"GOCOV_OAUTH_GITHUB_SECRET":   "shh",
		"GOCOV_GITHUB_WEBHOOK_SECRET": "hook",
		"GOCOV_ALLOWED_WORKSPACES":    "acme, widgets ",
		"GOCOV_BASE_URL":              "https://cov.example.com",
	})
	if !srvCfg.Hosted {
		t.Error("Hosted = false for GOCOV_MODE=hosted")
	}
	if srvCfg.GitHubWebhookSecret != "hook" {
		t.Errorf("GitHubWebhookSecret = %q, want %q", srvCfg.GitHubWebhookSecret, "hook")
	}
	if got := strings.Join(srvCfg.AllowedWorkspaces, ","); got != "acme,widgets" {
		t.Errorf("AllowedWorkspaces = %q, want %q", got, "acme,widgets")
	}
	if srvCfg.BaseURL != "https://cov.example.com" {
		t.Errorf("BaseURL = %q", srvCfg.BaseURL)
	}
}

func TestParsersCoverEveryUploadFormat(t *testing.T) {
	// The upload API dispatches on these names; dropping one silently
	// turns every upload of that format into a 400.
	for _, format := range []string{"go", "lcov", "jacoco", "cobertura", "clover", "simplecov"} {
		if _, ok := parsers()[format]; !ok {
			t.Errorf("no parser registered for %q", format)
		}
	}
}

// testPEM returns a freshly generated PKCS#1 RSA private key in PEM
// form — the shape GitHub hands out for a GitHub App.
func testPEM(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
}

func TestPostHogWiring(t *testing.T) {
	if got := build(t, map[string]string{}).PostHog; got.Configured() {
		t.Errorf("PostHog = %+v with nothing set, want off", got)
	}
	got := build(t, map[string]string{"GOCOV_POSTHOG_KEY": "phc_abc"}).PostHog
	if !got.Configured() || got.Key != "phc_abc" || got.Host != "https://eu.i.posthog.com" {
		t.Errorf("PostHog = %+v, want the key with the EU default host", got)
	}
}
