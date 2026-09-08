// The crawler surface of public report pages: robots.txt keeps the login
// flow, the settings pages and raw profile downloads out of search
// indexes, and sitemap.xml lists every effectively public repo page.
// Both are sessionless (see publicPath).

package server

import (
	"encoding/xml"
	"net/http"
	"strings"
)

// handleRobots implements GET /robots.txt. The disallow list names the
// pages that exist but should never rank: the login and OAuth flows, the
// administrative pages (which 404 or redirect for crawlers anyway) and
// the raw profile downloads under otherwise indexable upload pages.
func (s *Server) handleRobots(w http.ResponseWriter, r *http.Request) {
	var sb strings.Builder
	sb.WriteString("User-agent: *\n")
	sb.WriteString("Disallow: /login\n")
	sb.WriteString("Disallow: /oauth/\n")
	sb.WriteString("Disallow: /repo-settings/\n")
	sb.WriteString("Disallow: /workspaces/\n")
	sb.WriteString("Disallow: /uploads/*/profile\n")
	if s.publicReports {
		sb.WriteString("\nSitemap: " + strings.TrimSuffix(s.baseURL, "/") + "/sitemap.xml\n")
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write([]byte(sb.String()))
}

// sitemapMaxEntries is the sitemap protocol's cap on URLs per file.
const sitemapMaxEntries = 50000

// sitemap is the minimal urlset shape of the sitemap protocol.
type sitemap struct {
	XMLName xml.Name     `xml:"urlset"`
	Xmlns   string       `xml:"xmlns,attr"`
	URLs    []sitemapURL `xml:"url"`
}

type sitemapURL struct {
	Loc string `xml:"loc"`
}

// handleSitemap implements GET /sitemap.xml: one entry per effectively
// public repo page, so every public repo is a page search engines can
// find. With public reports off the sitemap does not exist.
func (s *Server) handleSitemap(w http.ResponseWriter, r *http.Request) {
	if !s.publicReports {
		http.NotFound(w, r)
		return
	}
	refs, err := s.store.PublicRepoRefs(r.Context(), sitemapMaxEntries)
	if err != nil {
		s.internalError(w, "listing public repos for sitemap", err)
		return
	}
	base := strings.TrimSuffix(s.baseURL, "/")
	sm := sitemap{Xmlns: "http://www.sitemaps.org/schemas/sitemap/0.9"}
	for _, ref := range refs {
		sm.URLs = append(sm.URLs, sitemapURL{Loc: base + repoPath(ref.Forge, ref.Slug)})
	}
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write([]byte(xml.Header))
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	if err := enc.Encode(sm); err != nil {
		s.log.Error("encoding sitemap", "err", err)
	}
}
