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
	"github.com/gocov/gocov/internal/forge/bitbucket"
	forgefake "github.com/gocov/gocov/internal/forge/fake"
	"github.com/gocov/gocov/internal/profile"
	"github.com/gocov/gocov/internal/store"
	storemem "github.com/gocov/gocov/internal/store/memory"
)

// fakeBBConnect is a canned server.BitbucketConnect: refreshes hand out
// sequentially numbered rotating grants (or refreshErr), and ForgeClient
// returns grantForge regardless of the token.
type fakeBBConnect struct {
	grantForge   forge.Forge
	refreshErr   error
	refreshCalls []string // refresh tokens received
	refreshSeq   int
	exchanged    []string // codes received
}

func (f *fakeBBConnect) AuthorizeURL(state, redirectURI string) string {
	return "https://bitbucket.example/authorize?state=" + url.QueryEscape(state) +
		"&redirect_uri=" + url.QueryEscape(redirectURI)
}

func (f *fakeBBConnect) Exchange(_ context.Context, code, _ string) (*bitbucket.Grant, error) {
	f.exchanged = append(f.exchanged, code)
	return &bitbucket.Grant{Account: "covbot", AccessToken: "at-0", RefreshToken: "rt-0", TTL: 2 * time.Hour}, nil
}

func (f *fakeBBConnect) Refresh(_ context.Context, refreshToken string) (*bitbucket.Grant, error) {
	f.refreshCalls = append(f.refreshCalls, refreshToken)
	if f.refreshErr != nil {
		return nil, f.refreshErr
	}
	f.refreshSeq++
	return &bitbucket.Grant{
		AccessToken:  fmt.Sprintf("at-%d", f.refreshSeq),
		RefreshToken: fmt.Sprintf("rt-%d", f.refreshSeq),
		TTL:          2 * time.Hour,
	}, nil
}

func (f *fakeBBConnect) ForgeClient(string) forge.Forge { return f.grantForge }

type bbConnectFixture struct {
	*fixture
	bb         *fakeBBConnect
	grantForge *forgefake.Forge
}

// newBBConnectFixture builds a bitbucket-forge server with connect
// enabled, workspace acme and a signed-in member.
func newBBConnectFixture(t *testing.T) (*bbConnectFixture, *http.Cookie) {
	t.Helper()
	st := storemem.New()
	ws := &store.Workspace{Forge: "bitbucket", Prefix: "acme", Token: "ws-secret", DefaultBranch: "main"}
	if err := st.CreateWorkspace(t.Context(), ws); err != nil {
		t.Fatal(err)
	}
	grantForge := forgefake.New()
	bb := &fakeBBConnect{grantForge: grantForge}
	f := &bbConnectFixture{
		fixture: &fixture{
			srv: New(Config{
				Store:            st,
				Blobs:            blobmem.New(),
				Parsers:          map[string]profile.Parser{"go": profile.GoParser{}},
				BaseURL:          "https://gocov.example",
				Hosted:           true,
				Auths:            []auth.Provider{&fakeProvider{identity: memberIdentity()}},
				BitbucketConnect: bb,
			}),
			store: st,
			forge: grantForge,
		},
		bb:         bb,
		grantForge: grantForge,
	}
	return f, signIn(t, f.fixture, "/")
}

func (f *bbConnectFixture) workspace(t *testing.T) *store.Workspace {
	t.Helper()
	ws, err := f.store.WorkspaceByPrefix(t.Context(), "bitbucket", "acme")
	if err != nil {
		t.Fatal(err)
	}
	return ws
}

func (f *bbConnectFixture) grant(t *testing.T, account, refresh string, broken bool) {
	t.Helper()
	ws := f.workspace(t)
	if err := f.store.SetWorkspaceBitbucketGrant(t.Context(), ws.ID, account, refresh, broken); err != nil {
		t.Fatal(err)
	}
}

func (f *bbConnectFixture) addRepo(t *testing.T) {
	t.Helper()
	repo := &store.Repo{Forge: "bitbucket", Slug: "acme/widgets", Token: "repo-token",
		DefaultBranch: "main"}
	if err := f.store.CreateRepo(t.Context(), repo); err != nil {
		t.Fatal(err)
	}
	f.repo = repo
}

func (f *bbConnectFixture) upload(t *testing.T) uploadResponse {
	t.Helper()
	rec := doUpload(t, f.fixture, "repo-token", map[string]string{
		"repo": "acme/widgets", "commit": "abc123",
	}, testProfile)
	if rec.Code != http.StatusCreated {
		t.Fatalf("upload status = %d, body = %s", rec.Code, rec.Body)
	}
	return uploadResp(t, rec)
}

func TestBitbucketConnectFlow(t *testing.T) {
	f, sess := newBBConnectFixture(t)

	start := get(f.fixture, "/workspaces/bitbucket/acme/connect", sess)
	if start.Code != http.StatusFound {
		t.Fatalf("connect start: status = %d", start.Code)
	}
	loc, err := url.Parse(start.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	// Live Bitbucket enforces an exact match on the consumer's configured
	// callback, so the connect consent must reuse the sign-in callback.
	if got := loc.Query().Get("redirect_uri"); got != "https://gocov.example/oauth/bitbucket/callback" {
		t.Errorf("redirect_uri = %q, must equal the sign-in callback exactly", got)
	}
	stateCk := cookieNamed(t, start, connectStateCookie)
	state, prefix, _ := splitConnectState(stateCk.Value)
	if prefix != "acme" {
		t.Errorf("cookie prefix = %q", prefix)
	}

	cb := get(f.fixture, "/oauth/bitbucket/callback?code=thecode&state="+url.QueryEscape(state), stateCk, sess)
	if cb.Code != http.StatusSeeOther {
		t.Fatalf("callback: status = %d, body = %s", cb.Code, cb.Body)
	}
	if loc := cb.Header().Get("Location"); loc != "/workspaces/bitbucket/acme?connected=1" {
		t.Errorf("callback redirect = %q", loc)
	}
	ws := f.workspace(t)
	if ws.BitbucketGrantAccount != "covbot" || ws.BitbucketRefreshToken != "rt-0" || ws.BitbucketGrantBroken {
		t.Errorf("stored grant = %q/%q/broken=%v", ws.BitbucketGrantAccount, ws.BitbucketRefreshToken, ws.BitbucketGrantBroken)
	}

	// The settings page renders the connected identity (D8) and the notice.
	body := get(f.fixture, "/workspaces/bitbucket/acme?connected=1", sess).Body.String()
	if !strings.Contains(body, "@covbot") || !strings.Contains(body, "Disconnect") {
		t.Error("settings page must show the connected account and disconnect")
	}
	if !strings.Contains(body, "under the connecting account") {
		t.Error("settings page must state the comment identity caveat (D8)")
	}
}

// TestBitbucketConnectFromOnboarding: a connect started from the onboarding
// Workspace-ready card returns there (not the settings page), so "Continue
// to CI" is right where the user left off.
func TestBitbucketConnectFromOnboarding(t *testing.T) {
	f, sess := newBBConnectFixture(t)

	start := get(f.fixture, "/workspaces/bitbucket/acme/connect?from=onboarding", sess)
	stateCk := cookieNamed(t, start, connectStateCookie)
	state, _, from := splitConnectState(stateCk.Value)
	if from != "onboarding" {
		t.Fatalf("cookie from = %q, want onboarding", from)
	}
	cb := get(f.fixture, "/oauth/bitbucket/callback?code=thecode&state="+url.QueryEscape(state), stateCk, sess)
	if loc := cb.Header().Get("Location"); loc != "/onboarding?ws=acme" {
		t.Errorf("callback redirect = %q, want the onboarding ready state", loc)
	}
}

func TestBitbucketConnectCallbackRejects(t *testing.T) {
	f, sess := newBBConnectFixture(t)
	mk := func(value string) *http.Cookie {
		return &http.Cookie{Name: connectStateCookie, Value: value}
	}

	// State mismatch: not recognizably a connect return — falls through
	// to the sign-in callback flow, which rejects it its own way. The
	// connect must not run.
	if rec := get(f.fixture, "/oauth/bitbucket/callback?code=x&state=other", mk("state|acme"), sess); rec.Code != http.StatusFound ||
		rec.Header().Get("Location") != "/login?error=1" {
		t.Errorf("state mismatch: %d -> %q, want the sign-in flow's failure redirect", rec.Code, rec.Header().Get("Location"))
	}
	// No session.
	if rec := get(f.fixture, "/oauth/bitbucket/callback?code=x&state=s", mk("s|acme")); rec.Code != http.StatusForbidden {
		t.Errorf("no session: status = %d, want 403", rec.Code)
	}
	// A workspace the user is no member of.
	if err := f.store.CreateWorkspace(t.Context(),
		&store.Workspace{Forge: "bitbucket", Prefix: "beta", Token: "beta-tok", DefaultBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	if rec := get(f.fixture, "/oauth/bitbucket/callback?code=x&state=s", mk("s|beta"), sess); rec.Code != http.StatusNotFound {
		t.Errorf("non-member workspace: status = %d, want 404", rec.Code)
	}
	if len(f.bb.exchanged) != 0 {
		t.Errorf("rejected callbacks must not exchange codes; exchanged %v", f.bb.exchanged)
	}
}

func TestBitbucketConnectRequiresFeature(t *testing.T) {
	// No BitbucketConnect configured: the connect start does not exist,
	// and a stray connect cookie on the sign-in callback changes nothing.
	f, sess := newWorkspaceFixture(t, false)
	if rec := get(f, "/workspaces/bitbucket/acme/connect", sess); rec.Code != http.StatusNotFound {
		t.Errorf("connect without feature: status = %d, want 404", rec.Code)
	}
	stray := &http.Cookie{Name: connectStateCookie, Value: "s|acme"}
	if rec := get(f, "/oauth/bitbucket/callback?code=x&state=s", stray); rec.Code != http.StatusFound ||
		rec.Header().Get("Location") != "/login?error=1" {
		t.Errorf("callback without feature: %d -> %q, want the sign-in flow's failure redirect",
			rec.Code, rec.Header().Get("Location"))
	}
}

func TestUploadUsesGrantAndPersistsRotation(t *testing.T) {
	// D7: the grant serves the upload, and the rotated refresh token
	// replaces the stored one on the first refresh.
	f, _ := newBBConnectFixture(t)
	f.grant(t, "covbot", "rt-0", false)
	f.addRepo(t)

	resp := f.upload(t)
	if resp.BuildStatus != "posted" || resp.CodeInsights != "posted" {
		t.Errorf("status/insights = %q/%q, want posted/posted", resp.BuildStatus, resp.CodeInsights)
	}
	if len(f.grantForge.StatusCalls) != 1 {
		t.Errorf("grant forge got %d status calls, want 1", len(f.grantForge.StatusCalls))
	}
	if got := f.bb.refreshCalls; len(got) != 1 || got[0] != "rt-0" {
		t.Errorf("refresh calls = %v, want exactly the stored token", got)
	}
	if ws := f.workspace(t); ws.BitbucketRefreshToken != "rt-1" {
		t.Errorf("stored refresh = %q, want the rotated rt-1", ws.BitbucketRefreshToken)
	}
}

func TestUploadGrantAccessTokenCached(t *testing.T) {
	f, _ := newBBConnectFixture(t)
	f.grant(t, "covbot", "rt-0", false)
	f.addRepo(t)

	f.upload(t)
	f.upload(t)
	if got := len(f.bb.refreshCalls); got != 1 {
		t.Errorf("refresh ran %d times for 2 uploads, want 1 (2h token cached)", got)
	}
}

func TestUploadGrantRevokedDegrades(t *testing.T) {
	// The connecting member left, Bitbucket revoked the grant (D7):
	// detected lazily, flagged, upload degrades like missing credentials.
	f, _ := newBBConnectFixture(t)
	f.grant(t, "covbot", "rt-0", false)
	f.addRepo(t)
	f.bb.refreshErr = fmt.Errorf("%w: invalid_grant", forge.ErrCredentialsRevoked)

	resp := f.upload(t)
	if resp.BuildStatus != "skipped" || resp.CodeInsights != "skipped" {
		t.Errorf("status/insights = %q/%q, want skipped/skipped", resp.BuildStatus, resp.CodeInsights)
	}
	ws := f.workspace(t)
	if !ws.BitbucketGrantBroken {
		t.Error("revoked refresh must flag the grant broken")
	}
	if ws.BitbucketGrantAccount != "covbot" {
		t.Error("the account name is kept — it says who to replace")
	}
}

func TestUploadGrantHealsBrokenFlag(t *testing.T) {
	f, _ := newBBConnectFixture(t)
	f.grant(t, "covbot", "rt-0", true)
	f.addRepo(t)

	if resp := f.upload(t); resp.BuildStatus != "posted" {
		t.Fatalf("build status = %q", resp.BuildStatus)
	}
	if ws := f.workspace(t); ws.BitbucketGrantBroken {
		t.Error("a working refresh must clear the broken flag")
	}
}

func TestBitbucketConnectIsOwnersOnly(t *testing.T) {
	// The grant acts for the whole workspace, so starting or dropping it
	// is an owner's move; a member gets a 403 on both routes.
	f, sess := newBBConnectFixture(t)
	demote(t, f.fixture, "acme")
	if rec := get(f.fixture, "/workspaces/bitbucket/acme/connect", sess); rec.Code != http.StatusForbidden {
		t.Errorf("member connect: status = %d, want 403", rec.Code)
	}
	f.grant(t, "gocov-bot", "rt", false)
	if rec := postForm(f.fixture, "/workspaces/bitbucket/acme/disconnect", url.Values{}, sess); rec.Code != http.StatusForbidden {
		t.Errorf("member disconnect: status = %d, want 403", rec.Code)
	}
	if ws := f.workspace(t); ws.BitbucketGrantAccount != "gocov-bot" {
		t.Errorf("member disconnect dropped the grant: %+v", ws)
	}
}

func TestBitbucketDisconnect(t *testing.T) {
	f, sess := newBBConnectFixture(t)
	f.grant(t, "covbot", "rt-0", false)

	rec := postForm(f.fixture, "/workspaces/bitbucket/acme/disconnect", url.Values{}, sess)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d", rec.Code)
	}
	ws := f.workspace(t)
	if ws.BitbucketGrantAccount != "" || ws.BitbucketRefreshToken != "" || ws.BitbucketGrantBroken {
		t.Errorf("after disconnect: %q/%q/%v", ws.BitbucketGrantAccount, ws.BitbucketRefreshToken, ws.BitbucketGrantBroken)
	}
}

func TestSettingsPageBitbucketStates(t *testing.T) {
	f, sess := newBBConnectFixture(t)

	body := get(f.fixture, "/workspaces/bitbucket/acme", sess).Body.String()
	if !strings.Contains(body, "Grant write access") || !strings.Contains(body, "under your own account") {
		t.Error("unconnected settings must offer to grant write access and state the identity caveat (D8)")
	}

	f.grant(t, "covbot", "rt-0", true)
	body = get(f.fixture, "/workspaces/bitbucket/acme", sess).Body.String()
	if !strings.Contains(body, "Reconnect needed") || !strings.Contains(body, "Grant write access again") {
		t.Error("broken grant must surface the reconnect state")
	}
}

func TestSetupPageRecommendsConnect(t *testing.T) {
	f, sess := newBBConnectFixture(t)

	body := get(f.fixture, "/onboarding?ws=acme", sess).Body.String()
	if !strings.Contains(body, "/workspaces/bitbucket/acme/connect") {
		t.Error("ready state must offer the connect grant while not connected")
	}

	f.grant(t, "covbot", "rt-0", false)
	body = get(f.fixture, "/onboarding?ws=acme", sess).Body.String()
	if strings.Contains(body, "/workspaces/bitbucket/acme/connect") {
		t.Error("ready state must drop the connect grant once connected")
	}
	if !strings.Contains(body, "@covbot") {
		t.Error("connected workspace must show the posting account")
	}
}
