// Anonymous read-only report pages for public repos, end to end: which
// pages open without a session, which keep the login wall, and the
// switches (per repo and per instance) that close them again. The access
// decision itself lives in scope.go; these tests drive it through the
// routed pages like a signed-out browser would.

package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gocov/gocov/internal/auth"
	blobmem "github.com/gocov/gocov/internal/blobstore/memory"
	forgefake "github.com/gocov/gocov/internal/forge/fake"
	"github.com/gocov/gocov/internal/profile"
	"github.com/gocov/gocov/internal/store"
	storemem "github.com/gocov/gocov/internal/store/memory"
)

// newPublicFixture builds a sign-in-enabled server over the repo
// acme/widgets with the given forge-reported visibility. instanceOn is
// the GOCOV_PUBLIC_REPORTS switch. A workspace row for acme exists, so a
// member signing in through the fake provider lands with a membership.
func newPublicFixture(t *testing.T, visibility string, instanceOn bool) *fixture {
	t.Helper()
	ctx := t.Context()
	st := storemem.New()
	repo := &store.Repo{
		Forge: "bitbucket", Slug: "acme/widgets", Token: "secret-token",
		DefaultBranch: "main", Visibility: visibility,
	}
	if err := st.CreateRepo(ctx, repo); err != nil {
		t.Fatal(err)
	}
	ws := &store.Workspace{Forge: "bitbucket", Prefix: "acme", Token: "ws-secret", DefaultBranch: "main"}
	if err := st.CreateWorkspace(ctx, ws); err != nil {
		t.Fatal(err)
	}
	blobs := blobmem.New()
	srv := New(Config{
		Store:         st,
		Blobs:         blobs,
		Parsers:       map[string]profile.Parser{"go": profile.GoParser{}},
		BaseURL:       "https://gocov.example",
		Auths:         []auth.Provider{&fakeProvider{identity: memberIdentity()}},
		PublicReports: instanceOn,
	})
	return &fixture{srv: srv, store: st, blobs: blobs, repo: repo}
}

// seedUpload stores one upload with per-file rows and its raw profile
// blob, the way the upload API would have left them.
func seedUpload(t *testing.T, f *fixture) *store.Upload {
	t.Helper()
	ctx := t.Context()
	u := &store.Upload{
		RepoID: f.repo.ID, CommitSHA: "abc1234def", Branch: "main", Format: "go",
		TotalPct: 80, CoveredStmts: 8, TotalStmts: 10,
		RawBlobKey: "profiles/1/raw", Part: "default",
	}
	files := []*store.UploadFile{{
		Path: "a.go", Pct: 80, CoveredStmts: 8, TotalStmts: 10,
		Blocks: []profile.Block{
			{StartLine: 1, EndLine: 2, NumStmts: 8, Count: 1},
			{StartLine: 3, EndLine: 4, NumStmts: 2, Count: 0},
		},
	}}
	if err := f.store.CreateUpload(ctx, u, files); err != nil {
		t.Fatal(err)
	}
	if err := f.blobs.Put(ctx, u.RawBlobKey, []byte(testProfile)); err != nil {
		t.Fatal(err)
	}
	return u
}

func wantLoginRedirect(t *testing.T, rec *httptest.ResponseRecorder, path string) {
	t.Helper()
	if rec.Code != http.StatusFound || !strings.HasPrefix(rec.Header().Get("Location"), "/login") {
		t.Errorf("GET %s anonymous: %d -> %q, want the login redirect", path, rec.Code, rec.Header().Get("Location"))
	}
}

func TestPublicRepoReportPagesOpenAnonymously(t *testing.T) {
	f := newPublicFixture(t, store.VisibilityPublic, true)
	u := seedUpload(t, f)

	repoPage := get(f, "/repos/bitbucket/acme/widgets")
	if repoPage.Code != http.StatusOK {
		t.Fatalf("repo page anonymous: status = %d", repoPage.Code)
	}
	body := repoPage.Body.String()
	if !strings.Contains(body, "acme/widgets") {
		t.Error("repo page misses the slug")
	}
	// Read-only: no settings link, no member chrome — and the visitor CTA
	// band is there.
	if strings.Contains(body, "/repo-settings/bitbucket/") {
		t.Error("anonymous repo page shows the settings link")
	}
	if strings.Contains(body, "Sign out") {
		t.Error("anonymous repo page shows the signed-in chrome")
	}
	if !strings.Contains(body, "public-cta") {
		t.Error("anonymous repo page misses the CTA band")
	}
	// The anonymous render is briefly cacheable and must say so — a shared
	// cache with no policy would cache heuristically and keep serving after
	// the switch turns the pages off; Vary keeps it from answering a
	// signed-in member with the stored anonymous variant.
	if cc := repoPage.Header().Get("Cache-Control"); cc != "public, max-age=60" {
		t.Errorf("anonymous public page Cache-Control = %q, want public, max-age=60", cc)
	}
	if v := repoPage.Header().Get("Vary"); v != "Cookie" {
		t.Errorf("anonymous public page Vary = %q, want Cookie", v)
	}

	uploadPage := get(f, "/uploads/1")
	if uploadPage.Code != http.StatusOK {
		t.Fatalf("upload page anonymous: status = %d", uploadPage.Code)
	}
	if !strings.Contains(uploadPage.Body.String(), "public-cta") {
		t.Error("anonymous upload page misses the CTA band")
	}

	src := get(f, "/uploads/1/files/a.go")
	if src.Code != http.StatusOK {
		t.Fatalf("source page anonymous: status = %d", src.Code)
	}

	// Crawlers probe with HEAD; the mux serves it through the GET route,
	// so the sessionless pass-through must admit it too.
	headReq := httptest.NewRequest(http.MethodHead, "/repos/bitbucket/acme/widgets", nil)
	headRec := httptest.NewRecorder()
	f.srv.ServeHTTP(headRec, headReq)
	if headRec.Code != http.StatusOK {
		t.Errorf("HEAD repo page anonymous: status = %d", headRec.Code)
	}

	prof := get(f, "/uploads/1/profile")
	if prof.Code != http.StatusOK {
		t.Fatalf("raw profile anonymous: status = %d", prof.Code)
	}
	if prof.Body.String() != testProfile {
		t.Errorf("raw profile body = %q", prof.Body.String())
	}

	// An unrouted path under the public prefixes still answers with the
	// login redirect, keeping the signed-out response surface uniform.
	wantLoginRedirect(t, get(f, "/uploads/"+strconv.FormatInt(u.ID, 10)+"/bogus"), "/uploads/{id}/bogus")
}

func TestNonPublicRepoKeepsLoginWallForAnonymous(t *testing.T) {
	for _, visibility := range []string{"", store.VisibilityPrivate} {
		f := newPublicFixture(t, visibility, true)
		seedUpload(t, f)

		// Today's behavior exactly, and indistinguishable from a slug or
		// upload that does not exist — a signed-out probe learns nothing.
		for _, path := range []string{
			"/repos/bitbucket/acme/widgets",
			"/repos/bitbucket/no/such",
			"/uploads/1",
			"/uploads/1/profile",
			"/uploads/1/files/a.go",
			"/uploads/999",
			"/uploads/notanid",
			"/uploads/1/bogus",
		} {
			wantLoginRedirect(t, get(f, path), path)
		}
	}
}

func TestRepoSettingsSwitchClosesPublicPages(t *testing.T) {
	f := newPublicFixture(t, store.VisibilityPublic, true)
	seedUpload(t, f)

	f.repo.PublicReportsDisabled = true
	if err := f.store.UpdateRepo(t.Context(), f.repo); err != nil {
		t.Fatal(err)
	}
	wantLoginRedirect(t, get(f, "/repos/bitbucket/acme/widgets"), "/repos/bitbucket/acme/widgets")
	wantLoginRedirect(t, get(f, "/uploads/1"), "/uploads/1")
}

func TestInstanceSwitchClosesPublicPages(t *testing.T) {
	f := newPublicFixture(t, store.VisibilityPublic, false)
	seedUpload(t, f)

	wantLoginRedirect(t, get(f, "/repos/bitbucket/acme/widgets"), "/repos/bitbucket/acme/widgets")
	wantLoginRedirect(t, get(f, "/uploads/1"), "/uploads/1")
	wantLoginRedirect(t, get(f, "/uploads/1/profile"), "/uploads/1/profile")
}

func TestMemberViewOfPublicRepoIsUnchanged(t *testing.T) {
	f := newPublicFixture(t, store.VisibilityPublic, true)
	seedUpload(t, f)
	sess := signIn(t, f, "/")

	rec := get(f, "/repos/bitbucket/acme/widgets", sess)
	if rec.Code != http.StatusOK {
		t.Fatalf("member repo page: status = %d", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "public-cta") {
		t.Error("signed-in member sees the visitor CTA band")
	}
	if !strings.Contains(body, "/repo-settings/bitbucket/acme/widgets") {
		t.Error("member repo page misses the settings link")
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("member page Cache-Control = %q, want no-store", cc)
	}
}

// A signed-in user who is not a member of the repo's workspace passes
// through the public branch and must get the read-only view: no settings
// link (clicking it would 404) — and, being signed in, no visitor CTA
// either. Before public reports this state was unreachable (a non-member
// always 404d).
func TestSignedInNonMemberGetsReadOnlyPublicView(t *testing.T) {
	ctx := t.Context()
	st := storemem.New()
	repo := &store.Repo{
		Forge: "bitbucket", Slug: "acme/widgets", Token: "secret-token",
		DefaultBranch: "main", Visibility: store.VisibilityPublic,
	}
	if err := st.CreateRepo(ctx, repo); err != nil {
		t.Fatal(err)
	}
	// Both workspaces are tracked, so the outsider may sign in — as a
	// member of beta, never of acme.
	for _, prefix := range []string{"acme", "beta"} {
		if err := st.CreateWorkspace(ctx, &store.Workspace{Forge: "bitbucket", Prefix: prefix, Token: "ws-" + prefix, DefaultBranch: "main"}); err != nil {
			t.Fatal(err)
		}
	}
	outsider := &auth.Identity{ForgeUUID: "{uuid-out}", DisplayName: "Sam Outsider",
		Email: "sam@example.com", Workspaces: []string{"beta"}}
	srv := New(Config{
		Store:         st,
		Blobs:         blobmem.New(),
		Parsers:       map[string]profile.Parser{"go": profile.GoParser{}},
		BaseURL:       "https://gocov.example",
		Auths:         []auth.Provider{&fakeProvider{identity: outsider}},
		PublicReports: true,
	})
	f := &fixture{srv: srv, store: st, repo: repo}
	sess := signIn(t, f, "/")

	rec := get(f, "/repos/bitbucket/acme/widgets", sess)
	if rec.Code != http.StatusOK {
		t.Fatalf("non-member on public repo: status = %d", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "/repo-settings/bitbucket/") {
		t.Error("signed-in non-member sees the settings link")
	}
	if strings.Contains(body, "public-cta") {
		t.Error("signed-in visitor sees the anonymous CTA band")
	}
}

func TestPublicReportsToggleInRepoSettings(t *testing.T) {
	f := newPublicFixture(t, store.VisibilityPublic, true)
	sess := signIn(t, f, "/")

	// The switch renders for a public repo.
	page := get(f, "/repo-settings/bitbucket/acme/widgets", sess)
	if page.Code != http.StatusOK {
		t.Fatalf("settings page: status = %d", page.Code)
	}
	if !strings.Contains(page.Body.String(), "Public reports") {
		t.Error("settings page misses the Public reports switch")
	}

	// Saving without the checkbox turns public pages off at once.
	rec := postForm(f, "/repo-settings/save/bitbucket/acme/widgets", url.Values{"default_branch": {"main"}}, sess)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("save: status = %d, body = %s", rec.Code, rec.Body)
	}
	repo, err := f.store.RepoBySlug(t.Context(), "bitbucket", "acme/widgets")
	if err != nil {
		t.Fatal(err)
	}
	if !repo.PublicReportsDisabled {
		t.Error("saving with the checkbox off did not disable public reports")
	}
	wantLoginRedirect(t, get(f, "/repos/bitbucket/acme/widgets"), "/repos/bitbucket/acme/widgets")

	// Saving with the checkbox on reopens them.
	rec = postForm(f, "/repo-settings/save/bitbucket/acme/widgets", url.Values{"default_branch": {"main"}, "public_reports": {"on"}}, sess)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("re-save: status = %d", rec.Code)
	}
	if rec := get(f, "/repos/bitbucket/acme/widgets"); rec.Code != http.StatusOK {
		t.Errorf("public page after reopening: status = %d", rec.Code)
	}
}

func TestPrivateRepoSettingsHideTheSwitchAndKeepTheValue(t *testing.T) {
	f := newPublicFixture(t, store.VisibilityPrivate, true)
	sess := signIn(t, f, "/")

	page := get(f, "/repo-settings/bitbucket/acme/widgets", sess)
	if strings.Contains(page.Body.String(), "Public reports") {
		t.Error("private repo settings render the Public reports switch")
	}
	// A save without the (absent) checkbox must not flip the stored value.
	if rec := postForm(f, "/repo-settings/save/bitbucket/acme/widgets", url.Values{"default_branch": {"main"}}, sess); rec.Code != http.StatusSeeOther {
		t.Fatalf("save: status = %d", rec.Code)
	}
	repo, err := f.store.RepoBySlug(t.Context(), "bitbucket", "acme/widgets")
	if err != nil {
		t.Fatal(err)
	}
	if repo.PublicReportsDisabled {
		t.Error("saving a private repo's settings flipped PublicReportsDisabled")
	}
}

// TestUploadRefreshesVisibility drives the whole loop: a repo whose forge
// flips it public becomes anonymously viewable by the next upload — and
// while the cached answer is fresh, further uploads skip the forge
// round-trip instead of re-asking on every part.
func TestUploadRefreshesVisibility(t *testing.T) {
	f := newFixture(t, map[string]string{})
	f.forge.Visibility = store.VisibilityPublic

	rec := doUpload(t, f, "secret-token", map[string]string{"commit": "c1", "branch": "main"}, testProfile)
	if rec.Code != http.StatusCreated {
		t.Fatalf("upload: status = %d, body = %s", rec.Code, rec.Body)
	}
	repo, err := f.store.RepoBySlug(t.Context(), "bitbucket", "acme/widgets")
	if err != nil {
		t.Fatal(err)
	}
	if repo.Visibility != store.VisibilityPublic {
		t.Errorf("visibility after upload = %q, want %q", repo.Visibility, store.VisibilityPublic)
	}
	if got := len(f.forge.VisibilityCalls); got != 1 {
		t.Fatalf("visibility calls after first upload = %d, want 1", got)
	}

	// The answer is fresh, so the next upload (another part, a retry)
	// must not spend a forge round-trip on the same question.
	f.forge.Visibility = store.VisibilityPrivate
	if rec := doUpload(t, f, "secret-token", map[string]string{"commit": "c2", "branch": "main"}, testProfile); rec.Code != http.StatusCreated {
		t.Fatalf("second upload: status = %d", rec.Code)
	}
	if got := len(f.forge.VisibilityCalls); got != 1 {
		t.Errorf("visibility calls after fresh-answer upload = %d, want still 1", got)
	}
	if repo, _ = f.store.RepoBySlug(t.Context(), "bitbucket", "acme/widgets"); repo.Visibility != store.VisibilityPublic {
		t.Errorf("fresh-answer upload rewrote visibility to %q", repo.Visibility)
	}

	// Once the answer has aged out, the next upload re-asks and picks up
	// the private flip.
	f.srv.pipeline.VisibilityUploadTTL = time.Nanosecond
	if rec := doUpload(t, f, "secret-token", map[string]string{"commit": "c3", "branch": "main"}, testProfile); rec.Code != http.StatusCreated {
		t.Fatalf("third upload: status = %d", rec.Code)
	}
	repo, err = f.store.RepoBySlug(t.Context(), "bitbucket", "acme/widgets")
	if err != nil {
		t.Fatal(err)
	}
	if repo.Visibility != store.VisibilityPrivate {
		t.Errorf("visibility after flip = %q, want %q", repo.Visibility, store.VisibilityPrivate)
	}
}

// TestStalePublicAnswerIsReverifiedWhenServed covers the no-more-uploads
// hole: a repo cached public whose CI went quiet is re-verified in the
// background when its pages are served anonymously on a stale answer, so
// a private flip on the forge closes the pages without any upload.
func TestStalePublicAnswerIsReverifiedWhenServed(t *testing.T) {
	ctx := t.Context()
	st := storemem.New()
	// Cached public, but the stamp is zero — the forge has never answered
	// within any TTL — and the forge now says private.
	repo := &store.Repo{
		Forge: "bitbucket", Slug: "acme/widgets", Token: "secret-token",
		DefaultBranch: "main", Visibility: store.VisibilityPublic,
	}
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
	ff := forgefake.New()
	ff.Visibility = store.VisibilityPrivate
	blobs := blobmem.New()
	srv := New(Config{
		Store:            st,
		Blobs:            blobs,
		Parsers:          map[string]profile.Parser{"go": profile.GoParser{}},
		BaseURL:          "https://gocov.example",
		Auths:            []auth.Provider{&fakeProvider{identity: memberIdentity()}},
		PublicReports:    true,
		BitbucketConnect: &fakeBBConnect{grantForge: ff},
	})
	f := &fixture{srv: srv, store: st, blobs: blobs, forge: ff, repo: repo}
	seedUpload(t, f)

	// The stale answer still serves — the re-check must not cost this
	// request a forge round-trip — but it kicks the re-verification off.
	if rec := get(f, "/repos/bitbucket/acme/widgets"); rec.Code != http.StatusOK {
		t.Fatalf("stale public page: status = %d", rec.Code)
	}
	waitForVisibility(t, st, "acme/widgets", store.VisibilityPrivate)

	// The answer landed: the pages are closed for the requests after it.
	wantLoginRedirect(t, get(f, "/repos/bitbucket/acme/widgets"), "/repos/bitbucket/acme/widgets")
	wantLoginRedirect(t, get(f, "/uploads/1"), "/uploads/1")
}

// waitForVisibility polls the store until the repo's cached visibility
// matches — the background re-check runs on its own goroutine. (Mirrors
// the helper of the same name in internal/core's tests.)
func waitForVisibility(t *testing.T, st *storemem.Store, slug, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		stored, err := st.RepoBySlug(t.Context(), "bitbucket", slug)
		if err != nil {
			t.Fatal(err)
		}
		if stored.Visibility == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("visibility = %q, want %q (background re-check never landed)", stored.Visibility, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
