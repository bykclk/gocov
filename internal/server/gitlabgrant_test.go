package server

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gocov/gocov/internal/auth"
	blobmem "github.com/gocov/gocov/internal/blobstore/memory"
	"github.com/gocov/gocov/internal/forge"
	forgefake "github.com/gocov/gocov/internal/forge/fake"
	"github.com/gocov/gocov/internal/forge/gitlab"
	"github.com/gocov/gocov/internal/profile"
	"github.com/gocov/gocov/internal/store"
	storemem "github.com/gocov/gocov/internal/store/memory"
)

// fakeGLConnect is a canned server.GitLabConnect: refreshes hand out
// sequentially numbered rotating grants (or refreshErr), and ForgeClient
// returns grantForge regardless of the token.
type fakeGLConnect struct {
	grantForge   forge.Forge
	refreshErr   error
	refreshCalls []string // refresh tokens received
	redirectURIs []string // redirect URIs received on refresh
	refreshSeq   int
	exchanged    []string // codes received
}

func (f *fakeGLConnect) AuthorizeURL(state, redirectURI string) string {
	return "https://gitlab.example/authorize?state=" + url.QueryEscape(state) +
		"&redirect_uri=" + url.QueryEscape(redirectURI) + "&scope=api"
}

func (f *fakeGLConnect) Exchange(_ context.Context, code, _ string) (*gitlab.Grant, error) {
	f.exchanged = append(f.exchanged, code)
	return &gitlab.Grant{Account: "covbot", AccessToken: "at-0", RefreshToken: "rt-0", TTL: 2 * time.Hour}, nil
}

func (f *fakeGLConnect) Refresh(_ context.Context, refreshToken, redirectURI string) (*gitlab.Grant, error) {
	f.refreshCalls = append(f.refreshCalls, refreshToken)
	f.redirectURIs = append(f.redirectURIs, redirectURI)
	if f.refreshErr != nil {
		return nil, f.refreshErr
	}
	f.refreshSeq++
	return &gitlab.Grant{
		AccessToken:  fmt.Sprintf("at-%d", f.refreshSeq),
		RefreshToken: fmt.Sprintf("rt-%d", f.refreshSeq),
		TTL:          2 * time.Hour,
	}, nil
}

func (f *fakeGLConnect) ForgeClient(string) forge.Forge { return f.grantForge }

type glConnectFixture struct {
	*fixture
	gl         *fakeGLConnect
	grantForge *forgefake.Forge
}

// newGLConnectFixture builds a gitlab-forge server with connect enabled,
// the subgroup workspace grp/sub (its prefix rides URL-encoded in every
// route) and a signed-in member.
func newGLConnectFixture(t *testing.T) (*glConnectFixture, *http.Cookie) {
	t.Helper()
	st := storemem.New()
	ws := &store.Workspace{Forge: "gitlab", Prefix: "grp/sub", Token: "ws-secret", DefaultBranch: "main"}
	if err := st.CreateWorkspace(t.Context(), ws); err != nil {
		t.Fatal(err)
	}
	grantForge := forgefake.New()
	gl := &fakeGLConnect{grantForge: grantForge}
	f := &glConnectFixture{
		fixture: &fixture{
			srv: New(Config{
				Store:   st,
				Blobs:   blobmem.New(),
				Parsers: map[string]profile.Parser{"go": profile.GoParser{}},
				BaseURL: "https://gocov.example",
				Hosted:  true,
				Auths: []auth.Provider{&fakeProvider{name: "gitlab", identity: &auth.Identity{
					ForgeUUID: "777", DisplayName: "Jane Dev", Email: "jane@example.com",
					Workspaces:      []string{"grp/sub", "janedev"},
					OwnedWorkspaces: []string{"grp/sub", "janedev"},
				}}},
				GitLabConnect: gl,
			}),
			store: st,
			forge: grantForge,
		},
		gl:         gl,
		grantForge: grantForge,
	}
	return f, signInVia(t, f.fixture, "gitlab")
}

func (f *glConnectFixture) workspace(t *testing.T) *store.Workspace {
	t.Helper()
	ws, err := f.store.WorkspaceByPrefix(t.Context(), "gitlab", "grp/sub")
	if err != nil {
		t.Fatal(err)
	}
	return ws
}

func (f *glConnectFixture) grant(t *testing.T, account, refresh string, broken bool) {
	t.Helper()
	ws := f.workspace(t)
	if err := f.store.SetWorkspaceGitLabGrant(t.Context(), ws.ID, account, refresh, broken); err != nil {
		t.Fatal(err)
	}
}

func (f *glConnectFixture) addRepo(t *testing.T) {
	t.Helper()
	repo := &store.Repo{Forge: "gitlab", Slug: "grp/sub/proj", Token: "repo-token",
		DefaultBranch: "main"}
	if err := f.store.CreateRepo(t.Context(), repo); err != nil {
		t.Fatal(err)
	}
	f.repo = repo
}

func (f *glConnectFixture) upload(t *testing.T) uploadResponse {
	t.Helper()
	rec := doUpload(t, f.fixture, "repo-token", map[string]string{
		"repo": "grp/sub/proj", "commit": "abc123",
	}, testProfile)
	if rec.Code != http.StatusCreated {
		t.Fatalf("upload status = %d, body = %s", rec.Code, rec.Body)
	}
	return uploadResp(t, rec)
}

func TestGitLabConnectFlow(t *testing.T) {
	f, sess := newGLConnectFixture(t)

	start := get(f.fixture, "/workspaces/gitlab/grp%2Fsub/connect", sess)
	if start.Code != http.StatusFound {
		t.Fatalf("connect start: status = %d", start.Code)
	}
	loc, err := url.Parse(start.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	// The connect consent reuses the sign-in callback — GitLab enforces
	// an exact match against the application's registered redirect URIs.
	if got := loc.Query().Get("redirect_uri"); got != "https://gocov.example/oauth/gitlab/callback" {
		t.Errorf("redirect_uri = %q, must equal the sign-in callback exactly", got)
	}
	stateCk := cookieNamed(t, start, glConnectStateCookie)
	state, prefix, _ := splitConnectState(stateCk.Value)
	if prefix != "grp/sub" {
		t.Errorf("cookie prefix = %q", prefix)
	}

	cb := get(f.fixture, "/oauth/gitlab/callback?code=thecode&state="+url.QueryEscape(state), stateCk, sess)
	if cb.Code != http.StatusSeeOther {
		t.Fatalf("callback: status = %d, body = %s", cb.Code, cb.Body)
	}
	// The nested prefix must come back %2F-encoded in the redirect.
	if loc := cb.Header().Get("Location"); loc != "/workspaces/gitlab/grp%2Fsub?connected=1" {
		t.Errorf("callback redirect = %q", loc)
	}
	ws := f.workspace(t)
	if ws.GitLabGrantAccount != "covbot" || ws.GitLabRefreshToken != "rt-0" || ws.GitLabGrantBroken {
		t.Errorf("stored grant = %q/%q/broken=%v", ws.GitLabGrantAccount, ws.GitLabRefreshToken, ws.GitLabGrantBroken)
	}

	// The settings page renders the connected identity and the notice.
	body := get(f.fixture, "/workspaces/gitlab/grp%2Fsub?connected=1", sess).Body.String()
	if !strings.Contains(body, "@covbot") || !strings.Contains(body, "Disconnect") {
		t.Error("settings page must show the connected account and disconnect")
	}
	if !strings.Contains(body, "post as @covbot") {
		t.Error("connected notice must state the posting identity")
	}
}

func TestGitLabConnectCallbackRejects(t *testing.T) {
	f, sess := newGLConnectFixture(t)
	mk := func(value string) *http.Cookie {
		return &http.Cookie{Name: glConnectStateCookie, Value: value}
	}

	// State mismatch: not recognizably a connect return — falls through
	// to the sign-in callback flow, which rejects it its own way.
	if rec := get(f.fixture, "/oauth/gitlab/callback?code=x&state=other", mk("state|grp/sub"), sess); rec.Code != http.StatusFound ||
		rec.Header().Get("Location") != "/login?error=1" {
		t.Errorf("state mismatch: %d -> %q, want the sign-in flow's failure redirect", rec.Code, rec.Header().Get("Location"))
	}
	// No session.
	if rec := get(f.fixture, "/oauth/gitlab/callback?code=x&state=s", mk("s|grp/sub")); rec.Code != http.StatusForbidden {
		t.Errorf("no session: status = %d, want 403", rec.Code)
	}
	// A workspace the user is no member of.
	if err := f.store.CreateWorkspace(t.Context(),
		&store.Workspace{Forge: "gitlab", Prefix: "beta", Token: "beta-tok", DefaultBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	if rec := get(f.fixture, "/oauth/gitlab/callback?code=x&state=s", mk("s|beta"), sess); rec.Code != http.StatusNotFound {
		t.Errorf("non-member workspace: status = %d, want 404", rec.Code)
	}
	if len(f.gl.exchanged) != 0 {
		t.Errorf("rejected callbacks must not exchange codes; exchanged %v", f.gl.exchanged)
	}
}

func TestGitLabConnectRequiresFeature(t *testing.T) {
	// No GitLabConnect configured: the connect start does not exist, and
	// a stray connect cookie on the sign-in callback changes nothing.
	f := &fixture{srv: New(Config{
		Store:   storemem.New(),
		Blobs:   blobmem.New(),
		Parsers: map[string]profile.Parser{"go": profile.GoParser{}},
		BaseURL: "https://gocov.example",
		Auths:   []auth.Provider{&fakeProvider{name: "gitlab", identity: &auth.Identity{ForgeUUID: "1", Workspaces: []string{"grp"}}}},
		Hosted:  true,
	})}
	sess := signInVia(t, f, "gitlab")
	if rec := get(f, "/workspaces/gitlab/grp/connect", sess); rec.Code != http.StatusNotFound {
		t.Errorf("connect without feature: status = %d, want 404", rec.Code)
	}
	stray := &http.Cookie{Name: glConnectStateCookie, Value: "s|grp"}
	if rec := get(f, "/oauth/gitlab/callback?code=x&state=s", stray); rec.Code != http.StatusFound ||
		rec.Header().Get("Location") != "/login?error=1" {
		t.Errorf("callback without feature: %d -> %q, want the sign-in flow's failure redirect",
			rec.Code, rec.Header().Get("Location"))
	}
}

func TestGitLabUploadUsesGrantAndPersistsRotation(t *testing.T) {
	// The grant serves the upload, and the rotated refresh token replaces
	// the stored one on the first refresh.
	f, _ := newGLConnectFixture(t)
	f.grant(t, "covbot", "rt-0", false)
	f.addRepo(t)

	resp := f.upload(t)
	if resp.BuildStatus != "posted" {
		t.Errorf("build status = %q, want posted", resp.BuildStatus)
	}
	if len(f.grantForge.StatusCalls) != 1 {
		t.Errorf("grant forge got %d status calls, want 1", len(f.grantForge.StatusCalls))
	}
	if got := f.gl.refreshCalls; len(got) != 1 || got[0] != "rt-0" {
		t.Errorf("refresh calls = %v, want exactly the stored token", got)
	}
	// GitLab wants the redirect URI on the refresh grant too.
	if got := f.gl.redirectURIs; len(got) != 1 || got[0] != "https://gocov.example/oauth/gitlab/callback" {
		t.Errorf("refresh redirect URIs = %v", got)
	}
	if ws := f.workspace(t); ws.GitLabRefreshToken != "rt-1" {
		t.Errorf("stored refresh = %q, want the rotated rt-1", ws.GitLabRefreshToken)
	}
}

func TestGitLabUploadGrantAccessTokenCached(t *testing.T) {
	f, _ := newGLConnectFixture(t)
	f.grant(t, "covbot", "rt-0", false)
	f.addRepo(t)

	f.upload(t)
	f.upload(t)
	if got := len(f.gl.refreshCalls); got != 1 {
		t.Errorf("refresh ran %d times for 2 uploads, want 1 (2h token cached)", got)
	}
}

func TestGitLabUploadGrantRevokedDegrades(t *testing.T) {
	f, _ := newGLConnectFixture(t)
	f.grant(t, "covbot", "rt-0", false)
	f.addRepo(t)
	f.gl.refreshErr = fmt.Errorf("%w: invalid_grant", forge.ErrCredentialsRevoked)

	resp := f.upload(t)
	if resp.BuildStatus != "skipped" {
		t.Errorf("build status = %q, want skipped", resp.BuildStatus)
	}
	ws := f.workspace(t)
	if !ws.GitLabGrantBroken {
		t.Error("revoked refresh must flag the grant broken")
	}
	if ws.GitLabGrantAccount != "covbot" {
		t.Error("the account name is kept — it says who to replace")
	}
}

func TestGitLabUploadGrantHealsBrokenFlag(t *testing.T) {
	f, _ := newGLConnectFixture(t)
	f.grant(t, "covbot", "rt-0", true)
	f.addRepo(t)

	if resp := f.upload(t); resp.BuildStatus != "posted" {
		t.Fatalf("build status = %q", resp.BuildStatus)
	}
	if ws := f.workspace(t); ws.GitLabGrantBroken {
		t.Error("a working refresh must clear the broken flag")
	}
}

func TestGitLabDisconnect(t *testing.T) {
	f, sess := newGLConnectFixture(t)
	f.grant(t, "covbot", "rt-0", false)

	rec := postForm(f.fixture, "/workspaces/gitlab/grp%2Fsub/disconnect", url.Values{}, sess)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d", rec.Code)
	}
	ws := f.workspace(t)
	if ws.GitLabGrantAccount != "" || ws.GitLabRefreshToken != "" || ws.GitLabGrantBroken {
		t.Errorf("after disconnect: %q/%q/%v", ws.GitLabGrantAccount, ws.GitLabRefreshToken, ws.GitLabGrantBroken)
	}
}

func TestSettingsPageGitLabStates(t *testing.T) {
	f, sess := newGLConnectFixture(t)

	body := get(f.fixture, "/workspaces/gitlab/grp%2Fsub", sess).Body.String()
	if !strings.Contains(body, "Grant write access") || !strings.Contains(body, "under your own account") {
		t.Error("unconnected settings must offer to grant write access and state the identity caveat")
	}

	f.grant(t, "covbot", "rt-0", true)
	body = get(f.fixture, "/workspaces/gitlab/grp%2Fsub", sess).Body.String()
	if !strings.Contains(body, "Reconnect needed") || !strings.Contains(body, "Grant write access again") {
		t.Error("broken grant must surface the reconnect state")
	}
}

func TestGitLabSetupPageRecommendsConnect(t *testing.T) {
	f, sess := newGLConnectFixture(t)

	body := get(f.fixture, "/onboarding?ws=grp%2Fsub", sess).Body.String()
	if !strings.Contains(body, "/workspaces/gitlab/grp%2Fsub/connect") {
		t.Error("ready state must offer the connect with the encoded prefix link")
	}

	f.grant(t, "covbot", "rt-0", false)
	body = get(f.fixture, "/onboarding?ws=grp%2Fsub", sess).Body.String()
	if strings.Contains(body, "/workspaces/gitlab/grp%2Fsub/connect") {
		t.Error("connected workspace must not offer the connect link")
	}
	if !strings.Contains(body, "@covbot") {
		t.Error("connected workspace must show the posting identity")
	}
}
