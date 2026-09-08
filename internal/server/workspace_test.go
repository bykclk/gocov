package server

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gocov/gocov/internal/auth"
	blobmem "github.com/gocov/gocov/internal/blobstore/memory"
	forgefake "github.com/gocov/gocov/internal/forge/fake"
	"github.com/gocov/gocov/internal/hosted"
	"github.com/gocov/gocov/internal/profile"
	"github.com/gocov/gocov/internal/store"
	storemem "github.com/gocov/gocov/internal/store/memory"
)

// newWorkspaceFixture builds a private-mode server with sign-in enabled,
// the workspace acme (token ws-secret) and optionally the repo
// acme/widgets (token secret-token), then signs the owner in.
func newWorkspaceFixture(t *testing.T, withRepo bool) (*fixture, *http.Cookie) {
	t.Helper()
	return newWorkspaceFixtureAs(t, withRepo, memberIdentity())
}

// newMemberFixture is newWorkspaceFixture with the account seated as a
// plain member: the forge lists it in acme without the admin role.
func newMemberFixture(t *testing.T, withRepo bool) (*fixture, *http.Cookie) {
	t.Helper()
	return newWorkspaceFixtureAs(t, withRepo, plainMemberIdentity())
}

func newWorkspaceFixtureAs(t *testing.T, withRepo bool, id *auth.Identity) (*fixture, *http.Cookie) {
	t.Helper()
	st := storemem.New()
	ws := &store.Workspace{Forge: "bitbucket", Prefix: "acme", Token: "ws-secret", DefaultBranch: "main"}
	if err := st.CreateWorkspace(t.Context(), ws); err != nil {
		t.Fatal(err)
	}
	var repo *store.Repo
	if withRepo {
		repo = &store.Repo{Forge: "bitbucket", Slug: "acme/widgets", Token: "secret-token", DefaultBranch: "main"}
		if err := st.CreateRepo(t.Context(), repo); err != nil {
			t.Fatal(err)
		}
	}
	ff := forgefake.New()
	f := &fixture{
		srv: New(Config{
			Store:   st,
			Blobs:   blobmem.New(),
			Parsers: map[string]profile.Parser{"go": profile.GoParser{}},
			BaseURL: "https://gocov.example",
			Auths:   []auth.Provider{&fakeProvider{identity: id}},
		}),
		store: st,
		forge: ff,
		repo:  repo,
	}
	return f, signIn(t, f, "/")
}

// demote reseats the fixture's (only) user as a plain member of the
// workspace — what the next login sync would do after the forge dropped
// their admin role.
func demote(t *testing.T, f *fixture, prefix string) {
	t.Helper()
	ctx := t.Context()
	users, err := f.store.ListUsers(ctx)
	if err != nil || len(users) != 1 {
		t.Fatalf("users = %v, %v", users, err)
	}
	ws, err := f.store.WorkspaceByPrefix(ctx, prefix)
	if err != nil {
		t.Fatal(err)
	}
	memberships, err := f.store.ListMembershipsForUser(ctx, users[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	for i := range memberships {
		if memberships[i].WorkspaceID == ws.ID {
			memberships[i].Role = store.RoleMember
		}
	}
	if err := f.store.SetUserMemberships(ctx, users[0].ID, memberships); err != nil {
		t.Fatal(err)
	}
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
	f, sess := newWorkspaceFixture(t, true)

	rec := get(f, "/workspaces/acme", sess)
	if rec.Code != http.StatusOK {
		t.Fatalf("member settings page: status = %d", rec.Code)
	}
	body := rec.Body.String()
	// Uploads card (v2): the token is stored in the clear and shown to
	// members on demand (Reveal), so it rides in the page for that control.
	// The security boundary is membership — non-members 404 below.
	if !strings.Contains(body, "ws-secret") {
		t.Error("settings page should make the upload token available to members (Reveal)")
	}
	if !strings.Contains(body, "/workspaces/acme/setup") {
		t.Error("settings page must link to the setup instructions (R4)")
	}

	// A workspace the user is no member of 404s, even though it exists.
	if err := f.store.CreateWorkspace(t.Context(),
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

func TestMemberSettingsAreReadOnly(t *testing.T) {
	// A member sees the settings page but nothing they cannot do: no
	// token, no forms, a note saying why. The owner-only routes answer a
	// hand-built POST with 403 and change nothing.
	f, sess := newMemberFixture(t, true)
	ctx := t.Context()

	rec := get(f, "/workspaces/acme", sess)
	if rec.Code != http.StatusOK {
		t.Fatalf("member settings page: status = %d", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "ws-secret") {
		t.Error("member page carries the upload token")
	}
	for _, control := range []string{"Rotate token", "Delete this workspace", `action="/workspaces/acme/settings"`, "Grant write access"} {
		if strings.Contains(body, control) {
			t.Errorf("member page renders the owner control %q", control)
		}
	}
	if !strings.Contains(body, "read-only") || !strings.Contains(body, "owner") {
		t.Errorf("member page does not say the settings are read-only for members:\n%s", body)
	}
	// The values are still visible, just not editable.
	if !strings.Contains(body, `value="main" disabled`) {
		t.Errorf("member page does not show the default branch read-only:\n%s", body)
	}

	for _, post := range []struct {
		path string
		form url.Values
	}{
		{"/workspaces/acme/rotate-token", url.Values{}},
		{"/workspaces/acme/settings", url.Values{"default_branch": {"develop"}}},
		{"/workspaces/acme/delete", url.Values{}},
		{"/workspaces/acme/bitbucket/disconnect", url.Values{}},
	} {
		if rec := postForm(f, post.path, post.form, sess); rec.Code != http.StatusForbidden {
			t.Errorf("member POST %s: status = %d, want 403", post.path, rec.Code)
		}
	}
	ws, err := f.store.WorkspaceByPrefix(ctx, "acme")
	if err != nil || ws.Token != "ws-secret" || ws.DefaultBranch != "main" {
		t.Errorf("a member's refused POSTs changed the workspace: %+v, %v", ws, err)
	}

	// The setup page (Wire up CI, so a workspace without repos yet) keeps
	// the CI snippet for a member, minus the token.
	f, sess = newMemberFixture(t, false)
	rec = get(f, "/workspaces/acme/setup", sess)
	if rec.Code != http.StatusOK {
		t.Fatalf("member setup page: status = %d", rec.Code)
	}
	if body := rec.Body.String(); strings.Contains(body, "ws-secret") || !strings.Contains(body, "owners only") {
		t.Errorf("member setup page must withhold the token and say so:\n%s", body)
	}
	if rec := get(f, "/workspaces/acme/setup/status", sess); rec.Code != http.StatusOK {
		t.Errorf("member setup poll: status = %d", rec.Code)
	}
}

func TestOwnerDemotedOnTheForgeLosesTheControls(t *testing.T) {
	// The role is whatever the last sign-in said; once it says member,
	// the owner-only routes close — no session or page state keeps them.
	f, sess := newWorkspaceFixture(t, false)
	if rec := postForm(f, "/workspaces/acme/settings", url.Values{"default_branch": {"develop"}}, sess); rec.Code != http.StatusSeeOther {
		t.Fatalf("owner save: status = %d", rec.Code)
	}
	demote(t, f, "acme")
	if rec := postForm(f, "/workspaces/acme/settings", url.Values{"default_branch": {"main"}}, sess); rec.Code != http.StatusForbidden {
		t.Errorf("demoted save: status = %d, want 403", rec.Code)
	}
	if ws, _ := f.store.WorkspaceByPrefix(t.Context(), "acme"); ws.DefaultBranch != "develop" {
		t.Errorf("demoted save went through: branch = %q", ws.DefaultBranch)
	}
}

func TestWorkspaceSettingsNeedAuthEnabled(t *testing.T) {
	// Open mode has no notion of members; the pages do not exist (M2/D5:
	// open mode stays byte-identical, so no new surfaces appear).
	f := newFixture(t, nil)
	if err := f.store.CreateWorkspace(t.Context(),
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
	f, sess := newWorkspaceFixture(t, true)
	ctx := t.Context()

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

	// A fresh settings page load still carries the current token so a
	// member can reveal it (masked by default); the OLD token is gone.
	page := get(f, "/workspaces/acme", sess).Body.String()
	if !strings.Contains(page, ws.Token) {
		t.Error("settings page should expose the rotated token to members (Reveal)")
	}
	if strings.Contains(page, "ws-secret") {
		t.Error("settings page still shows the pre-rotation token")
	}
}

func TestWorkspaceSettingsUpdate(t *testing.T) {
	f, sess := newWorkspaceFixture(t, true)
	ctx := t.Context()

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

func TestWorkspaceRetention(t *testing.T) {
	f, sess := newWorkspaceFixture(t, true)
	ctx := t.Context()

	// A valid retention window is persisted alongside the branch.
	rec := postForm(f, "/workspaces/acme/settings", url.Values{
		"default_branch":        {"main"},
		"report_retention_days": {"90"},
	}, sess)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("save: status = %d", rec.Code)
	}
	if ws, _ := f.store.WorkspaceByPrefix(ctx, "acme"); ws.ReportRetentionDays != 90 {
		t.Errorf("retention = %d, want 90", ws.ReportRetentionDays)
	}

	// "Forever" is 0; the selector renders it as the chosen option.
	postForm(f, "/workspaces/acme/settings", url.Values{
		"default_branch":        {"main"},
		"report_retention_days": {"0"},
	}, sess)
	if ws, _ := f.store.WorkspaceByPrefix(ctx, "acme"); ws.ReportRetentionDays != 0 {
		t.Errorf("retention = %d, want 0 (forever)", ws.ReportRetentionDays)
	}

	// An unlisted window is rejected and changes nothing.
	if rec := postForm(f, "/workspaces/acme/settings", url.Values{
		"default_branch":        {"main"},
		"report_retention_days": {"7"},
	}, sess); rec.Code != http.StatusBadRequest {
		t.Errorf("bad retention: status = %d, want 400", rec.Code)
	}
}

func TestWorkspaceDelete(t *testing.T) {
	f, sess := newWorkspaceFixture(t, true)
	ctx := t.Context()

	// An upload under the workspace creates a repo (and its data) — all of
	// which the delete must cascade away.
	if rec := doUpload(t, f, "ws-secret", map[string]string{
		"repo": "acme/widgets", "commit": "c1", "branch": "main"}, testProfile); rec.Code != http.StatusCreated {
		t.Fatalf("seed upload: status = %d", rec.Code)
	}
	if _, err := f.store.RepoBySlug(ctx, "acme/widgets"); err != nil {
		t.Fatalf("repo not present before delete: %v", err)
	}

	rec := postForm(f, "/workspaces/acme/delete", url.Values{}, sess)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("delete: %d -> %q", rec.Code, rec.Header().Get("Location"))
	}
	if _, err := f.store.WorkspaceByPrefix(ctx, "acme"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("workspace survived delete: %v", err)
	}
	if _, err := f.store.RepoBySlug(ctx, "acme/widgets"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("repo survived workspace delete (no cascade): %v", err)
	}

	// A non-member cannot delete a workspace they cannot see (404).
	f2, sess2 := newWorkspaceFixture(t, false)
	if err := f2.store.CreateWorkspace(ctx,
		&store.Workspace{Forge: "bitbucket", Prefix: "beta", Token: "beta-tok", DefaultBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	if rec := postForm(f2, "/workspaces/beta/delete", url.Values{}, sess2); rec.Code != http.StatusNotFound {
		t.Errorf("non-member delete: status = %d, want 404", rec.Code)
	}
	if _, err := f2.store.WorkspaceByPrefix(ctx, "beta"); err != nil {
		t.Errorf("non-member delete removed the workspace: %v", err)
	}
}

func TestSetupPageWaitsAndFlips(t *testing.T) {
	f, sess := newWorkspaceFixture(t, false) // no repos yet

	rec := get(f, "/workspaces/acme/setup", sess)
	if rec.Code != http.StatusOK {
		t.Fatalf("setup page: status = %d", rec.Code)
	}
	body := rec.Body.String()
	// The self-hosted base URL is surfaced and the token is available to
	// reveal/copy (R4/D6). The token rides in the secret card's data-full,
	// not an inline GOCOV_TOKEN= line, since it is masked by default.
	if !strings.Contains(body, "GOCOV_SERVER") || !strings.Contains(body, "https://gocov.example") {
		t.Errorf("setup page misses the self-hosted server URL:\n%s", body)
	}
	if !strings.Contains(body, `data-full="ws-secret"`) {
		t.Errorf("setup page misses the workspace token:\n%s", body)
	}
	if !strings.Contains(body, "bitbucket-pipelines.yml") {
		t.Errorf("bitbucket workspace must get the Pipelines snippet:\n%s", body)
	}
	// The CI step is a single clean card — no waiting card stacked under it —
	// with a button to advance to the First-upload step.
	if strings.Contains(body, "Waiting for your first upload") {
		t.Errorf("CI step must not stack the waiting card:\n%s", body)
	}
	if !strings.Contains(body, `href="/workspaces/acme/setup?awaiting=1"`) {
		t.Errorf("CI step misses the advance-to-first-upload button:\n%s", body)
	}
	// Advancing (or the poll target) shows the polling waiting state.
	await := get(f, "/workspaces/acme/setup?awaiting=1", sess).Body.String()
	if !strings.Contains(await, "Waiting for your first upload") ||
		!strings.Contains(await, `hx-get="/workspaces/acme/setup/status"`) {
		t.Errorf("first-upload step misses the waiting state:\n%s", await)
	}
	if st := get(f, "/workspaces/acme/setup/status", sess); !strings.Contains(st.Body.String(), "Waiting") {
		t.Errorf("status endpoint should still wait:\n%s", st.Body)
	}

	// First upload auto-registers the repo; the poll flips to the link.
	up := doUpload(t, f, "ws-secret", map[string]string{
		"repo": "acme/newrepo", "commit": "c1", "branch": "main"}, testProfile)
	if up.Code != http.StatusCreated {
		t.Fatalf("first upload: status = %d, body = %s", up.Code, up.Body)
	}
	// Once the upload lands the poll redirects to the clean done page
	// rather than swapping the card in under the CI step; landed=1 marks
	// that one load for the analytics snippet.
	st := get(f, "/workspaces/acme/setup/status", sess)
	if loc := st.Header().Get("HX-Redirect"); loc != "/workspaces/acme/setup?landed=1" {
		t.Errorf("status endpoint did not redirect on flip: HX-Redirect=%q", loc)
	}
	// The reloaded setup page is the clean First-upload done state: the
	// report summary, no CI card, no polling.
	rec = get(f, "/workspaces/acme/setup", sess)
	body = rec.Body.String()
	if !strings.Contains(body, "Coverage is flowing") ||
		!strings.Contains(body, `href="/repos/acme/newrepo"`) ||
		!strings.Contains(body, "First report") || !strings.Contains(body, "Lines covered") {
		t.Errorf("done page missing content:\n%s", body)
	}
	if strings.Contains(body, "GOCOV_TOKEN") || strings.Contains(body, "hx-get") {
		t.Errorf("done page should drop the CI card and stop polling:\n%s", body)
	}
}

func TestSetupPageGitHubSnippet(t *testing.T) {
	st := storemem.New()
	if err := st.CreateWorkspace(t.Context(),
		&store.Workspace{Forge: "github", Prefix: "myorg", Token: "gh-secret", DefaultBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	gh := &fakeProvider{name: "github", identity: &auth.Identity{
		ForgeUUID: "42", DisplayName: "Hub Dev", Workspaces: []string{"myorg"}, OwnedWorkspaces: []string{"myorg"},
	}}
	f := &fixture{
		srv: New(Config{
			Store:   st,
			Blobs:   blobmem.New(),
			Parsers: map[string]profile.Parser{"go": profile.GoParser{}},
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
		"gocov/gocov-action@v1",
		"${{ vars.GOCOV_SERVER }}",
		"${{ secrets.GOCOV_TOKEN }}",
		`data-full="gh-secret"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("github setup page misses %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "bitbucket-pipelines.yml") {
		t.Error("github workspace got the Bitbucket snippet")
	}
	// The GitHub snippet is the action, not a raw curl install.
	if strings.Contains(body, "curl -fsSL https://github.com/gocov/gocov/releases") {
		t.Error("github snippet should use the action, not a raw binary download")
	}
}

func TestSetupPageGitLabSnippet(t *testing.T) {
	st := storemem.New()
	if err := st.CreateWorkspace(t.Context(),
		&store.Workspace{Forge: "gitlab", Prefix: "grp/team", Token: "gl-secret", DefaultBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	gl := &fakeProvider{name: "gitlab", identity: &auth.Identity{
		ForgeUUID: "9", DisplayName: "GL Dev", Workspaces: []string{"grp/team"},
	}}
	f := &fixture{
		srv: New(Config{
			Store:   st,
			Blobs:   blobmem.New(),
			Parsers: map[string]profile.Parser{"go": profile.GoParser{}},
			BaseURL: "https://gocov.example",
			Auths:   []auth.Provider{gl},
		}),
		store: st,
	}
	sess := signInVia(t, f, "gitlab")

	body := get(f, "/workspaces/grp%2Fteam/setup", sess).Body.String()
	// gitlab.com workspaces get the CI/CD Catalog component, which does
	// the pinned download and checksum itself.
	if !strings.Contains(body, ".gitlab-ci.yml") || !strings.Contains(body, "component: gitlab.com/gocov/gocov/upload@1") {
		t.Errorf("gitlab setup page misses the component snippet:\n%s", body)
	}
	if strings.Contains(body, "sha256sum") {
		t.Error("gitlab.com snippet should include the component, not a raw binary download")
	}
	if !strings.Contains(body, "server: https://gocov.example") {
		t.Error("self-hosted gitlab snippet should pass this server to the component")
	}
	if strings.Contains(body, "bitbucket-pipelines.yml") {
		t.Error("gitlab workspace got the Bitbucket snippet")
	}
}

// A connected workspace's setup page leads with an OIDC identity token
// instead of the upload token: the GitHub snippet grants id-token: write
// and passes no token, and the token itself is folded away underneath,
// still reachable for owners.
func TestSetupPageTokenlessGitHub(t *testing.T) {
	st := storemem.New()
	if err := st.CreateWorkspace(t.Context(), &store.Workspace{
		Forge: "github", Prefix: "myorg", Token: "gh-secret", DefaultBranch: "main",
		GitHubInstallationID: 4242}); err != nil {
		t.Fatal(err)
	}
	gh := &fakeProvider{name: "github", identity: &auth.Identity{
		ForgeUUID: "42", DisplayName: "Hub Dev", Workspaces: []string{"myorg"}, OwnedWorkspaces: []string{"myorg"},
	}}
	f := &fixture{
		srv: New(Config{
			Store:   st,
			Blobs:   blobmem.New(),
			Parsers: map[string]profile.Parser{"go": profile.GoParser{}},
			BaseURL: "https://gocov.example",
			Auths:   []auth.Provider{gh},
		}),
		store: st,
	}
	sess := signInVia(t, f, "github")

	body := get(f, "/workspaces/myorg/setup", sess).Body.String()
	for _, want := range []string{
		"id-token: write",
		"gocov/gocov-action@v1",
		"${{ vars.GOCOV_SERVER }}",
		"Nothing to paste",
		`<details class="alt">`,
		`data-full="gh-secret"`,
		`data-ph-click="show_token_clicked"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("tokenless github setup page misses %q:\n%s", want, body)
		}
	}
	// The folded token section still says how a token would be passed; the
	// snippet itself must not.
	if sn := snippetOf(body); strings.Contains(sn, "GOCOV_TOKEN") {
		t.Errorf("tokenless github snippet should not pass a token:\n%s", sn)
	}
}

// snippetOf returns the copyable CI snippet on a setup page.
func snippetOf(body string) string {
	_, rest, ok := strings.Cut(body, `<code id="snippet">`)
	if !ok {
		return ""
	}
	sn, _, _ := strings.Cut(rest, "</code>")
	return sn
}

// Same on Bitbucket: the step names the server under oidc.audiences and
// the pipe gets no TOKEN.
func TestSetupPageTokenlessBitbucket(t *testing.T) {
	st := storemem.New()
	if err := st.CreateWorkspace(t.Context(), &store.Workspace{
		Forge: "bitbucket", Prefix: "acme", Token: "ws-secret", DefaultBranch: "main",
		BitbucketGrantAccount: "gocov-bot", BitbucketRefreshToken: "rt"}); err != nil {
		t.Fatal(err)
	}
	bb := &fakeProvider{name: "bitbucket", identity: &auth.Identity{
		ForgeUUID: "1", DisplayName: "Dev", Workspaces: []string{"acme"},
	}}
	f := &fixture{
		srv: New(Config{
			Store:   st,
			Blobs:   blobmem.New(),
			Parsers: map[string]profile.Parser{"go": profile.GoParser{}},
			BaseURL: hosted.DefaultServer,
			Auths:   []auth.Provider{bb},
		}),
		store: st,
	}
	sess := signInVia(t, f, "bitbucket")

	body := get(f, "/workspaces/acme/setup", sess).Body.String()
	for _, want := range []string{
		"oidc:",
		"- " + hosted.DefaultServer,
		"pipe: docker://gocov/upload-pipe:",
		"FILES: coverage.out",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("tokenless bitbucket setup page misses %q:\n%s", want, body)
		}
	}
	if sn := snippetOf(body); strings.Contains(sn, "GOCOV_TOKEN") {
		t.Errorf("tokenless bitbucket snippet should not pass a token:\n%s", sn)
	}
	// A member (not an owner) still sees where the token would go, but no
	// token value.
	if strings.Contains(body, `data-full="ws-secret"`) || !strings.Contains(body, "Shown to workspace owners only") {
		t.Errorf("member should not see the token value:\n%s", body)
	}
}

// A self-managed GitLab cannot include components from gitlab.com's
// catalog, so its wizard keeps the raw download recipe with the checksum
// line. The server knows which GitLab it faces from the trusted OIDC
// issuers: set means self-managed.
func TestSetupPageSelfManagedGitLabSnippet(t *testing.T) {
	st := storemem.New()
	if err := st.CreateWorkspace(t.Context(),
		&store.Workspace{Forge: "gitlab", Prefix: "grp/team", Token: "gl-secret", DefaultBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	gl := &fakeProvider{name: "gitlab", identity: &auth.Identity{
		ForgeUUID: "9", DisplayName: "GL Dev", Workspaces: []string{"grp/team"},
	}}
	f := &fixture{
		srv: New(Config{
			Store:       st,
			Blobs:       blobmem.New(),
			Parsers:     map[string]profile.Parser{"go": profile.GoParser{}},
			BaseURL:     "https://gocov.example",
			Auths:       []auth.Provider{gl},
			OIDCIssuers: []string{"https://gitlab.example"},
		}),
		store: st,
	}
	sess := signInVia(t, f, "gitlab")

	body := get(f, "/workspaces/grp%2Fteam/setup", sess).Body.String()
	if !strings.Contains(body, ".gitlab-ci.yml") || !strings.Contains(body, "sha256sum") {
		t.Errorf("self-managed gitlab setup page misses the CI snippet with checksum verification:\n%s", body)
	}
	if strings.Contains(body, "component: gitlab.com") {
		t.Error("self-managed gitlab snippet should not include the gitlab.com component")
	}
}

// On the hosted instance the CLI already defaults to it, so onboarding
// drops GOCOV_SERVER entirely and installs the release binary rather than
// `go run @latest` (which needs a matching Go toolchain).
func TestSetupPageHostedOmitsServer(t *testing.T) {
	st := storemem.New()
	if err := st.CreateWorkspace(t.Context(),
		&store.Workspace{Forge: "bitbucket", Prefix: "acme", Token: "ws-secret", DefaultBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	bb := &fakeProvider{name: "bitbucket", identity: &auth.Identity{
		ForgeUUID: "1", DisplayName: "Dev", Workspaces: []string{"acme"}, OwnedWorkspaces: []string{"acme"},
	}}
	f := &fixture{
		srv: New(Config{
			Store:   st,
			Blobs:   blobmem.New(),
			Parsers: map[string]profile.Parser{"go": profile.GoParser{}},
			BaseURL: hosted.DefaultServer,
			Auths:   []auth.Provider{bb},
		}),
		store: st,
	}
	start := get(f, "/oauth/bitbucket/start")
	stateCk := cookieNamed(t, start, stateCookie)
	state, _, _ := strings.Cut(stateCk.Value, "|")
	cb := get(f, "/oauth/bitbucket/callback?code=x&state="+url.QueryEscape(state), stateCk)
	sess := cookieNamed(t, cb, sessionCookie)

	body := get(f, "/workspaces/acme/setup", sess).Body.String()
	if strings.Contains(body, "GOCOV_SERVER") {
		t.Errorf("hosted onboarding should omit GOCOV_SERVER:\n%s", body)
	}
	if !strings.Contains(body, `data-full="ws-secret"`) {
		t.Errorf("hosted onboarding still needs the token:\n%s", body)
	}
	if !strings.Contains(body, "pipe: docker://gocov/upload-pipe:") {
		t.Errorf("bitbucket onboarding should upload with the gocov pipe:\n%s", body)
	}
	if strings.Contains(body, "go run github.com/gocov/gocov") {
		t.Errorf("onboarding should not use go run @latest:\n%s", body)
	}
}

// TestGitLabSubgroupWorkspace covers D2 end to end at the UI layer: a
// workspace registered at subgroup depth ("grp/sub") admits its member,
// serves its pages behind a %2F-encoded prefix, and scopes visibility to
// projects below the subgroup.
func TestGitLabSubgroupWorkspace(t *testing.T) {
	ctx := t.Context()
	st := storemem.New()
	ws := &store.Workspace{Forge: "gitlab", Prefix: "grp/sub", Token: "ws-secret", DefaultBranch: "main"}
	if err := st.CreateWorkspace(ctx, ws); err != nil {
		t.Fatal(err)
	}
	repo := &store.Repo{Forge: "gitlab", Slug: "grp/sub/proj", Token: "repo-token", DefaultBranch: "main"}
	if err := st.CreateRepo(ctx, repo); err != nil {
		t.Fatal(err)
	}
	other := &store.Repo{Forge: "gitlab", Slug: "grp/elsewhere", Token: "other-token", DefaultBranch: "main"}
	if err := st.CreateRepo(ctx, other); err != nil {
		t.Fatal(err)
	}
	f := &fixture{
		srv: New(Config{
			Store:   st,
			Blobs:   blobmem.New(),
			Parsers: map[string]profile.Parser{"go": profile.GoParser{}},
			BaseURL: "https://gocov.example",
			Auths: []auth.Provider{&fakeProvider{name: "gitlab", identity: &auth.Identity{
				ForgeUUID:   "12345",
				DisplayName: "Jane Dev",
				Email:       "jane@example.com",
				// The forge reports the subgroup's full path, not its root.
				Workspaces: []string{"grp/sub", "janedev"},
			}}},
		}),
		store: st,
	}
	sess := signInVia(t, f, "gitlab")

	// The settings and setup pages live behind the %2F-encoded prefix.
	for _, path := range []string{"/workspaces/grp%2Fsub", "/workspaces/grp%2Fsub/setup"} {
		rec := get(f, path, sess)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s: status = %d, want 200", path, rec.Code)
		}
	}
	// The raw-slash form must not resolve to the workspace pages.
	if rec := get(f, "/workspaces/grp/sub", sess); rec.Code != http.StatusNotFound {
		t.Errorf("raw-slash workspace path: status = %d, want 404", rec.Code)
	}

	// Membership at subgroup depth scopes repo visibility: the subgroup's
	// project is visible, the sibling project outside it is not.
	if rec := get(f, "/repos/grp/sub/proj", sess); rec.Code != http.StatusOK {
		t.Errorf("member repo page: status = %d, want 200", rec.Code)
	}
	if rec := get(f, "/repos/grp/elsewhere", sess); rec.Code != http.StatusNotFound {
		t.Errorf("non-member repo page: status = %d, want 404", rec.Code)
	}

	// The subgroup already has a repo, so its setup page is the clean
	// First-upload done state — and it still resolves through the
	// %2F-encoded prefix. (The forge-specific CI snippet, shown only before
	// the first upload, is covered by TestSetupPageGitLabSnippet.)
	rec := get(f, "/workspaces/grp%2Fsub/setup", sess)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Coverage is flowing") {
		t.Errorf("subgroup setup done page: status %d\n%s", rec.Code, rec.Body)
	}
}
