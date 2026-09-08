package server

import (
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/gocov/gocov/internal/store"
)

func TestRepoSettingsAccess(t *testing.T) {
	f, sess := newWorkspaceFixture(t, true) // workspace acme + repo acme/widgets, member signed in

	rec := get(f, "/repo-settings/bitbucket/acme/widgets", sess)
	if rec.Code != http.StatusOK {
		t.Fatalf("member settings page: status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "secret-token") {
		t.Error("settings page should make the upload token available to members (Reveal)")
	}
	for _, want := range []string{"Coverage gates", "Base branch", "Remove repository"} {
		if !strings.Contains(body, want) {
			t.Errorf("settings page missing %q", want)
		}
	}

	// A repo whose workspace the user is no member of 404s, even though it exists.
	if err := f.store.CreateWorkspace(t.Context(),
		&store.Workspace{Forge: "bitbucket", Prefix: "beta", Token: "bt", DefaultBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	if err := f.store.CreateRepo(t.Context(),
		&store.Repo{Forge: "bitbucket", Slug: "beta/thing", Token: "x", DefaultBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	if rec := get(f, "/repo-settings/bitbucket/beta/thing", sess); rec.Code != http.StatusNotFound {
		t.Errorf("non-member settings page: status = %d, want 404", rec.Code)
	}
	// Anonymous is redirected to login by the auth middleware.
	if rec := get(f, "/repo-settings/bitbucket/acme/widgets"); rec.Code != http.StatusFound {
		t.Errorf("anonymous settings page: status = %d, want login redirect", rec.Code)
	}
}

func TestMemberRepoSettingsAreReadOnly(t *testing.T) {
	// Same split as the workspace page: a member reads, an owner changes.
	f, sess := newMemberFixture(t, true)
	ctx := t.Context()

	rec := get(f, "/repo-settings/bitbucket/acme/widgets", sess)
	if rec.Code != http.StatusOK {
		t.Fatalf("member settings page: status = %d", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "secret-token") {
		t.Error("member page carries the upload token")
	}
	for _, control := range []string{"Rotate token", "Remove this repository", `action="/repo-settings/save/bitbucket/acme/widgets"`} {
		if strings.Contains(body, control) {
			t.Errorf("member page renders the owner control %q", control)
		}
	}
	// What a member came for is still there: the values, and the badge.
	if !strings.Contains(body, "read-only") || !strings.Contains(body, "/badge/bitbucket/acme/widgets.svg") {
		t.Errorf("member page misses the read-only note or the badge:\n%s", body)
	}

	for _, path := range []string{
		"/repo-settings/save/bitbucket/acme/widgets",
		"/repo-settings/rotate-token/bitbucket/acme/widgets",
		"/repo-settings/delete/bitbucket/acme/widgets",
	} {
		if rec := postForm(f, path, url.Values{"default_branch": {"develop"}}, sess); rec.Code != http.StatusForbidden {
			t.Errorf("member POST %s: status = %d, want 403", path, rec.Code)
		}
	}
	repo, err := f.store.RepoBySlug(ctx, "bitbucket", "acme/widgets")
	if err != nil || repo.Token != "secret-token" || repo.DefaultBranch != "main" {
		t.Errorf("a member's refused POSTs changed the repo: %+v, %v", repo, err)
	}
}

func TestRepoSettingsSaveRotateDelete(t *testing.T) {
	f, sess := newWorkspaceFixture(t, true)
	ctx := t.Context()

	// Save base branch + a min-coverage gate.
	rec := postForm(f, "/repo-settings/save/bitbucket/acme/widgets", url.Values{
		"default_branch": {"develop"}, "min_coverage": {"85"},
	}, sess)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("save: status = %d, want 303", rec.Code)
	}
	repo, err := f.store.RepoBySlug(ctx, "bitbucket", "acme/widgets")
	if err != nil {
		t.Fatal(err)
	}
	if repo.DefaultBranch != "develop" {
		t.Errorf("base branch not saved: %q", repo.DefaultBranch)
	}
	if repo.Gate.MinCoverage == nil || *repo.Gate.MinCoverage != 85 {
		t.Errorf("gate not saved: %+v", repo.Gate)
	}

	// A bad gate value is rejected.
	if rec := postForm(f, "/repo-settings/save/bitbucket/acme/widgets", url.Values{
		"default_branch": {"main"}, "min_coverage": {"250"},
	}, sess); rec.Code != http.StatusBadRequest {
		t.Errorf("bad gate: status = %d, want 400", rec.Code)
	}

	// Rotate the token: a new one is issued and shown once.
	rec = postForm(f, "/repo-settings/rotate-token/bitbucket/acme/widgets", url.Values{}, sess)
	if rec.Code != http.StatusOK {
		t.Fatalf("rotate: status = %d", rec.Code)
	}
	repo, _ = f.store.RepoBySlug(ctx, "bitbucket", "acme/widgets")
	if repo.Token == "secret-token" {
		t.Error("token was not rotated")
	}
	if !strings.Contains(rec.Body.String(), repo.Token) {
		t.Error("rotated token not shown once in the response")
	}

	// Delete removes the repo and redirects to the workspace.
	rec = postForm(f, "/repo-settings/delete/bitbucket/acme/widgets", url.Values{}, sess)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("delete: status = %d, want 303", rec.Code)
	}
	if _, err := f.store.RepoBySlug(ctx, "bitbucket", "acme/widgets"); err == nil {
		t.Error("repo still present after delete")
	}
}

// The Ignored files card saves one pattern per line, shows them back, and
// refuses a pattern the matcher cannot compile.
func TestRepoSettingsSaveIgnorePaths(t *testing.T) {
	f, sess := newWorkspaceFixture(t, true)
	ctx := t.Context()

	rec := postForm(f, "/repo-settings/save/bitbucket/acme/widgets", url.Values{
		"default_branch": {"main"},
		"ignore_paths":   {"cmd/preview/**\r\n\r\n# generated\r\n*_mock.go\r\n"},
	}, sess)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("save: status = %d, body = %s", rec.Code, rec.Body)
	}
	repo, err := f.store.RepoBySlug(ctx, "bitbucket", "acme/widgets")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"cmd/preview/**", "*_mock.go"}; !slices.Equal(repo.IgnorePaths, want) {
		t.Errorf("ignore paths = %q, want %q", repo.IgnorePaths, want)
	}

	page := get(f, "/repo-settings/bitbucket/acme/widgets", sess)
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "cmd/preview/**\n*_mock.go") {
		t.Errorf("settings page (%d) does not show the saved patterns", page.Code)
	}

	// An uncompilable pattern is refused and nothing changes.
	if rec := postForm(f, "/repo-settings/save/bitbucket/acme/widgets", url.Values{
		"default_branch": {"main"}, "ignore_paths": {"src/["},
	}, sess); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "ignore pattern") {
		t.Errorf("bad pattern: status = %d", rec.Code)
	}
	if repo, _ = f.store.RepoBySlug(ctx, "bitbucket", "acme/widgets"); len(repo.IgnorePaths) != 2 {
		t.Errorf("bad save changed the patterns: %q", repo.IgnorePaths)
	}

	// Clearing the field clears the patterns.
	if rec := postForm(f, "/repo-settings/save/bitbucket/acme/widgets", url.Values{
		"default_branch": {"main"}, "ignore_paths": {""},
	}, sess); rec.Code != http.StatusSeeOther {
		t.Errorf("clear: status = %d", rec.Code)
	}
	if repo, _ = f.store.RepoBySlug(ctx, "bitbucket", "acme/widgets"); repo.IgnorePaths != nil {
		t.Errorf("patterns not cleared: %q", repo.IgnorePaths)
	}
}
