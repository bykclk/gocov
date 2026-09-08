package server

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/gocov/gocov/internal/store"
)

// The dashboard (GET /) is scoped to one workspace at a time. A workspace is a
// group of repos sharing a slug prefix; a tracked store.Workspace row enriches
// the group with reporting state and a settings page, but an untracked prefix
// (open instances, or repos whose workspace was never registered) still gets a
// group so its repos remain visible. The switcher moves between groups via
// ?ws=<forge>/<prefix>.

const dashStaleAfter = 14 * 24 * time.Hour

// dashboardView is the whole page: the selected workspace, the switcher over
// all groups the viewer can pick, the selected group's repos, and the rollups
// (stats + needs-attention) derived from them.
type dashboardView struct {
	Current   *wsGroup
	Switcher  []*wsGroup
	Repos     []*dashRepo
	Stats     dashStats
	Attention []attnItem
	Counts    filterCounts
}

// wsGroup is one workspace in the switcher — its identity, a repo count, and a
// weighted-coverage rollup so the picker previews each workspace's health.
type wsGroup struct {
	Prefix    string
	Forge     string
	Workspace *store.Workspace
	Initial   string // avatar letter
	ForgeCls  string // gh / bb / gl — avatar colour
	ForgeName string // GitHub / Bitbucket / GitLab
	RepoCount int
	HasCov    bool
	Pct       float64
	Current   bool
	Href      string

	repos []*store.Repo // repos bucketed into this group (assembly-only)
}

// dashRepo is one row of the repositories table. The *Sort fields feed the
// client-side sort control as plain data attributes.
type dashRepo struct {
	Repo    *store.Repo
	Name    string // slug with the workspace prefix trimmed
	Latest  *store.CommitReport
	Delta   *deltaView
	Gate    string // pass / fail / ""
	State   string // space-joined flags for data-state (failing/stale/nogate/ok)
	Stale   bool
	Spark   *sparkView
	LastAgo string

	HasReport bool
	CovVal    float64
	HasDelta  bool
	DropVal   float64
	UpUnix    int64
}

// sparkView is a tiny inline coverage sparkline, plotted in a 76x22 viewBox.
type sparkView struct {
	Path  string
	Class string // up / down / "" (flat)
	Tail  string // dashed continuation, drawn for stale repos whose series stopped
}

// dashStats is the three-up rollup above the tables.
type dashStats struct {
	HasCoverage    bool
	CoveragePct    float64
	GatesPassing   int
	GatesTotal     int
	StaleCount     int
	Reporting      string // "Connected", "Not connected", "Reconnect needed", "Not available"
	ReportingSub   string // "gocov[bot]", the granting account, or ""
	ReportingState string // on / off / broken / "" (styling)
}

// attnItem is one entry in the Needs-attention list.
type attnItem struct {
	Kind   string // bad / warn / info — icon colour
	Icon   string // ✗ / ! / ?
	Pre    string // text before the repo name
	Repo   string // repo name, rendered monospace
	Post   string // text after the repo name
	Msg    string
	Action string
	Href   string
}

type filterCounts struct{ All, Failing, Stale, NoGate int }

// handleIndex implements GET / — the workspace-scoped repo dashboard: a
// switcher over the viewer's workspaces, a stat rollup, the needs-attention
// list and the repositories table. See dashboard.go for the assembly.
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	scope, err := s.userScope(r)
	if err != nil {
		s.internalError(w, "scoping repos", err)
		return
	}
	// A hosted user without a single workspace membership would see a
	// permanently empty dashboard; registration is the only useful page
	// for them (M3/R1).
	if s.hosted && scope.scoped && len(scope.prefixes) == 0 && currentUser(r) != nil {
		http.Redirect(w, r, "/onboarding", http.StatusFound)
		return
	}
	dash, err := s.buildDashboard(r, strings.TrimSpace(r.FormValue("ws")))
	if err != nil {
		s.internalError(w, "building dashboard", err)
		return
	}
	s.render(w, r, "index.html", map[string]any{
		"Dash": dash,
		// A signed-in user can register a workspace from the onboarding
		// wizard (hosted and private mode alike); an open instance has no
		// identity to register from and points at sign-in instead.
		"CanOnboard": currentUser(r) != nil,
	})
}

// buildDashboard assembles the dashboard for the given ?ws selection. Returns a
// nil view when the viewer has no repos and no tracked workspaces at all (the
// template then shows the empty state).
func (s *Server) buildDashboard(r *http.Request, selected string) (*dashboardView, error) {
	ctx := r.Context()
	scope, err := s.userScope(r)
	if err != nil {
		return nil, err
	}
	repos, err := s.store.ListRepos(ctx)
	if err != nil {
		return nil, err
	}

	// Seed groups from the viewer's tracked workspaces so a workspace with no
	// uploads yet still appears in the switcher; then bucket every visible repo,
	// creating an untracked group for any prefix that has no workspace row.
	tracked, err := s.viewerWorkspaces(r)
	if err != nil {
		return nil, err
	}
	// Groups are keyed by wsKey: the GitHub org and the GitLab group of
	// one name are two workspaces, and the switcher lists both.
	groups := map[wsKey]*wsGroup{}
	order := []wsKey{}
	add := func(prefix, forge string, ws *store.Workspace) *wsGroup {
		key := wsKey{forge, prefix}
		g := groups[key]
		if g == nil {
			g = &wsGroup{Prefix: prefix, Forge: forge, Workspace: ws}
			groups[key] = g
			order = append(order, key)
		}
		if ws != nil && g.Workspace == nil {
			g.Workspace = ws
		}
		return g
	}
	for _, ws := range tracked {
		add(ws.Prefix, ws.Forge, ws)
	}
	for _, repo := range repos {
		if !scope.allows(repo) {
			continue
		}
		g := add(s.groupPrefix(repo, tracked), repo.Forge, nil)
		g.repos = append(g.repos, repo)
	}
	if len(order) == 0 {
		return nil, nil
	}
	slices.SortFunc(order, func(a, b wsKey) int {
		return cmp.Or(cmp.Compare(a.forge, b.forge), cmp.Compare(a.prefix, b.prefix))
	})

	// Resolve the selected group (?ws=forge/prefix); fall back to the
	// first when ?ws is missing or names a group the viewer cannot see.
	forge, prefix, _ := strings.Cut(selected, "/")
	cur := groups[wsKey{forge, prefix}]
	if cur == nil {
		cur = groups[order[0]]
	}

	dv := &dashboardView{Current: cur}
	for _, k := range order {
		g := groups[k]
		s.fillGroupMeta(ctx, g)
		g.Current = g == cur
		g.Href = "/?ws=" + url.QueryEscape(k.String()) // GitLab prefixes carry slashes too
		dv.Switcher = append(dv.Switcher, g)
	}

	s.fillCurrent(r, dv)
	return dv, nil
}

// viewerWorkspaces lists the workspaces the switcher may offer: the signed-in
// user's memberships, or every tracked workspace on an open instance.
func (s *Server) viewerWorkspaces(r *http.Request) ([]*store.Workspace, error) {
	if u := currentUser(r); u != nil {
		return s.store.ListWorkspacesForUser(r.Context(), u.ID)
	}
	if !s.authEnabled() {
		return s.store.ListWorkspaces(r.Context())
	}
	return nil, nil
}

// groupPrefix resolves the switcher group a repo belongs to: its most specific
// tracked workspace prefix, or the leading slug segment when none is tracked.
func (s *Server) groupPrefix(repo *store.Repo, tracked []*store.Workspace) string {
	if ws := owningWorkspace(repo, tracked); ws != nil {
		return ws.Prefix
	}
	if i := strings.IndexByte(repo.Slug, '/'); i >= 0 {
		return repo.Slug[:i]
	}
	return repo.Slug
}

// fillGroupMeta computes a group's switcher preview: repo count and weighted
// coverage over its repos' latest default-branch reports. This runs for every
// group, so it stays to one report lookup per repo.
func (s *Server) fillGroupMeta(ctx context.Context, g *wsGroup) {
	g.Initial, g.ForgeCls, g.ForgeName = forgeAvatar(g.Prefix, g.Forge)
	g.RepoCount = len(g.repos)
	var covered, total int64
	for _, repo := range g.repos {
		latest, err := s.store.LatestCommitReport(ctx, repo.ID, repo.DefaultBranch)
		if err != nil || latest == nil {
			continue
		}
		covered += latest.CoveredStmts
		total += latest.TotalStmts
	}
	if total > 0 {
		g.HasCov = true
		g.Pct = 100 * float64(covered) / float64(total)
	}
}

// fillCurrent builds the selected group's repo rows, the stat rollup and the
// needs-attention list. Repos in the selected group get the full treatment
// (delta + sparkline), reading each branch's recent reports once.
func (s *Server) fillCurrent(r *http.Request, dv *dashboardView) {
	ctx := r.Context()
	cur := dv.Current
	var covered, total int64
	for _, repo := range cur.repos {
		reports, err := s.store.ListBranchCommitReports(ctx, repo.ID, repo.DefaultBranch, trendReportLimit)
		if err != nil {
			continue
		}
		row := &dashRepo{
			Repo: repo,
			Name: strings.TrimPrefix(repo.Slug, cur.Prefix+"/"),
		}
		var latest *store.CommitReport
		if len(reports) > 0 {
			latest = reports[0]
			row.Latest = latest
			row.HasReport = true
			row.CovVal = latest.TotalPct
			row.UpUnix = latest.CreatedAt.Unix()
			row.LastAgo = timeAgo(latest.CreatedAt)
			covered += latest.CoveredStmts
			total += latest.TotalStmts
		}
		stale := latest != nil && time.Since(latest.CreatedAt) > dashStaleAfter
		row.Stale = stale
		if repo.Gate.Configured() && latest != nil {
			row.Gate = "pass"
			if latest.GateFailed {
				row.Gate = "fail"
			}
		}
		if _, base := reportBaseline(reports); base != nil {
			row.HasDelta = true
			row.DropVal = latest.TotalPct - base.TotalPct
			row.Delta = newDeltaView(row.DropVal)
		}
		row.Spark = newSparkView(reports, stale)

		// State flags drive both the filter tabs and the needs-attention list.
		var flags []string
		if row.Gate == "fail" {
			flags = append(flags, "failing")
			dv.Counts.Failing++
		}
		if stale {
			flags = append(flags, "stale")
			dv.Counts.Stale++
		}
		if !repo.Gate.Configured() {
			flags = append(flags, "nogate")
			dv.Counts.NoGate++
		}
		if len(flags) == 0 {
			flags = append(flags, "ok")
		}
		row.State = strings.Join(flags, " ")

		dv.Repos = append(dv.Repos, row)
		s.collectAttention(dv, repo, row, latest, stale)
	}
	dv.Counts.All = len(dv.Repos)

	// Default order: lowest coverage first (repos without a report sort last),
	// so the rows needing work lead. The client re-sorts on the sort control.
	slices.SortStableFunc(dv.Repos, func(a, b *dashRepo) int {
		if (a.Latest == nil) != (b.Latest == nil) {
			if a.Latest != nil {
				return -1
			}
			return 1
		}
		if a.Latest == nil {
			return cmp.Compare(a.Name, b.Name)
		}
		return cmp.Compare(a.Latest.TotalPct, b.Latest.TotalPct)
	})

	// Attention reads most-severe first: failing, then stale, then no-gate.
	slices.SortStableFunc(dv.Attention, func(a, b attnItem) int {
		return cmp.Compare(attnRank(a.Kind), attnRank(b.Kind))
	})

	dv.Stats = dashStats{StaleCount: dv.Counts.Stale}
	if total > 0 {
		dv.Stats.HasCoverage = true
		dv.Stats.CoveragePct = 100 * float64(covered) / float64(total)
	}
	for _, row := range dv.Repos {
		if row.Gate == "" {
			continue
		}
		dv.Stats.GatesTotal++
		if row.Gate == "pass" {
			dv.Stats.GatesPassing++
		}
	}
	s.fillReporting(&dv.Stats, cur)
}

// collectAttention appends the needs-attention entries a repo warrants: a
// failing gate, a stale feed, or a missing gate. Only repos that have uploaded
// raise stale/no-gate notices — a brand-new repo is not a problem.
func (s *Server) collectAttention(dv *dashboardView, repo *store.Repo, row *dashRepo, latest *store.CommitReport, stale bool) {
	if row.Gate == "fail" {
		msg := "Its latest report failed the coverage gate."
		if repo.Gate.MinCoverage != nil {
			msg = fmt.Sprintf("Coverage %.1f%%, below the %.4g%% minimum.", latest.TotalPct, *repo.Gate.MinCoverage)
		}
		dv.Attention = append(dv.Attention, attnItem{
			Kind: "bad", Icon: "✗", Repo: row.Name, Post: " is failing its coverage gate",
			Msg: msg, Action: "Open repo", Href: repoURL(repo),
		})
	}
	if stale {
		days := int(time.Since(latest.CreatedAt).Hours() / 24)
		dv.Attention = append(dv.Attention, attnItem{
			Kind: "warn", Icon: "!", Pre: "No uploads from ", Repo: row.Name,
			Post:   fmt.Sprintf(" in %d days", days),
			Msg:    "Its last pipeline run did not reach the upload step; the coverage shown is stale.",
			Action: "Open repo", Href: repoURL(repo),
		})
	}
	if !repo.Gate.Configured() && latest != nil {
		dv.Attention = append(dv.Attention, attnItem{
			Kind: "info", Icon: "?", Repo: row.Name, Post: " has no coverage gate",
			Msg:    "Uploads are recorded, but nothing blocks a drop.",
			Action: "Set a gate", Href: repoSettingsURL(repo, ""),
		})
	}
}

// fillReporting sets the Reporting stat from the current group's tracked
// workspace connection. Untracked groups (or deployments without a one-click
// mechanism) read as not connected.
func (s *Server) fillReporting(st *dashStats, g *wsGroup) {
	if g.Workspace == nil {
		st.Reporting, st.ReportingState = "Not connected", "off"
		return
	}
	ws := g.Workspace
	switch ws.Forge {
	case "github":
		switch {
		case ws.GitHubAppBroken:
			st.Reporting, st.ReportingState = "Reconnect needed", "broken"
		case ws.GitHubInstallationID != 0:
			st.Reporting, st.ReportingState, st.ReportingSub = "Connected", "on", "gocov[bot]"
		default:
			st.Reporting, st.ReportingState = "Not connected", "off"
		}
	case "bitbucket":
		switch {
		case ws.BitbucketGrantBroken:
			st.Reporting, st.ReportingState = "Reconnect needed", "broken"
		case ws.BitbucketGrantAccount != "":
			st.Reporting, st.ReportingState, st.ReportingSub = "Connected", "on", ws.BitbucketGrantAccount
		default:
			st.Reporting, st.ReportingState = "Not connected", "off"
		}
	case "gitlab":
		switch {
		case ws.GitLabGrantBroken:
			st.Reporting, st.ReportingState = "Reconnect needed", "broken"
		case ws.GitLabGrantAccount != "":
			st.Reporting, st.ReportingState, st.ReportingSub = "Connected", "on", ws.GitLabGrantAccount
		default:
			st.Reporting, st.ReportingState = "Not connected", "off"
		}
	default:
		st.Reporting, st.ReportingState = "Not connected", "off"
	}
}

func attnRank(kind string) int {
	switch kind {
	case "bad":
		return 0
	case "warn":
		return 1
	default:
		return 2
	}
}

// newSparkView plots a repo's recent coverage as a compact sparkline. It needs
// at least two branch commits (PR reports excluded); fewer returns nil and the
// cell shows a dash. A stale repo's line is compressed to the left with a
// dashed tail, signalling that the series simply stopped.
func newSparkView(reports []*store.CommitReport, stale bool) *sparkView {
	var series []float64 // chronological
	for _, report := range slices.Backward(reports) {
		if report.PRID == "" {
			series = append(series, report.TotalPct)
		}
	}
	if len(series) > 12 { // keep the last dozen points; older ones crowd the glyph
		series = series[len(series)-12:]
	}
	if len(series) < 2 {
		return nil
	}
	lo, hi := slices.Min(series), slices.Max(series)
	const top, bot = 3.0, 19.0 // vertical plot band within the 22px box
	right := 76.0
	if stale {
		right = 46.0 // leave room for the dashed "stopped" tail
	}
	y := func(v float64) float64 {
		if hi == lo {
			return (top + bot) / 2
		}
		return round1(bot - (v-lo)/(hi-lo)*(bot-top))
	}
	var path linePath
	for i, v := range series {
		path.lineTo(round1(right*float64(i)/float64(len(series)-1)), y(v))
	}
	sv := &sparkView{Path: path.String()}
	last := series[len(series)-1]
	switch {
	case last > series[0]+0.05:
		sv.Class = "up"
	case last < series[0]-0.05:
		sv.Class = "down"
	}
	if stale {
		ly := y(last)
		sv.Tail = fmt.Sprintf("M%g %g L76 %g", right, ly, ly)
		sv.Class = "" // a stopped series has no meaningful direction
	}
	return sv
}

// forgeAvatar returns the switcher avatar letter, colour class and forge label.
func forgeAvatar(prefix, forge string) (initial, cls, name string) {
	initial = "?"
	if rs := []rune(prefix); len(rs) > 0 {
		initial = strings.ToUpper(string(rs[0]))
	}
	switch forge {
	case "github":
		return initial, "gh", "GitHub"
	case "bitbucket":
		return initial, "bb", "Bitbucket"
	case "gitlab":
		return initial, "gl", "GitLab"
	default:
		return initial, "", ""
	}
}
