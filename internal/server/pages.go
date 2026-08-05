package server

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/bykclk/gocov/internal/profile"
	"github.com/bykclk/gocov/internal/store"
)

const uploadsPageSize = 25

// deltaView is a precomputed coverage delta for the templates.
type deltaView struct {
	Class string // up, down, flat
	Arrow string
	Text  string
}

func newDeltaView(d float64) *deltaView {
	switch {
	case d >= 0.05:
		return &deltaView{"up", "▲", fmt.Sprintf("%+.1f%%", d)}
	case d <= -0.05:
		return &deltaView{"down", "▼", fmt.Sprintf("%+.1f%%", d)}
	default:
		return &deltaView{"flat", "—", "0.0%"}
	}
}

// branchDelta compares the newest upload on a branch against the most
// recent gate-passing upload before it — the same baseline rule the
// upload API uses, so the UI never shows a delta measured against an
// upload that failed the gate. Lookback is bounded; a branch whose last
// 50 uploads all failed shows no delta.
func (s *Server) branchDelta(r *http.Request, repoID int64, branch string) *deltaView {
	ups, err := s.store.ListBranchUploads(r.Context(), repoID, branch, 50)
	if err != nil || len(ups) < 2 {
		return nil
	}
	current := ups[0]
	for _, u := range ups[1:] {
		if !u.GateFailed {
			return newDeltaView(current.TotalPct - u.TotalPct)
		}
	}
	return nil
}

type indexRow struct {
	Repo   *store.Repo
	Latest *store.Upload // nil when the default branch has no uploads
	Delta  *deltaView
	Gate   string // "pass", "fail" or ""
}

// handleIndex implements GET / — the repo dashboard with search.
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.FormValue("q"))
	repos, err := s.store.ListRepos(r.Context())
	if err != nil {
		s.internalError(w, "listing repos", err)
		return
	}
	rows := make([]indexRow, 0, len(repos))
	for _, repo := range repos {
		if query != "" && !strings.Contains(strings.ToLower(repo.Slug), strings.ToLower(query)) {
			continue
		}
		row := indexRow{Repo: repo}
		latest, err := s.store.LatestUpload(r.Context(), repo.ID, repo.DefaultBranch)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			s.internalError(w, "loading latest upload", err)
			return
		}
		if latest != nil {
			row.Latest = latest
			row.Delta = s.branchDelta(r, repo.ID, repo.DefaultBranch)
			if repo.Gate.Configured() {
				row.Gate = "pass"
				if latest.GateFailed {
					row.Gate = "fail"
				}
			}
		}
		rows = append(rows, row)
	}
	s.render(w, r, "index.html", map[string]any{"Rows": rows, "Query": query})
}

// gateSummary renders the repo's gate rules for the stats card.
func gateSummary(g store.Gate) string {
	var parts []string
	if g.MinCoverage != nil {
		parts = append(parts, fmt.Sprintf("total ≥ %.4g%%", *g.MinCoverage))
	}
	if g.MinDiffCoverage != nil {
		parts = append(parts, fmt.Sprintf("diff ≥ %.4g%%", *g.MinDiffCoverage))
	}
	if g.MaxCoverageDrop != nil {
		parts = append(parts, fmt.Sprintf("drop ≤ %.4g%%", *g.MaxCoverageDrop))
	}
	return strings.Join(parts, ", ")
}

// handleRepo implements GET /repos/{workspace}/{repo} — stats, badge embed,
// branch filter and the upload list.
func (s *Server) handleRepo(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	repo, err := s.store.RepoBySlug(r.Context(), slug)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.internalError(w, "loading repo", err)
		return
	}

	branch := r.FormValue("branch")
	page, _ := strconv.Atoi(r.FormValue("page"))
	if page < 0 {
		page = 0
	}

	// Fetch one page beyond the current one so "Older" knows whether to
	// render; the recent list also feeds the branch selector.
	recent, err := s.store.ListUploads(r.Context(), repo.ID, 100)
	if err != nil {
		s.internalError(w, "listing uploads", err)
		return
	}
	var branches []string
	seen := map[string]bool{}
	for _, u := range recent {
		if !seen[u.Branch] {
			seen[u.Branch] = true
			branches = append(branches, u.Branch)
		}
	}
	sort.Strings(branches)

	limit := (page+1)*uploadsPageSize + 1
	var fetched []*store.Upload
	if branch == "" {
		if limit <= 101 {
			fetched = recent
		} else if fetched, err = s.store.ListUploads(r.Context(), repo.ID, limit); err != nil {
			s.internalError(w, "listing uploads", err)
			return
		}
	} else if fetched, err = s.store.ListBranchUploads(r.Context(), repo.ID, branch, limit); err != nil {
		s.internalError(w, "listing branch uploads", err)
		return
	}
	start := min(page*uploadsPageSize, len(fetched))
	end := min(start+uploadsPageSize, len(fetched))
	uploads := fetched[start:end]
	hasOlder := len(fetched) > (page+1)*uploadsPageSize

	var latest *store.Upload
	if l, err := s.store.LatestUpload(r.Context(), repo.ID, repo.DefaultBranch); err == nil {
		latest = l
	} else if !errors.Is(err, store.ErrNotFound) {
		s.internalError(w, "loading latest upload", err)
		return
	}
	gate := ""
	if repo.Gate.Configured() && latest != nil {
		gate = "pass"
		if latest.GateFailed {
			gate = "fail"
		}
	}

	s.render(w, r, "repo.html", map[string]any{
		"Repo":        repo,
		"Latest":      latest,
		"Delta":       s.branchDelta(r, repo.ID, repo.DefaultBranch),
		"Gate":        gate,
		"GateSummary": gateSummary(repo.Gate),
		"Branches":    branches,
		"Branch":      branch,
		"Uploads":     uploads,
		"Page":        page,
		"PrevPage":    page - 1,
		"NextPage":    page + 1,
		"HasOlder":    hasOlder,
		"BaseURL":     strings.TrimSuffix(s.baseURL, "/"),
	})
}

// uploadFileRow decorates a stored file with its uncovered line ranges.
type uploadFileRow struct {
	*store.UploadFile
	Uncovered string
}

// maxUncoveredRanges caps the ranges shown per file in the table.
const maxUncoveredRanges = 6

// uncoveredRanges formats the line ranges of never-executed blocks,
// e.g. "45-52, 88 +3 more".
func uncoveredRanges(blocks []profile.Block) string {
	type span struct{ start, end int }
	var spans []span
	for _, b := range blocks {
		if b.Count > 0 || b.NumStmts == 0 {
			continue
		}
		spans = append(spans, span{b.StartLine, b.EndLine})
	}
	if len(spans) == 0 {
		return ""
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
	merged := spans[:1]
	for _, sp := range spans[1:] {
		last := &merged[len(merged)-1]
		if sp.start <= last.end+1 {
			if sp.end > last.end {
				last.end = sp.end
			}
			continue
		}
		merged = append(merged, sp)
	}

	var parts []string
	for i, sp := range merged {
		if i == maxUncoveredRanges {
			parts = append(parts, fmt.Sprintf("+%d more", len(merged)-maxUncoveredRanges))
			break
		}
		if sp.start == sp.end {
			parts = append(parts, strconv.Itoa(sp.start))
		} else {
			parts = append(parts, fmt.Sprintf("%d-%d", sp.start, sp.end))
		}
	}
	return strings.Join(parts, ", ")
}

// handleUploadPage implements GET /uploads/{id} — summary stats, diff
// coverage and the per-file table.
func (s *Server) handleUploadPage(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	upload, err := s.store.Upload(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.internalError(w, "loading upload", err)
		return
	}
	files, err := s.store.UploadFiles(r.Context(), id)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		s.internalError(w, "loading upload files", err)
		return
	}
	repo, err := s.store.RepoByID(r.Context(), upload.RepoID)
	if err != nil {
		s.internalError(w, "loading repo for upload", err)
		return
	}
	rows := make([]uploadFileRow, 0, len(files))
	for _, f := range files {
		rows = append(rows, uploadFileRow{UploadFile: f, Uncovered: uncoveredRanges(f.Blocks)})
	}
	s.render(w, r, "upload.html", map[string]any{
		"Upload":         upload,
		"Files":          rows,
		"Repo":           repo,
		"GateConfigured": repo.Gate.Configured(),
	})
}
