package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	blobmem "github.com/gocov/gocov/internal/blobstore/memory"
	"github.com/gocov/gocov/internal/forge"
	forgefake "github.com/gocov/gocov/internal/forge/fake"
	"github.com/gocov/gocov/internal/forge/github"
	"github.com/gocov/gocov/internal/profile"
	"github.com/gocov/gocov/internal/store"
	storemem "github.com/gocov/gocov/internal/store/memory"
)

// newTokenlessFixture builds a github-forge server with repo acme/widgets
// whose workspace is connected to App installation 77 — the setup a
// tokenless fork-PR upload requires. The fake App accepts every claim
// unless a test installs its own verify.
func newTokenlessFixture(t *testing.T) (*fixture, *fakeGitHubApp, *store.Workspace) {
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
	srv := New(Config{
		Store:     st,
		Blobs:     blobs,
		Parsers:   map[string]profile.Parser{"go": profile.GoParser{}},
		BaseURL:   "https://gocov.example",
		GitHubApp: app,
	})
	return &fixture{srv: srv, store: st, blobs: blobs, forge: ff, repo: repo}, app, ws
}

// tokenlessFields is a valid fork-PR upload claim for the fixture repo.
func tokenlessFields() map[string]string {
	return map[string]string{
		"repo":        "acme/widgets",
		"commit":      "abc123",
		"branch":      "feature",
		"pr_id":       "42",
		"run_id":      "9001",
		"run_attempt": "2",
		"head_repo":   "forker/widgets",
	}
}

func TestTokenlessUploadHappyPath(t *testing.T) {
	f, app, _ := newTokenlessFixture(t)
	rec := doUpload(t, f, "", tokenlessFields(), testProfile)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}

	// The claim reached verification exactly as sent.
	if len(app.verifyCalls) != 1 {
		t.Fatalf("got %d verify calls, want 1", len(app.verifyCalls))
	}
	want := github.RunClaim{RepoSlug: "acme/widgets", RunID: 9001, RunAttempt: 2, PRNumber: 42, HeadSHA: "abc123", HeadRepo: "forker/widgets"}
	if app.verifyCalls[0] != want {
		t.Errorf("verified claim = %+v, want %+v", app.verifyCalls[0], want)
	}

	// The upload landed marked tokenless — server-set, not a form field.
	var resp uploadResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	u, err := f.store.Upload(t.Context(), resp.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !u.Meta.Tokenless {
		t.Error("upload not marked tokenless")
	}
	if u.PRID != "42" {
		t.Errorf("pr id = %q, want 42", u.PRID)
	}

	// Status and PR comment went out through the App-installation forge.
	if len(f.forge.StatusCalls) != 1 {
		t.Errorf("got %d status calls, want 1", len(f.forge.StatusCalls))
	}
	if len(f.forge.CommentCalls) != 1 {
		t.Errorf("got %d PR comment calls, want 1", len(f.forge.CommentCalls))
	}
}

// A form field must not be able to fake the tokenless mark on a
// token-authenticated upload.
func TestTokenAuthedUploadNotMarkedTokenless(t *testing.T) {
	f, app, _ := newTokenlessFixture(t)
	fields := tokenlessFields()
	fields["tokenless"] = "true"
	rec := doUpload(t, f, "secret-token", fields, testProfile)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if len(app.verifyCalls) != 0 {
		t.Errorf("token-authenticated upload went through claim verification")
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
		t.Error("token-authenticated upload marked tokenless")
	}
}

func TestTokenlessUploadRejectedClaim(t *testing.T) {
	f, app, _ := newTokenlessFixture(t)
	app.verify = func(int64, github.RunClaim) error {
		return &github.ClaimRejectedError{Reason: "workflow run 9001 is completed; only a run still in progress may upload"}
	}
	rec := doUpload(t, f, "", tokenlessFields(), testProfile)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "still in progress") {
		t.Errorf("rejection reason not surfaced to the uploader: %s", rec.Body)
	}
	if ups, _ := f.store.ListUploads(t.Context(), f.repo.ID, 0); len(ups) != 0 {
		t.Errorf("rejected upload was stored")
	}
}

func TestTokenlessUploadWithoutInstallation(t *testing.T) {
	f, _, ws := newTokenlessFixture(t)
	ws.GitHubInstallationID = 0
	if err := f.store.UpdateWorkspace(t.Context(), ws); err != nil {
		t.Fatal(err)
	}
	rec := doUpload(t, f, "", tokenlessFields(), testProfile)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "GitHub App installed") {
		t.Errorf("missing-installation reason not surfaced: %s", rec.Body)
	}
}

// A revoked installation refuses the upload with a reconnect hint and
// marks the workspace broken — the same lazy detection the forge push
// path uses.
func TestTokenlessUploadRevokedInstallation(t *testing.T) {
	f, app, _ := newTokenlessFixture(t)
	app.verify = func(int64, github.RunClaim) error {
		return fmt.Errorf("%w: installation 77 is gone", forge.ErrCredentialsRevoked)
	}
	rec := doUpload(t, f, "", tokenlessFields(), testProfile)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "reconnect") {
		t.Errorf("reconnect hint missing: %s", rec.Body)
	}
	ws, err := f.store.WorkspaceByPrefix(t.Context(), "github", "acme")
	if err != nil {
		t.Fatal(err)
	}
	if !ws.GitHubAppBroken {
		t.Error("workspace not marked broken after revoked installation")
	}
}

// One accept per (run, attempt, part): the duplicate is refused and the
// first report stays.
func TestTokenlessUploadDuplicate(t *testing.T) {
	f, _, _ := newTokenlessFixture(t)
	if rec := doUpload(t, f, "", tokenlessFields(), testProfile); rec.Code != http.StatusCreated {
		t.Fatalf("first upload: status = %d, body = %s", rec.Code, rec.Body)
	}
	rec := doUpload(t, f, "", tokenlessFields(), testProfile)
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate: status = %d, body = %s", rec.Code, rec.Body)
	}
	if ups, _ := f.store.ListUploads(t.Context(), f.repo.ID, 0); len(ups) != 1 {
		t.Errorf("got %d stored uploads, want 1 (duplicate must not land)", len(ups))
	}

	// A different part of the same run is a matrix job, not a replay.
	fields := tokenlessFields()
	fields["part"] = "frontend"
	if rec := doUpload(t, f, "", fields, testProfile); rec.Code != http.StatusCreated {
		t.Errorf("second part: status = %d, body = %s", rec.Code, rec.Body)
	}
	// A rerun (next attempt) may upload again.
	fields = tokenlessFields()
	fields["run_attempt"] = "3"
	if rec := doUpload(t, f, "", fields, testProfile); rec.Code != http.StatusCreated {
		t.Errorf("next attempt: status = %d, body = %s", rec.Code, rec.Body)
	}
}

func TestTokenlessUploadUnknownRepo(t *testing.T) {
	f, _, _ := newTokenlessFixture(t)
	fields := tokenlessFields()
	fields["repo"] = "acme/unknown"
	rec := doUpload(t, f, "", fields, testProfile)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
}

func TestTokenlessUploadFieldValidation(t *testing.T) {
	for _, drop := range []string{"head_repo", "pr_id", "commit", "repo"} {
		f, _, _ := newTokenlessFixture(t)
		fields := tokenlessFields()
		delete(fields, drop)
		rec := doUpload(t, f, "", fields, testProfile)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("missing %s: status = %d, want 400 (body %s)", drop, rec.Code, rec.Body)
		}
	}
}

// Without a token and without a run claim the answer stays the historical
// 401 — the tokenless path is only entered by requests that ask for it.
func TestUploadWithoutTokenOrClaim(t *testing.T) {
	f, app, _ := newTokenlessFixture(t)
	rec := doUpload(t, f, "", map[string]string{"repo": "acme/widgets", "commit": "abc123"}, testProfile)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if len(app.verifyCalls) != 0 {
		t.Error("claim verification ran without a claim")
	}
}

// A server without a GitHub App cannot verify anything and says so.
func TestTokenlessUploadWithoutApp(t *testing.T) {
	f := newFixture(t, nil) // bitbucket fixture: no GitHubApp configured
	rec := doUpload(t, f, "", tokenlessFields(), testProfile)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "no GitHub App") {
		t.Errorf("reason not surfaced: %s", rec.Body)
	}
}

func TestTokenlessLimiter(t *testing.T) {
	l := newTokenlessLimiter()
	now := time.Now()
	for i := range maxTokenlessPerRepoHour {
		if !l.allow("acme/widgets", now) {
			t.Fatalf("attempt %d refused inside the limit", i)
		}
	}
	if l.allow("acme/widgets", now) {
		t.Error("attempt over the limit allowed")
	}
	if !l.allow("acme/other", now) {
		t.Error("another repo throttled by its neighbor")
	}
	if !l.allow("acme/widgets", now.Add(time.Hour)) {
		t.Error("attempt refused after the window reset")
	}
}

// A PR whose head branch shares the default branch's name must not take
// over the repo badge (or the delta baseline) — with tokenless uploads
// that series would otherwise be writable by any fork.
func TestBadgeIgnoresPRReportsOnDefaultBranch(t *testing.T) {
	f, _, _ := newTokenlessFixture(t)
	fields := tokenlessFields()
	fields["branch"] = "main" // a fork's main, PR #42
	if rec := doUpload(t, f, "", fields, testProfile); rec.Code != http.StatusCreated {
		t.Fatalf("upload: status = %d, body = %s", rec.Code, rec.Body)
	}
	rec := get(f, "/badge/github/acme/widgets.svg")
	if rec.Code != http.StatusOK {
		t.Fatalf("badge: status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "unknown") {
		t.Errorf("badge shows PR coverage: %s", rec.Body)
	}

	// The branch's own upload still drives the badge.
	if rec := doUpload(t, f, "secret-token", map[string]string{"repo": "acme/widgets", "commit": "def456", "branch": "main"}, testProfile); rec.Code != http.StatusCreated {
		t.Fatalf("branch upload: status = %d, body = %s", rec.Code, rec.Body)
	}
	rec = get(f, "/badge/github/acme/widgets.svg")
	if !strings.Contains(rec.Body.String(), "80.0%") {
		t.Errorf("badge = %s, want 80.0%%", rec.Body)
	}
}
