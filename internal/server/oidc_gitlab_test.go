package server

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	blobmem "github.com/gocov/gocov/internal/blobstore/memory"
	forgefake "github.com/gocov/gocov/internal/forge/fake"
	"github.com/gocov/gocov/internal/oidc"
	"github.com/gocov/gocov/internal/profile"
	"github.com/gocov/gocov/internal/store"
	storemem "github.com/gocov/gocov/internal/store/memory"
)

func glClaims(issuer, projectPath, aud string) map[string]any {
	now := time.Now()
	return map[string]any{
		"iss":          issuer,
		"sub":          "project_path:" + projectPath + ":ref_type:branch:ref:main",
		"aud":          aud,
		"exp":          now.Add(5 * time.Minute).Unix(),
		"iat":          now.Unix(),
		"project_path": projectPath,
	}
}

// newGitLabOIDCFixture builds a gitlab-forge server with repo acme/widgets
// whose workspace is grant-connected, and an OIDC verifier trusting the
// given issuer. extraIssuers are configured as self-managed GitLab issuers
// (GOCOV_OIDC_ISSUERS) so the token router recognizes them.
func newGitLabOIDCFixture(t *testing.T, issuer string, extraIssuers []string) (*fixture, *oidcIssuer) {
	t.Helper()
	ctx := t.Context()
	st := storemem.New()
	repo := &store.Repo{Forge: "gitlab", Slug: "acme/widgets", Token: "secret-token", DefaultBranch: "main"}
	if err := st.CreateRepo(ctx, repo); err != nil {
		t.Fatal(err)
	}
	ws := &store.Workspace{Forge: "gitlab", Prefix: "acme", Token: "ws-secret", DefaultBranch: "main"}
	if err := st.CreateWorkspace(ctx, ws); err != nil {
		t.Fatal(err)
	}
	if err := st.SetWorkspaceGitLabGrant(ctx, ws.ID, "covbot", "rt-0", false); err != nil {
		t.Fatal(err)
	}
	blobs := blobmem.New()
	ff := forgefake.New()

	is, client := newIssuer(t, issuer, issuer+"/jwks")
	verifier := oidc.New(oidc.Config{
		Audience:   "https://gocov.example",
		Issuers:    []string{issuer},
		HTTPClient: client,
	})
	srv := New(Config{
		Store:         st,
		Blobs:         blobs,
		Parsers:       map[string]profile.Parser{"go": profile.GoParser{}},
		BaseURL:       "https://gocov.example",
		GitLabConnect: &fakeGLConnect{grantForge: ff},
		OIDCVerifier:  verifier,
		OIDCIssuers:   extraIssuers,
	})
	return &fixture{srv: srv, store: st, blobs: blobs, forge: ff, repo: repo}, is
}

func TestGitLabOIDCHappyPath(t *testing.T) {
	f, is := newGitLabOIDCFixture(t, gitLabDotComIssuer, nil)
	tok := is.mint(t, glClaims(gitLabDotComIssuer, "acme/widgets", "https://gocov.example"))

	rec := doOIDCUpload(t, f, tok, map[string]string{"repo": "acme/widgets"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	var resp uploadResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	u, err := f.store.Upload(t.Context(), resp.ID)
	if err != nil {
		t.Fatal(err)
	}
	if u.Meta.Tokenless {
		t.Error("GitLab OIDC upload wrongly marked tokenless")
	}
	if len(f.forge.StatusCalls) != 1 {
		t.Errorf("got %d status calls, want 1", len(f.forge.StatusCalls))
	}
}

// A project below the registered group is registered on its first OIDC
// upload, keeping its subgroup path as the repo name the way a workspace
// token's registration does.
func TestGitLabOIDCRegistersNestedProject(t *testing.T) {
	f, is := newGitLabOIDCFixture(t, gitLabDotComIssuer, nil)
	tok := is.mint(t, glClaims(gitLabDotComIssuer, "acme/tools/gadgets", "https://gocov.example"))

	rec := doOIDCUpload(t, f, tok, map[string]string{"repo": "acme/tools/gadgets"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	repo, err := f.store.RepoBySlug(t.Context(), "gitlab", "acme/tools/gadgets")
	if err != nil {
		t.Fatalf("repo not registered: %v", err)
	}
	if repo.Forge != "gitlab" {
		t.Errorf("registered repo forge = %q, want gitlab", repo.Forge)
	}
	if len(f.forge.StatusCalls) != 1 {
		t.Errorf("got %d status calls, want 1", len(f.forge.StatusCalls))
	}
}

// A token whose project_path is not this repo's slug is a mismatch.
func TestGitLabOIDCRepoMismatch(t *testing.T) {
	f, is := newGitLabOIDCFixture(t, gitLabDotComIssuer, nil)
	tok := is.mint(t, glClaims(gitLabDotComIssuer, "acme/widgets", "https://gocov.example"))

	rec := doOIDCUpload(t, f, tok, map[string]string{"repo": "acme/other"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	assertErrorContains(t, rec, "oidc_repo_mismatch")
}

// A self-managed GitLab issuer added via GOCOV_OIDC_ISSUERS is trusted and
// routed to the gitlab claim mapping.
func TestGitLabOIDCSelfManagedIssuer(t *testing.T) {
	const selfManaged = "https://gitlab.acme.example"
	f, is := newGitLabOIDCFixture(t, selfManaged, []string{selfManaged})
	tok := is.mint(t, glClaims(selfManaged, "acme/widgets", "https://gocov.example"))

	rec := doOIDCUpload(t, f, tok, map[string]string{"repo": "acme/widgets"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
}

// Configuring a self-managed GitLab issuer replaces the gitlab.com default:
// a deployment connects to one GitLab, so a gitlab.com token must not
// authenticate an upload on an instance that trusts a self-managed issuer.
// The real verifier (built by New from the config) rejects it at the issuer
// check, before any key fetch.
func TestGitLabOIDCConfigReplacesGitlabDotCom(t *testing.T) {
	const selfManaged = "https://gitlab.acme.example"
	ctx := t.Context()
	st := storemem.New()
	repo := &store.Repo{Forge: "gitlab", Slug: "acme/widgets", Token: "secret-token", DefaultBranch: "main"}
	if err := st.CreateRepo(ctx, repo); err != nil {
		t.Fatal(err)
	}
	srv := New(Config{
		Store:       st,
		Blobs:       blobmem.New(),
		Parsers:     map[string]profile.Parser{"go": profile.GoParser{}},
		BaseURL:     "https://gocov.example",
		OIDCIssuers: []string{selfManaged},
	})
	f := &fixture{srv: srv, store: st, repo: repo}

	// A well-formed token from gitlab.com — rejected because gitlab.com is no
	// longer trusted once a self-managed issuer is configured. Issuer is
	// checked before signature/fetch, so any key signs it.
	is := &oidcIssuer{key: genTestKey(t), kid: "x"}
	tok := is.mint(t, glClaims(gitLabDotComIssuer, "acme/widgets", "https://gocov.example"))

	rec := doOIDCUpload(t, f, tok, map[string]string{"repo": "acme/widgets"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	assertErrorContains(t, rec, "oidc_unknown_issuer")
}

// A self-managed issuer that the operator did not configure is unknown,
// even though its token is otherwise valid — the router has no mapping and
// the verifier's allowlist does not include it. Here the verifier trusts it
// (to reach the router) but the server was not told it is a GitLab issuer.
func TestGitLabOIDCUnconfiguredIssuerUnsupported(t *testing.T) {
	const selfManaged = "https://gitlab.rogue.example"
	// extraIssuers nil: the server does not know this is a GitLab issuer.
	f, is := newGitLabOIDCFixture(t, selfManaged, nil)
	tok := is.mint(t, glClaims(selfManaged, "acme/widgets", "https://gocov.example"))

	rec := doOIDCUpload(t, f, tok, map[string]string{"repo": "acme/widgets"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	assertErrorContains(t, rec, "oidc_invalid_token")
}
