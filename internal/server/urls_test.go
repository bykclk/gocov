package server

import (
	"testing"

	"github.com/gocov/gocov/internal/store"
)

// The tenant URL builders pin the escaping split the live mux depends on:
// repo slugs ride bare as the trailing {slug...} wildcard, workspace
// prefixes are one escaped segment (a GitLab subgroup's slash becomes
// %2F). httptest preserves an unescaped slash where the live mux does
// not, so page tests alone cannot catch a builder that gets this wrong.
func TestTenantURLs(t *testing.T) {
	repo := &store.Repo{Forge: "gitlab", Slug: "grp/sub/proj"}
	for name, got := range map[string]string{
		"repo":            repoURL(repo),
		"repo path":       repoPath("github", "acme/widgets"),
		"badge":           badgeURL(repo),
		"settings":        repoSettingsURL(repo, ""),
		"settings action": repoSettingsURL(repo, "rotate-token"),
		"workspace":       workspaceURL(&store.Workspace{Forge: "gitlab", Prefix: "grp/sub"}, "/setup"),
		"workspace plain": workspaceURL(&store.Workspace{Forge: "bitbucket", Prefix: "acme"}, ""),
	} {
		want := map[string]string{
			"repo":            "/repos/gitlab/grp/sub/proj",
			"repo path":       "/repos/github/acme/widgets",
			"badge":           "/badge/gitlab/grp/sub/proj.svg",
			"settings":        "/repo-settings/gitlab/grp/sub/proj",
			"settings action": "/repo-settings/rotate-token/gitlab/grp/sub/proj",
			"workspace":       "/workspaces/gitlab/grp%2Fsub/setup",
			"workspace plain": "/workspaces/bitbucket/acme",
		}[name]
		if got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}
