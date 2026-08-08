package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/bykclk/gocov/internal/auth"
	blobmem "github.com/bykclk/gocov/internal/blobstore/memory"
	"github.com/bykclk/gocov/internal/forge"
	forgefake "github.com/bykclk/gocov/internal/forge/fake"
	"github.com/bykclk/gocov/internal/profile"
	"github.com/bykclk/gocov/internal/store"
	storemem "github.com/bykclk/gocov/internal/store/memory"
)

// newWorkspaceFixture builds a private-mode server with sign-in enabled,
// the workspace acme (token ws-secret) and optionally the repo
// acme/widgets (token secret-token), then signs the member in.
func newWorkspaceFixture(t *testing.T, globalCreds map[string]string, withRepo bool) (*fixture, *http.Cookie) {
	t.Helper()
	st := storemem.New()
	ws := &store.Workspace{Forge: "bitbucket", Prefix: "acme", Token: "ws-secret", DefaultBranch: "main"}
	if err := st.CreateWorkspace(context.Background(), ws); err != nil {
		t.Fatal(err)
	}
	var repo *store.Repo
	if withRepo {
		repo = &store.Repo{Forge: "bitbucket", Slug: "acme/widgets", Token: "secret-token", DefaultBranch: "main"}
		if err := st.CreateRepo(context.Background(), repo); err != nil {
			t.Fatal(err)
		}
	}
	ff := forgefake.New()
	var defaults map[string]map[string]string
	if globalCreds != nil {
		defaults = map[string]map[string]string{"bitbucket": globalCreds}
	}
	f := &fixture{
		srv: New(Config{
			Store:                   st,
			Blobs:                   blobmem.New(),
			Parsers:                 map[string]profile.Parser{"go": profile.GoParser{}},
			Forges:                  map[string]forge.Factory{"bitbucket": ff.Factory()},
			BaseURL:                 "https://gocov.example",
			Auths:                   []auth.Provider{&fakeProvider{identity: memberIdentity()}},
			DefaultForgeCredentials: defaults,
		}),
		store: st,
		forge: ff,
		repo:  repo,
	}
	return f, signIn(t, f, "/")
}

func postForm(f *fixture, path string, form url.Values, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, req)
	return rec
}

func TestWorkspaceSettingsAccess(t *testing.T) {
	f, sess := newWorkspaceFixture(t, nil, true)

	rec := get(f, "/workspaces/acme", sess)
	if rec.Code != http.StatusOK {
		t.Fatalf("member settings page: status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "not configured") {
		t.Error("page must show the credentials-unset state")
	}
	// R3: the page never renders a stored secret — not even the token.
	if strings.Contains(body, "ws-secret") {
		t.Error("settings page leaks the upload token")
	}
	if !strings.Contains(body, "/workspaces/acme/setup") {
		t.Error("settings page must link to the setup instructions (R4)")
	}

	// A workspace the user is no member of 404s, even though it exists.
	if err := f.store.CreateWorkspace(context.Background(),
		&store.Workspace{Forge: "bitbucket", Prefix: "beta", Token: "beta-tok", DefaultBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	if rec := get(f, "/workspaces/beta", sess); rec.Code != http.StatusNotFound {
		t.Errorf("non-member settings page: status = %d, want 404", rec.Code)
	}
	// Without a session the auth middleware redirects to login.
	if rec := get(f, "/workspaces/acme"); rec.Code != http.StatusFound {
		t.Errorf("anonymous settings page: status = %d, want login redirect", rec.Code)
	}
}

func TestWorkspaceSettingsNeedAuthEnabled(t *testing.T) {
	// Open mode has no notion of members; the pages do not exist (M2/D5:
	// open mode stays byte-identical, so no new surfaces appear).
	f := newFixture(t, nil)
	if err := f.store.CreateWorkspace(context.Background(),
		&store.Workspace{Forge: "bitbucket", Prefix: "acme", Token: "ws-secret", DefaultBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/workspaces/acme", "/workspaces/acme/setup"} {
		if rec := get(f, path); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s in open mode: status = %d, want 404", path, rec.Code)
		}
	}
}

func TestWorkspaceRotateToken(t *testing.T) {
	f, sess := newWorkspaceFixture(t, nil, true)
	ctx := context.Background()

	// The workspace token authorizes uploads before rotation...
	rec := doUpload(t, f, "ws-secret", map[string]string{
		"repo": "acme/widgets", "commit": "c1", "branch": "main"}, testProfile)
	if rec.Code != http.StatusCreated {
		t.Fatalf("upload with workspace token: status = %d, body = %s", rec.Code, rec.Body)
	}

	rot := postForm(f, "/workspaces/acme/rotate-token", url.Values{}, sess)
	if rot.Code != http.StatusOK {
		t.Fatalf("rotate: status = %d", rot.Code)
	}
	ws, err := f.store.WorkspaceByPrefix(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if ws.Token == "ws-secret" {
		t.Fatal("token was not rotated")
	}
	// The new token is rendered once, on the rotation response itself.
	if !strings.Contains(rot.Body.String(), ws.Token) {
		t.Errorf("rotation response does not show the new token:\n%s", rot.Body)
	}

	// ...the old token dies on the next upload, the new one works (R3).
	if rec := doUpload(t, f, "ws-secret", map[string]string{
		"repo": "acme/widgets", "commit": "c2", "branch": "main"}, testProfile); rec.Code != http.StatusUnauthorized {
		t.Errorf("old token after rotation: status = %d, want 401", rec.Code)
	}
	if rec := doUpload(t, f, ws.Token, map[string]string{
		"repo": "acme/widgets", "commit": "c2", "branch": "main"}, testProfile); rec.Code != http.StatusCreated {
		t.Errorf("new token: status = %d, body = %s", rec.Code, rec.Body)
	}

	// A fresh settings page load no longer shows any token.
	if page := get(f, "/workspaces/acme", sess); strings.Contains(page.Body.String(), ws.Token) {
		t.Error("settings page shows the token after rotation (rotate-only means shown once)")
	}
}

func TestWorkspaceSettingsUpdate(t *testing.T) {
	f, sess := newWorkspaceFixture(t, nil, true)
	ctx := context.Background()

	rec := postForm(f, "/workspaces/acme/settings", url.Values{
		"default_branch":    {"develop"},
		"min_coverage":      {"80"},
		"max_coverage_drop": {"0"},
	}, sess)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/workspaces/acme?saved=1" {
		t.Fatalf("settings save: %d -> %q", rec.Code, rec.Header().Get("Location"))
	}
	ws, _ := f.store.WorkspaceByPrefix(ctx, "acme")
	if ws.DefaultBranch != "develop" ||
		ws.Gate.MinCoverage == nil || *ws.Gate.MinCoverage != 80 ||
		ws.Gate.MaxCoverageDrop == nil || *ws.Gate.MaxCoverageDrop != 0 ||
		ws.Gate.MinDiffCoverage != nil {
		t.Errorf("workspace after save: %+v gate %+v", ws, ws.Gate)
	}

	// Empty gate fields clear the rules.
	postForm(f, "/workspaces/acme/settings", url.Values{"default_branch": {"develop"}}, sess)
	if ws, _ := f.store.WorkspaceByPrefix(ctx, "acme"); ws.Gate.Configured() {
		t.Errorf("gate not cleared: %+v", ws.Gate)
	}

	// Validation failures re-render with 400 and change nothing.
	for name, form := range map[string]url.Values{
		"bad number":   {"default_branch": {"develop"}, "min_coverage": {"lots"}},
		"out of range": {"default_branch": {"develop"}, "min_diff_coverage": {"120"}},
		"empty branch": {"default_branch": {" "}},
	} {
		if rec := postForm(f, "/workspaces/acme/settings", form, sess); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", name, rec.Code)
		}
	}
	if ws, _ := f.store.WorkspaceByPrefix(ctx, "acme"); ws.DefaultBranch != "develop" {
		t.Errorf("failed validation changed the workspace: %+v", ws)
	}
}

func TestWorkspaceCredentials(t *testing.T) {
	f, sess := newWorkspaceFixture(t, nil, true)
	ctx := context.Background()

	// Both halves of the Bitbucket credential are required.
	if rec := postForm(f, "/workspaces/acme/credentials", url.Values{
		"action": {"save"}, "username": {"bot"}}, sess); rec.Code != http.StatusBadRequest {
		t.Errorf("half credential: status = %d, want 400", rec.Code)
	}

	rec := postForm(f, "/workspaces/acme/credentials", url.Values{
		"action": {"save"}, "username": {"bot"}, "app_password": {"hunter2"}}, sess)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("save credentials: status = %d", rec.Code)
	}
	ws, _ := f.store.WorkspaceByPrefix(ctx, "acme")
	want := map[string]string{"username": "bot", "app_password": "hunter2"}
	if !reflect.DeepEqual(ws.ForgeCredentials, want) {
		t.Errorf("stored credentials = %v", ws.ForgeCredentials)
	}

	// The page reports the configured state but never echoes the secret (R3).
	page := get(f, "/workspaces/acme", sess)
	if !strings.Contains(page.Body.String(), "configured") {
		t.Error("page must show the configured state")
	}
	if strings.Contains(page.Body.String(), "hunter2") || strings.Contains(page.Body.String(), `value="bot"`) {
		t.Errorf("settings page echoes stored credentials:\n%s", page.Body)
	}

	if rec := postForm(f, "/workspaces/acme/credentials", url.Values{"action": {"clear"}}, sess); rec.Code != http.StatusSeeOther {
		t.Fatalf("clear credentials: status = %d", rec.Code)
	}
	if ws, _ := f.store.WorkspaceByPrefix(ctx, "acme"); ws.ForgeCredentials != nil {
		t.Errorf("credentials not cleared: %v", ws.ForgeCredentials)
	}
}

// TestCredentialPrecedence covers the R3 acceptance chain: repo
// credentials beat workspace credentials beat the global defaults.
func TestCredentialPrecedence(t *testing.T) {
	global := map[string]string{"username": "global-bot", "app_password": "g"}
	f, sess := newWorkspaceFixture(t, global, true)
	ctx := context.Background()

	wsCreds := url.Values{"action": {"save"}, "username": {"ws-bot"}, "app_password": {"w"}}
	if rec := postForm(f, "/workspaces/acme/credentials", wsCreds, sess); rec.Code != http.StatusSeeOther {
		t.Fatalf("save workspace credentials: status = %d", rec.Code)
	}

	// factoryCreds runs one upload and returns the credential sets the
	// forge factory saw for it.
	factoryCreds := func(commit string) []map[string]string {
		t.Helper()
		f.forge.FactoryCreds = nil
		rec := doUpload(t, f, "secret-token", map[string]string{
			"repo": "acme/widgets", "commit": commit, "branch": "main"}, testProfile)
		if rec.Code != http.StatusCreated {
			t.Fatalf("upload: status = %d, body = %s", rec.Code, rec.Body)
		}
		if len(f.forge.FactoryCreds) == 0 {
			t.Fatal("upload built no forge client")
		}
		return f.forge.FactoryCreds
	}
	assertAll := func(got []map[string]string, wantUser string) {
		t.Helper()
		for _, creds := range got {
			if creds["username"] != wantUser {
				t.Errorf("forge client built with %v, want username %q", creds, wantUser)
			}
		}
	}

	// Repo has no credentials of its own -> workspace credentials (D4).
	assertAll(factoryCreds("c1"), "ws-bot")

	// Repo credentials win over both.
	repo, _ := f.store.RepoBySlug(ctx, "acme/widgets")
	repo.ForgeCredentials = map[string]string{"username": "repo-bot", "app_password": "r"}
	if err := f.store.UpdateRepo(ctx, repo); err != nil {
		t.Fatal(err)
	}
	assertAll(factoryCreds("c2"), "repo-bot")

	// Neither repo nor workspace credentials -> the global defaults.
	repo.ForgeCredentials = nil
	if err := f.store.UpdateRepo(ctx, repo); err != nil {
		t.Fatal(err)
	}
	if rec := postForm(f, "/workspaces/acme/credentials", url.Values{"action": {"clear"}}, sess); rec.Code != http.StatusSeeOther {
		t.Fatalf("clear workspace credentials: status = %d", rec.Code)
	}
	assertAll(factoryCreds("c3"), "global-bot")
}

func TestSetupPageWaitsAndFlips(t *testing.T) {
	f, sess := newWorkspaceFixture(t, nil, false) // no repos yet

	rec := get(f, "/workspaces/acme/setup", sess)
	if rec.Code != http.StatusOK {
		t.Fatalf("setup page: status = %d", rec.Code)
	}
	body := rec.Body.String()
	// The snippet carries the real server URL and token (R4/D6)...
	if !strings.Contains(body, "GOCOV_SERVER=https://gocov.example") || !strings.Contains(body, "GOCOV_TOKEN=ws-secret") {
		t.Errorf("setup snippet misses server URL or token:\n%s", body)
	}
	if !strings.Contains(body, "bitbucket-pipelines.yml") {
		t.Errorf("bitbucket workspace must get the Pipelines snippet:\n%s", body)
	}
	// ...and starts in the polling waiting state.
	if !strings.Contains(body, "Waiting for your first upload") ||
		!strings.Contains(body, `hx-get="/workspaces/acme/setup/status"`) {
		t.Errorf("setup page misses the waiting state:\n%s", body)
	}
	// The poll target keeps waiting while there are no repos.
	if st := get(f, "/workspaces/acme/setup/status", sess); !strings.Contains(st.Body.String(), "Waiting") {
		t.Errorf("status endpoint should still wait:\n%s", st.Body)
	}

	// First upload auto-registers the repo; the poll flips to the link.
	up := doUpload(t, f, "ws-secret", map[string]string{
		"repo": "acme/newrepo", "commit": "c1", "branch": "main"}, testProfile)
	if up.Code != http.StatusCreated {
		t.Fatalf("first upload: status = %d, body = %s", up.Code, up.Body)
	}
	st := get(f, "/workspaces/acme/setup/status", sess)
	if !strings.Contains(st.Body.String(), "First upload received") ||
		!strings.Contains(st.Body.String(), `href="/repos/acme/newrepo"`) {
		t.Errorf("status endpoint did not flip:\n%s", st.Body)
	}
	if strings.Contains(st.Body.String(), "hx-get") {
		t.Error("flipped status block must stop polling")
	}
	// A fresh page load shows the done state directly.
	if rec := get(f, "/workspaces/acme/setup", sess); !strings.Contains(rec.Body.String(), "First upload received") {
		t.Errorf("reloaded setup page still waiting:\n%s", rec.Body)
	}
}

func TestSetupPageGitHubSnippet(t *testing.T) {
	st := storemem.New()
	if err := st.CreateWorkspace(context.Background(),
		&store.Workspace{Forge: "github", Prefix: "myorg", Token: "gh-secret", DefaultBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	gh := &fakeProvider{name: "github", identity: &auth.Identity{
		ForgeUUID: "42", DisplayName: "Hub Dev", Workspaces: []string{"myorg"},
	}}
	f := &fixture{
		srv: New(Config{
			Store:   st,
			Blobs:   blobmem.New(),
			Parsers: map[string]profile.Parser{"go": profile.GoParser{}},
			Forges:  map[string]forge.Factory{"github": forgefake.New().Factory()},
			BaseURL: "https://gocov.example",
			Auths:   []auth.Provider{gh},
		}),
		store: st,
	}
	start := get(f, "/oauth/github/start")
	stateCk := cookieNamed(t, start, stateCookie)
	state, _, _ := strings.Cut(stateCk.Value, "|")
	cb := get(f, "/oauth/github/callback?code=x&state="+url.QueryEscape(state), stateCk)
	sess := cookieNamed(t, cb, sessionCookie)

	rec := get(f, "/workspaces/myorg/setup", sess)
	if rec.Code != http.StatusOK {
		t.Fatalf("setup page: status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"GitHub Actions workflow",
		"${{ vars.GOCOV_SERVER }}",
		"${{ secrets.GOCOV_TOKEN }}",
		"GOCOV_TOKEN=gh-secret",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("github setup page misses %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "bitbucket-pipelines.yml") {
		t.Error("github workspace got the Bitbucket snippet")
	}
}
