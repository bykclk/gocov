package server

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	blobmem "github.com/gocov/gocov/internal/blobstore/memory"
	"github.com/gocov/gocov/internal/forge"
	forgefake "github.com/gocov/gocov/internal/forge/fake"
	"github.com/gocov/gocov/internal/oidc"
	"github.com/gocov/gocov/internal/profile"
	"github.com/gocov/gocov/internal/store"
	storemem "github.com/gocov/gocov/internal/store/memory"
)

// oidcIssuer is a fake OIDC issuer: its key signs test identity tokens
// and its public half is served as the issuer's JWKS by newIssuer.
type oidcIssuer struct {
	key *rsa.PrivateKey
	kid string
}

// newIssuer serves discovery + JWKS for an OIDC issuer: the well-known
// document at the issuer's own path, the key set at /jwks. The returned
// client rewrites every host to this server, so a token's iss can stay the
// real issuer (which the claim mapping keys off) while the fetch stays
// local. jwksURI is what discovery advertises; the rewrite makes any host
// land here.
func newIssuer(t *testing.T, issuer, jwksURI string) (*oidcIssuer, *http.Client) {
	t.Helper()
	is := &oidcIssuer{key: genTestKey(t), kid: "k1"}
	mux := http.NewServeMux()
	mux.HandleFunc(mustPath(issuer)+"/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"issuer": issuer, "jwks_uri": jwksURI})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(jwksFor(is))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return is, &http.Client{Transport: rewriteHost{target: srv.Listener.Addr().String()}}
}

// genTestKey makes an RSA key for a test issuer.
func genTestKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// jwksFor renders a test issuer's single public key as a JWKS document.
func jwksFor(is *oidcIssuer) map[string]any {
	return map[string]any{"keys": []map[string]string{{
		"kty": "RSA",
		"kid": is.kid,
		"n":   base64.RawURLEncoding.EncodeToString(is.key.N.Bytes()),
		"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(is.key.E)).Bytes()),
	}}}
}

// mustPath returns the path component of a URL, for registering a handler at
// an issuer's discovery path.
func mustPath(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		panic(err)
	}
	return u.Path
}

// rewriteHost sends every request to the test JWKS server, whatever host the
// verifier addressed — so a token's iss can stay the real GitHub issuer
// (which the claim mapping keys off) while the fetch stays local.
type rewriteHost struct{ target string }

func (rt rewriteHost) RoundTrip(req *http.Request) (*http.Response, error) {
	req.URL.Scheme = "http"
	req.URL.Host = rt.target
	return http.DefaultTransport.RoundTrip(req)
}

func (is *oidcIssuer) mint(t *testing.T, claims map[string]any) string {
	t.Helper()
	header := map[string]string{"alg": "RS256", "kid": is.kid, "typ": "JWT"}
	seg := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(b)
	}
	signingInput := seg(header) + "." + seg(claims)
	hashed := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, is.key, crypto.SHA256, hashed[:])
	if err != nil {
		t.Fatal(err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// githubClaims is a valid GitHub Actions identity token for acme/widgets.
func githubClaims(aud string) map[string]any {
	now := time.Now()
	return map[string]any{
		"iss":        gitHubActionsIssuer,
		"sub":        "repo:acme/widgets:ref:refs/heads/main",
		"aud":        aud,
		"exp":        now.Add(5 * time.Minute).Unix(),
		"iat":        now.Unix(),
		"repository": "acme/widgets",
	}
}

// newOIDCFixture builds a github-forge server with repo acme/widgets whose
// workspace is App-connected, and an OIDC verifier bound to the fixture's
// BaseURL as audience and pointed at the local test issuer.
func newOIDCFixture(t *testing.T) (*fixture, *oidcIssuer) {
	t.Helper()
	ctx := t.Context()
	st := storemem.New()
	repo := &store.Repo{Forge: "github", Slug: "acme/widgets", Token: "secret-token", DefaultBranch: "main"}
	if err := st.CreateRepo(ctx, repo); err != nil {
		t.Fatal(err)
	}
	ws := &store.Workspace{Forge: "github", Prefix: "acme", Token: "ws-secret", DefaultBranch: "main", GitHubInstallationID: 77}
	if err := st.CreateWorkspace(ctx, ws); err != nil {
		t.Fatal(err)
	}
	blobs := blobmem.New()
	ff := forgefake.New()
	app := &fakeGitHubApp{appForge: ff, accounts: map[int64]string{77: "acme"}}

	is, client := newIssuer(t, gitHubActionsIssuer, gitHubActionsIssuer+"/jwks")
	verifier := oidc.New(oidc.Config{
		Audience:   "https://gocov.example",
		Issuers:    []string{gitHubActionsIssuer},
		HTTPClient: client,
	})
	srv := New(Config{
		Store:        st,
		Blobs:        blobs,
		Parsers:      map[string]profile.Parser{"go": profile.GoParser{}},
		BaseURL:      "https://gocov.example",
		GitHubApp:    app,
		OIDCVerifier: verifier,
	})
	return &fixture{srv: srv, store: st, blobs: blobs, forge: ff, repo: repo}, is
}

// doOIDCUpload posts an upload authenticated by an OIDC token (no bearer,
// no run claim).
func doOIDCUpload(t *testing.T, f *fixture, token string, extra map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	fields := map[string]string{
		"repo":       "acme/widgets",
		"commit":     "abc123",
		"branch":     "main",
		"oidc_token": token,
	}
	for k, v := range extra {
		if v == "" {
			delete(fields, k)
			continue
		}
		fields[k] = v
	}
	return doUpload(t, f, "", fields, testProfile)
}

func TestOIDCUploadHappyPath(t *testing.T) {
	f, is := newOIDCFixture(t)
	tok := is.mint(t, githubClaims("https://gocov.example"))

	rec := doOIDCUpload(t, f, tok, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}

	// A forge-signed identity is a fully verified upload — not the fork-PR
	// "unverified" mark.
	var resp uploadResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	u, err := f.store.Upload(t.Context(), resp.ID)
	if err != nil {
		t.Fatal(err)
	}
	if u.Meta.Tokenless {
		t.Error("OIDC upload wrongly marked tokenless/unverified")
	}

	// Status posted through the App installation, exactly like a token upload.
	if len(f.forge.StatusCalls) != 1 {
		t.Errorf("got %d status calls, want 1", len(f.forge.StatusCalls))
	}
}

func TestOIDCBadAudience(t *testing.T) {
	f, is := newOIDCFixture(t)
	tok := is.mint(t, githubClaims("https://someone-else.example"))

	rec := doOIDCUpload(t, f, tok, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	assertErrorContains(t, rec, "oidc_bad_audience")
}

func TestOIDCRepoMismatch(t *testing.T) {
	f, is := newOIDCFixture(t)
	tok := is.mint(t, githubClaims("https://gocov.example"))

	// The token is for acme/widgets; the form claims another repo.
	rec := doOIDCUpload(t, f, tok, map[string]string{"repo": "acme/other"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	assertErrorContains(t, rec, "oidc_repo_mismatch")
}

func TestOIDCUntracked(t *testing.T) {
	f, is := newOIDCFixture(t)
	claims := githubClaims("https://gocov.example")
	claims["repository"] = "stranger/repo"
	tok := is.mint(t, claims)

	// The form repo is omitted so the mismatch check does not fire first;
	// the untracked repository is what must be reported. Its workspace is
	// not registered, which is the one thing an OIDC upload cannot fix —
	// the reason names it so the CI log says what to do.
	rec := doOIDCUpload(t, f, tok, map[string]string{"repo": ""})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	assertErrorContains(t, rec, "registered stranger")
	if _, err := f.store.RepoBySlug(t.Context(), "github", "stranger/repo"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("repo registered under no workspace (err = %v)", err)
	}
}

// A repo the forge vouches for is registered on its first OIDC upload,
// exactly as a workspace token's first upload would register it: under
// the tracked workspace, with the default branch asked through the App.
func TestOIDCRegistersRepo(t *testing.T) {
	f, is := newOIDCFixture(t)
	f.forge.DefaultBranch = "trunk"
	claims := githubClaims("https://gocov.example")
	claims["repository"] = "acme/gadgets"
	tok := is.mint(t, claims)

	rec := doOIDCUpload(t, f, tok, map[string]string{"repo": "acme/gadgets"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	repo, err := f.store.RepoBySlug(t.Context(), "github", "acme/gadgets")
	if err != nil {
		t.Fatalf("repo not registered: %v", err)
	}
	if repo.Forge != "github" || repo.DefaultBranch != "trunk" {
		t.Errorf("registered repo = %+v, want github repo on trunk", repo)
	}
	if len(f.forge.StatusCalls) != 1 {
		t.Errorf("got %d status calls, want 1", len(f.forge.StatusCalls))
	}
}

// Names are scoped per forge: a GitHub token for a slug that Bitbucket
// already tracks registers the GitHub repo beside it — two tenants, the
// Bitbucket row untouched — where the global namespace used to refuse it.
func TestOIDCRegistersBesideAnotherForgesNamesake(t *testing.T) {
	f, is := newOIDCFixture(t)
	bb := &store.Repo{Forge: "bitbucket", Slug: "acme/gadgets", Token: "bb-token", DefaultBranch: "main"}
	if err := f.store.CreateRepo(t.Context(), bb); err != nil {
		t.Fatal(err)
	}
	claims := githubClaims("https://gocov.example")
	claims["repository"] = "acme/gadgets"
	tok := is.mint(t, claims)

	rec := doOIDCUpload(t, f, tok, map[string]string{"repo": "acme/gadgets"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	gh, err := f.store.RepoBySlug(t.Context(), "github", "acme/gadgets")
	if err != nil {
		t.Fatalf("github repo not registered beside the bitbucket one: %v", err)
	}
	if gh.ID == bb.ID {
		t.Error("the upload landed on the bitbucket repo")
	}
	if got, _ := f.store.RepoByID(t.Context(), bb.ID); got == nil || got.Token != "bb-token" {
		t.Errorf("bitbucket namesake changed: %+v", got)
	}
}

// The forge existence check a workspace token's registration makes
// applies here too: a repo the App says does not exist is not registered.
func TestOIDCRegisterRefusedWhenForgeHasNoRepo(t *testing.T) {
	f, is := newOIDCFixture(t)
	f.forge.DefaultBranchErr = forge.ErrRepoNotFound
	claims := githubClaims("https://gocov.example")
	claims["repository"] = "acme/ghost"
	tok := is.mint(t, claims)

	rec := doOIDCUpload(t, f, tok, map[string]string{"repo": ""})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if _, err := f.store.RepoBySlug(t.Context(), "github", "acme/ghost"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("repo registered despite the forge refusing it (err = %v)", err)
	}
}

func TestOIDCInvalidToken(t *testing.T) {
	f, _ := newOIDCFixture(t)
	rec := doOIDCUpload(t, f, "not.a.jwt", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	assertErrorContains(t, rec, "oidc_invalid_token")
}

func TestOIDCUnknownIssuer(t *testing.T) {
	f, is := newOIDCFixture(t)
	claims := githubClaims("https://gocov.example")
	claims["iss"] = "https://evil.example"
	tok := is.mint(t, claims)

	rec := doOIDCUpload(t, f, tok, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	assertErrorContains(t, rec, "oidc_unknown_issuer")
}

func assertErrorContains(t *testing.T, rec *httptest.ResponseRecorder, want string) {
	t.Helper()
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding error body: %v", err)
	}
	if !strings.Contains(body.Error, want) {
		t.Errorf("error %q does not contain %q", body.Error, want)
	}
}

// A server built without a BaseURL has no audience to bind, so OIDC uploads
// are unavailable rather than panicking at construction.
func TestOIDCUnavailableWithoutBaseURL(t *testing.T) {
	srv := New(Config{
		Store:   storemem.New(),
		Blobs:   blobmem.New(),
		Parsers: map[string]profile.Parser{"go": profile.GoParser{}},
	})
	if srv.oidc != nil {
		t.Fatal("verifier built without a BaseURL")
	}
}
