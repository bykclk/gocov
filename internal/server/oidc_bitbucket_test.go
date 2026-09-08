package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	blobmem "github.com/gocov/gocov/internal/blobstore/memory"
	forgefake "github.com/gocov/gocov/internal/forge/fake"
	"github.com/gocov/gocov/internal/oidc"
	"github.com/gocov/gocov/internal/profile"
	"github.com/gocov/gocov/internal/store"
	storemem "github.com/gocov/gocov/internal/store/memory"
)

// bbIssuer is the acme workspace's Bitbucket Pipelines OIDC issuer.
const bbIssuer = "https://api.bitbucket.org/2.0/workspaces/acme/pipelines-config/identity/oidc"

// bbWorkspaceARI is the default audience Bitbucket always includes. A
// pipeline adds gocov's own audience under the step's oidc.audiences, and
// Bitbucket *appends* it to this default, so a real token's aud is an array
// carrying both — which is why the tests mint aud as [ARI, serverURL] and
// the verifier only requires the server URL to be present. See Atlassian's
// "Bitbucket Pipelines OIDC now supports multiple audiences" and the
// resource-server integration docs.
const bbWorkspaceARI = "ari:cloud:bitbucket::workspace/{11111111-1111-1111-1111-111111111111}"

// bbClaims is a valid Bitbucket identity token naming repo acme/widgets by
// the given UUID, with gocov's audience appended to the workspace ARI.
func bbClaims(repoUUID string, auds []any) map[string]any {
	now := time.Now()
	return map[string]any{
		"iss":            bbIssuer,
		"sub":            repoUUID + ":{22222222-2222-2222-2222-222222222222}",
		"aud":            auds,
		"exp":            now.Add(5 * time.Minute).Unix(),
		"iat":            now.Unix(),
		"repositoryUuid": repoUUID,
		"workspaceUuid":  "{11111111-1111-1111-1111-111111111111}",
	}
}

// newBitbucketOIDCFixture builds a bitbucket-forge server with repo
// acme/widgets whose workspace is grant-connected, its fake forge reporting
// forgeUUID as the repo's id, and an OIDC verifier that trusts Bitbucket
// issuers and fetches keys from the local test issuer.
func newBitbucketOIDCFixture(t *testing.T, forgeUUID string) (*fixture, *oidcIssuer) {
	t.Helper()
	ctx := t.Context()
	st := storemem.New()
	repo := &store.Repo{Forge: "bitbucket", Slug: "acme/widgets", Token: "secret-token", DefaultBranch: "main"}
	if err := st.CreateRepo(ctx, repo); err != nil {
		t.Fatal(err)
	}
	ws := &store.Workspace{Forge: "bitbucket", Prefix: "acme", Token: "ws-secret", DefaultBranch: "main"}
	if err := st.CreateWorkspace(ctx, ws); err != nil {
		t.Fatal(err)
	}
	if err := st.SetWorkspaceBitbucketGrant(ctx, ws.ID, "covbot", "rt-0", false); err != nil {
		t.Fatal(err)
	}
	blobs := blobmem.New()
	ff := forgefake.New()
	ff.RepoID = forgeUUID

	is, client := newIssuer(t, bbIssuer, "https://api.bitbucket.org/jwks")
	verifier := oidc.New(oidc.Config{
		Audience:      "https://gocov.example",
		ResolveIssuer: bitbucketIssuerResolver(st),
		HTTPClient:    client,
	})
	srv := New(Config{
		Store:            st,
		Blobs:            blobs,
		Parsers:          map[string]profile.Parser{"go": profile.GoParser{}},
		BaseURL:          "https://gocov.example",
		BitbucketConnect: &fakeBBConnect{grantForge: ff},
		OIDCVerifier:     verifier,
	})
	return &fixture{srv: srv, store: st, blobs: blobs, forge: ff, repo: repo}, is
}

const bbRepoUUID = "{aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee}"

func TestBitbucketOIDCHappyPath(t *testing.T) {
	f, is := newBitbucketOIDCFixture(t, bbRepoUUID)
	// The token spells the UUID in upper case and the forge in lower — the
	// binding is on identity, not spelling.
	tok := is.mint(t, bbClaims(strings.ToUpper(bbRepoUUID), []any{bbWorkspaceARI, "https://gocov.example"}))

	rec := doOIDCUpload(t, f, tok, map[string]string{"repo": "acme/widgets"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}

	// Fully verified, not the fork-PR "unverified" mark.
	var resp uploadResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	u, err := f.store.Upload(t.Context(), resp.ID)
	if err != nil {
		t.Fatal(err)
	}
	if u.Meta.Tokenless {
		t.Error("Bitbucket OIDC upload wrongly marked tokenless")
	}
	// The UUID binding was verified through the forge, and status posted.
	if len(f.forge.RepoIDCalls) != 1 {
		t.Errorf("got %d GetRepoID calls, want 1", len(f.forge.RepoIDCalls))
	}
	if len(f.forge.StatusCalls) != 1 {
		t.Errorf("got %d status calls, want 1", len(f.forge.StatusCalls))
	}
}

// The repo UUID in the token can also ride in sub as "{repo}:{step}" when
// the repositoryUuid claim is absent.
func TestBitbucketOIDCSubFallback(t *testing.T) {
	f, is := newBitbucketOIDCFixture(t, bbRepoUUID)
	claims := bbClaims(bbRepoUUID, []any{bbWorkspaceARI, "https://gocov.example"})
	delete(claims, "repositoryUuid")
	tok := is.mint(t, claims)

	rec := doOIDCUpload(t, f, tok, map[string]string{"repo": "acme/widgets"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
}

// A token whose repository UUID does not match the tracked repo's forge id
// is refused — the slug cannot redirect the upload onto another repo.
// An untracked slug is verified through the workspace it would join —
// the forge's UUID for it must be the signed one — and registered only
// then.
func TestBitbucketOIDCRegistersRepo(t *testing.T) {
	f, is := newBitbucketOIDCFixture(t, bbRepoUUID)
	tok := is.mint(t, bbClaims(bbRepoUUID, []any{bbWorkspaceARI, "https://gocov.example"}))

	rec := doOIDCUpload(t, f, tok, map[string]string{"repo": "acme/gadgets"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	repo, err := f.store.RepoBySlug(t.Context(), "bitbucket", "acme/gadgets")
	if err != nil {
		t.Fatalf("repo not registered: %v", err)
	}
	if repo.Forge != "bitbucket" {
		t.Errorf("registered repo forge = %q, want bitbucket", repo.Forge)
	}
	if got := f.forge.RepoIDCalls; len(got) != 1 || got[0] != "acme/gadgets" {
		t.Errorf("GetRepoID calls = %v, want one for acme/gadgets", got)
	}
}

// A valid token replayed with a victim's untracked slug fails the UUID
// binding before anything is registered: no repo row is left behind.
func TestBitbucketOIDCMismatchRegistersNothing(t *testing.T) {
	f, is := newBitbucketOIDCFixture(t, bbRepoUUID)
	tok := is.mint(t, bbClaims("{99999999-9999-9999-9999-999999999999}", []any{bbWorkspaceARI, "https://gocov.example"}))

	rec := doOIDCUpload(t, f, tok, map[string]string{"repo": "acme/gadgets"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	assertErrorContains(t, rec, "oidc_repo_mismatch")
	if _, err := f.store.RepoBySlug(t.Context(), "bitbucket", "acme/gadgets"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("repo registered despite the UUID mismatch (err = %v)", err)
	}
}

func TestBitbucketOIDCRepoMismatch(t *testing.T) {
	f, is := newBitbucketOIDCFixture(t, bbRepoUUID)
	tok := is.mint(t, bbClaims("{99999999-9999-9999-9999-999999999999}", []any{bbWorkspaceARI, "https://gocov.example"}))

	rec := doOIDCUpload(t, f, tok, map[string]string{"repo": "acme/widgets"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	assertErrorContains(t, rec, "oidc_repo_mismatch")
}

// Without gocov's own audience appended, the default workspace ARI alone is
// not enough — a token minted for Bitbucket's default audience cannot be
// replayed at gocov.
func TestBitbucketOIDCBadAudience(t *testing.T) {
	f, is := newBitbucketOIDCFixture(t, bbRepoUUID)
	tok := is.mint(t, bbClaims(bbRepoUUID, []any{bbWorkspaceARI}))

	rec := doOIDCUpload(t, f, tok, map[string]string{"repo": "acme/widgets"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	assertErrorContains(t, rec, "oidc_bad_audience")
}

// The repo field is required: a Bitbucket token names its repo only by
// UUID, so there is no slug to resolve without it.
func TestBitbucketOIDCRequiresRepoField(t *testing.T) {
	f, is := newBitbucketOIDCFixture(t, bbRepoUUID)
	tok := is.mint(t, bbClaims(bbRepoUUID, []any{bbWorkspaceARI, "https://gocov.example"}))

	rec := doOIDCUpload(t, f, tok, map[string]string{"repo": ""})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
}

// A Bitbucket issuer whose workspace this server does not track is refused
// at the resolver — before any discovery/JWKS fetch — so an unknown
// workspace name can never drive an outbound request to Bitbucket.
func TestBitbucketOIDCUntrackedWorkspaceIssuer(t *testing.T) {
	f, is := newBitbucketOIDCFixture(t, bbRepoUUID)
	claims := bbClaims(bbRepoUUID, []any{bbWorkspaceARI, "https://gocov.example"})
	claims["iss"] = "https://api.bitbucket.org/2.0/workspaces/stranger/pipelines-config/identity/oidc"
	tok := is.mint(t, claims)

	rec := doOIDCUpload(t, f, tok, map[string]string{"repo": "acme/widgets"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	assertErrorContains(t, rec, "oidc_unknown_issuer")
	if len(f.forge.RepoIDCalls) != 0 {
		t.Errorf("untracked-workspace issuer reached the forge")
	}
}

// The Bitbucket path rate-limits its live forge call per repo, so a valid
// token replayed with a victim's slug cannot hammer that workspace's
// Bitbucket connection.
func TestBitbucketOIDCRateLimited(t *testing.T) {
	f, is := newBitbucketOIDCFixture(t, bbRepoUUID)
	// Exhaust the per-repo limiter for the target slug.
	for range maxTokenlessPerRepoHour {
		f.srv.tokenless.allow("acme/widgets", time.Now())
	}
	tok := is.mint(t, bbClaims(bbRepoUUID, []any{bbWorkspaceARI, "https://gocov.example"}))

	rec := doOIDCUpload(t, f, tok, map[string]string{"repo": "acme/widgets"})
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429; body = %s", rec.Code, rec.Body)
	}
	// The forge UUID call was never reached.
	if len(f.forge.RepoIDCalls) != 0 {
		t.Errorf("rate-limited request still called GetRepoID (%d times)", len(f.forge.RepoIDCalls))
	}
}

func TestBitbucketIssuerMatch(t *testing.T) {
	good := []string{
		"https://api.bitbucket.org/2.0/workspaces/acme/pipelines-config/identity/oidc",
		"https://api.bitbucket.org/2.0/workspaces/my-team/pipelines-config/identity/oidc",
	}
	for _, iss := range good {
		if !bitbucketIssuerMatch(iss) {
			t.Errorf("bitbucketIssuerMatch(%q) = false, want true", iss)
		}
	}
	bad := []string{
		"http://api.bitbucket.org/2.0/workspaces/acme/pipelines-config/identity/oidc",        // not https
		"https://evil.example/2.0/workspaces/acme/pipelines-config/identity/oidc",            // wrong host
		"https://api.bitbucket.org/2.0/workspaces/a/b/pipelines-config/identity/oidc",        // two-segment workspace
		"https://api.bitbucket.org/2.0/workspaces/acme/pipelines-config/identity/oidc/extra", // trailing path
		"https://user@api.bitbucket.org/2.0/workspaces/acme/pipelines-config/identity/oidc",  // userinfo
		"https://api.bitbucket.org/2.0/workspaces/acme/pipelines-config/identity/oidc?x=1",   // query
		"https://token.actions.githubusercontent.com",                                        // other forge
	}
	for _, iss := range bad {
		if bitbucketIssuerMatch(iss) {
			t.Errorf("bitbucketIssuerMatch(%q) = true, want false", iss)
		}
	}
}
