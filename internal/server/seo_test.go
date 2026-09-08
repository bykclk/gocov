package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/gocov/gocov/internal/store"
)

func TestRobotsKeepsPrivateSurfacesOutOfIndexes(t *testing.T) {
	f := newPublicFixture(t, store.VisibilityPublic, true)

	rec := get(f, "/robots.txt")
	if rec.Code != http.StatusOK {
		t.Fatalf("robots.txt: status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, line := range []string{
		"Disallow: /login",
		"Disallow: /oauth/",
		"Disallow: /uploads/*/profile",
		"Sitemap: https://gocov.example/sitemap.xml",
	} {
		if !strings.Contains(body, line) {
			t.Errorf("robots.txt misses %q:\n%s", line, body)
		}
	}

	// With the instance switch off there is no sitemap to point at.
	off := newPublicFixture(t, store.VisibilityPublic, false)
	if body := get(off, "/robots.txt").Body.String(); strings.Contains(body, "Sitemap:") {
		t.Errorf("robots.txt references a sitemap although public reports are off:\n%s", body)
	}
}

func TestSitemapListsOnlyPublicRepoPages(t *testing.T) {
	f := newPublicFixture(t, store.VisibilityPublic, true)
	ctx := t.Context()
	for _, r := range []*store.Repo{
		{Forge: "bitbucket", Slug: "acme/private", Token: "t2", DefaultBranch: "main", Visibility: store.VisibilityPrivate},
		{Forge: "bitbucket", Slug: "acme/optedout", Token: "t3", DefaultBranch: "main",
			Visibility: store.VisibilityPublic, PublicReportsDisabled: true},
	} {
		if err := f.store.CreateRepo(ctx, r); err != nil {
			t.Fatal(err)
		}
	}

	rec := get(f, "/sitemap.xml")
	if rec.Code != http.StatusOK {
		t.Fatalf("sitemap.xml: status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "https://gocov.example/repos/bitbucket/acme/widgets") {
		t.Errorf("sitemap misses the public repo:\n%s", body)
	}
	if strings.Contains(body, "acme/private") || strings.Contains(body, "acme/optedout") {
		t.Errorf("sitemap lists a non-public repo:\n%s", body)
	}
}

func TestSitemapGoneWhenPublicReportsOff(t *testing.T) {
	f := newPublicFixture(t, store.VisibilityPublic, false)
	if rec := get(f, "/sitemap.xml"); rec.Code != http.StatusNotFound {
		t.Errorf("sitemap.xml with public reports off: status = %d, want 404", rec.Code)
	}
}

func TestRepoPageCarriesSEOTags(t *testing.T) {
	f := newPublicFixture(t, store.VisibilityPublic, true)

	body := get(f, "/repos/bitbucket/acme/widgets").Body.String()
	if !strings.Contains(body, "<title>acme/widgets code coverage — gocov</title>") {
		t.Errorf("repo page misses the descriptive title:\n%.400s", body)
	}
	if !strings.Contains(body, `<link rel="canonical" href="https://gocov.example/repos/bitbucket/acme/widgets">`) {
		t.Error("repo page misses the canonical link")
	}
	if !strings.Contains(body, `<meta name="description"`) {
		t.Error("repo page misses the meta description")
	}
}
