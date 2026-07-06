package server

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/bykclk/gocov/internal/store"
)

// Badge color thresholds: <50 red, 50–75 yellow, >75 green.
const (
	badgeRed    = "#e05d44"
	badgeYellow = "#dfb317"
	badgeGreen  = "#4c1"
	badgeGray   = "#9f9f9f"
)

func badgeColor(pct float64) string {
	switch {
	case pct < 50:
		return badgeRed
	case pct <= 75:
		return badgeYellow
	default:
		return badgeGreen
	}
}

// handleBadge implements GET /badge/{workspace}/{repo}.svg with the latest
// coverage on the repo's default branch.
func (s *Server) handleBadge(w http.ResponseWriter, r *http.Request) {
	slug, ok := strings.CutSuffix(r.PathValue("slug"), ".svg")
	if !ok || slug == "" {
		http.NotFound(w, r)
		return
	}

	repo, err := s.store.RepoBySlug(r.Context(), slug)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.internalError(w, "loading repo for badge", err)
		return
	}

	value, color := "unknown", badgeGray
	latest, err := s.store.LatestUpload(r.Context(), repo.ID, repo.DefaultBranch)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		s.internalError(w, "loading latest upload for badge", err)
		return
	}
	if latest != nil {
		value = fmt.Sprintf("%.1f%%", latest.TotalPct)
		color = badgeColor(latest.TotalPct)
	}

	w.Header().Set("Content-Type", "image/svg+xml; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, max-age=0")
	_, _ = w.Write([]byte(badgeSVG("coverage", value, color)))
}

// badgeSVG renders a flat two-segment badge, shields.io style. Text width is
// approximated from character count, which is fine for digits and short words.
func badgeSVG(label, value, color string) string {
	const charWidth, pad = 7, 10
	labelW := len(label)*charWidth + pad
	valueW := len(value)*charWidth + pad
	total := labelW + valueW
	return fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" width="%[1]d" height="20" role="img" aria-label="%[2]s: %[3]s">
<linearGradient id="s" x2="0" y2="100%%"><stop offset="0" stop-color="#bbb" stop-opacity=".1"/><stop offset="1" stop-opacity=".1"/></linearGradient>
<clipPath id="r"><rect width="%[1]d" height="20" rx="3" fill="#fff"/></clipPath>
<g clip-path="url(#r)">
<rect width="%[4]d" height="20" fill="#555"/>
<rect x="%[4]d" width="%[5]d" height="20" fill="%[6]s"/>
<rect width="%[1]d" height="20" fill="url(#s)"/>
</g>
<g fill="#fff" text-anchor="middle" font-family="Verdana,Geneva,DejaVu Sans,sans-serif" font-size="11">
<text x="%[7]d" y="14">%[2]s</text>
<text x="%[8]d" y="14">%[3]s</text>
</g>
</svg>`, total, label, value, labelW, valueW, color, labelW/2, labelW+valueW/2)
}
