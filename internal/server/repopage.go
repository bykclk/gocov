// The repo page (GET /repos/{slug...}): one repo's current verdict, its
// coverage trend, the files behind its latest upload, and a paged list of
// the uploads behind it.

package server

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/gocov/gocov/internal/core"
	"github.com/gocov/gocov/internal/store"
)

// handleRepo implements GET /repos/{forge}/{workspace}/{repo} — stats,
// badge embed, branch filter and the upload list.
func (s *Server) handleRepo(w http.ResponseWriter, r *http.Request) {
	repo, err := s.store.RepoBySlug(r.Context(), r.PathValue("forge"), r.PathValue("slug"))
	if errors.Is(err, store.ErrNotFound) {
		s.reportNotFound(w, r)
		return
	}
	if err != nil {
		s.internalError(w, "loading repo", err)
		return
	}
	member, ok := s.authorizeReport(w, r, repo)
	if !ok {
		return
	}

	branch := r.FormValue("branch")
	page, _ := strconv.Atoi(r.FormValue("page"))
	if page < 0 {
		page = 0
	}

	// Fetch one page beyond the current one so "Older" knows whether to
	// render; the recent list also feeds the branch selector.
	recent, err := s.store.ListUploads(r.Context(), repo.ID, recentUploads)
	if err != nil {
		s.internalError(w, "listing uploads", err)
		return
	}
	seen := map[string]bool{}
	for _, u := range recent {
		seen[u.Branch] = true
	}
	branches := slices.Sorted(maps.Keys(seen))

	limit := (page+1)*uploadsPageSize + 1
	var fetched []*store.Upload
	if branch == "" {
		// Reuse the branch-selector fetch only while it also covers the
		// sentinel row; at limit == recentUploads+1 it is one row short of
		// deciding "Older" and would hide the link with pages still to come.
		if limit <= recentUploads {
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

	// The trend follows the page's branch filter, defaulting to the
	// repo's default branch when "All branches" is selected.
	trendBranch := cmp.Or(branch, repo.DefaultBranch)
	trendReports, err := s.store.ListBranchCommitReports(r.Context(), repo.ID, trendBranch, trendReportLimit)
	if err != nil {
		s.internalError(w, "listing reports for trend", err)
		return
	}

	// The verdict, stats and files view all describe the
	// selected branch's current standing (the default branch when "All
	// branches" is chosen); they ride inside the branch-filtered region so the
	// selector moves them together with the trend and history.
	var latest *store.CommitReport
	if l, err := s.store.LatestCommitReport(r.Context(), repo.ID, trendBranch); err == nil {
		latest = l
	} else if !errors.Is(err, store.ErrNotFound) {
		s.internalError(w, "loading latest report", err)
		return
	}

	var (
		verdict   *repoVerdictView
		lastProv  *provView
		uncovered int64
		filesView *filesViewData
	)
	if latest != nil {
		_, base := s.branchBaseReport(r.Context(), repo.ID, trendBranch)
		verdict = s.repoVerdict(latest, repo, base)
		uncovered = latest.TotalStmts - latest.CoveredStmts
		if lu, err := s.store.Upload(r.Context(), latest.UploadID); err == nil {
			p := s.uploadProvenance(r.Context(), lu)
			lastProv = &p
			baseUpload, baseFiles := s.baselineUpload(r.Context(), repo, lu)
			if fv, err := s.buildFilesViewData(r.Context(), lu, baseUpload, baseFiles, "Files on "+trendBranch); err == nil {
				filesView = &fv
			} else {
				s.log.Warn("loading files for repo page", "upload", lu.ID, "err", err)
			}
		}
	}

	// The settings link is for members of a tracked workspace; anyone
	// admitted through the public branch — anonymous or a signed-in
	// non-member — gets neither the button nor the workspace lookup
	// behind it.
	settings := member && s.forges.WorkspaceFor(r.Context(), repo.Slug, repo.Forge) != nil

	s.render(w, r, "repo.html", map[string]any{
		"Repo":          repo,
		"Latest":        latest,
		"Verdict":       verdict,
		"Uncovered":     uncovered,
		"LastUpload":    lastProv,
		"FilesView":     filesView,
		"Settings":      settings,
		"PublicView":    s.publicView(r),
		"BadgeMarkdown": s.badgeMarkdown(repo),
		"GateSummary":   gateSummary(repo.Gate),
		"Branches":      branches,
		"Branch":        branch,
		"TrendBranch":   trendBranch,
		"Trend":         newTrendView(trendBranch, trendReports, repo.Gate.MinCoverage),
		"Uploads":       uploads,
		"Page":          page,
		"PrevPage":      page - 1,
		"NextPage":      page + 1,
		"HasOlder":      hasOlder,
		"BaseURL":       strings.TrimSuffix(s.baseURL, "/"),
	})
}

const (
	uploadsPageSize = 10
	// recentUploads bounds the newest-uploads fetch that fills the branch
	// selector, and doubles as the first pages' history without a second query.
	recentUploads = 100
)

// reportBaseline pairs a branch's newest merged report (reports come newest
// first) with the report it should be compared against — the most recent
// gate-passing report before it, the same baseline rule the upload API uses,
// so the UI never shows a delta measured against a report that failed the
// gate. base is nil when the branch has no earlier passing report (a single
// report, or a run of failures fills the window).
func reportBaseline(reports []*store.CommitReport) (current, base *store.CommitReport) {
	if len(reports) == 0 {
		return nil, nil
	}
	if i := slices.IndexFunc(reports[1:], func(cr *store.CommitReport) bool { return !cr.GateFailed }); i >= 0 {
		base = reports[1+i]
	}
	return reports[0], base
}

// branchBaseReport reads a branch's recent reports and pairs the newest with
// its baseline. Lookback is bounded; a branch whose last 50 reports all
// failed shows no delta.
func (s *Server) branchBaseReport(ctx context.Context, repoID int64, branch string) (current, base *store.CommitReport) {
	reports, err := s.store.ListBranchCommitReports(ctx, repoID, branch, 50)
	if err != nil {
		return nil, nil
	}
	return reportBaseline(reports)
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

// repoVerdictView is the coverage verdict at the top of the repo page: the
// default branch's current standing against its gate, stated once. It mirrors
// the upload page's verdict but reads a merged commit report.
type repoVerdictView struct {
	State      string // "pass", "fail" or "neutral" (no gate configured)
	Pct        float64
	CovClass   string
	Delta      *deltaView // total coverage vs the branch baseline
	Reason     string     // prose walk-through of the gate rules and their outcome
	CommitID   int64      // upload id of the latest commit, for the detail link
	CommitSHA  string
	CommitAgo  string
	Branch     string
	IsDefault  bool
	PRID       string
	BaseID     int64
	BaseSHA    string
	BasePctStr string
}

// repoVerdict assembles the verdict card from the branch's newest merged
// report and the report it is compared against (nil when there is none).
func (s *Server) repoVerdict(latest *store.CommitReport, repo *store.Repo, base *store.CommitReport) *repoVerdictView {
	v := &repoVerdictView{
		Pct:       latest.TotalPct,
		CovClass:  covClass(latest.TotalPct),
		CommitID:  latest.UploadID,
		CommitSHA: latest.CommitSHA,
		CommitAgo: timeAgo(latest.CreatedAt),
		Branch:    latest.Branch,
		IsDefault: latest.Branch == repo.DefaultBranch,
		PRID:      latest.PRID,
	}
	var baseTotal float64
	if base != nil {
		v.Delta = newDeltaView(latest.TotalPct - base.TotalPct)
		v.BaseID = base.UploadID
		v.BaseSHA = base.CommitSHA
		v.BasePctStr = fmt.Sprintf("%.1f%%", base.TotalPct)
		baseTotal = base.TotalPct
	}
	switch {
	case !repo.Gate.Configured():
		v.State = "neutral"
	case latest.GateFailed:
		v.State = "fail"
	default:
		v.State = "pass"
	}
	v.Reason = core.GateReason(latest.TotalPct, latest.DiffCoverage, repo.Gate, baseTotal, base != nil, "The latest commit")
	return v
}
