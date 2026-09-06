// Command gocov-preview is a throwaway dev harness: it serves the web UI
// from an in-memory store seeded with a synthetic upload history, for
// eyeballing UI changes without Postgres. Not part of the product.
package main

import (
	"context"
	"fmt"
	"log"
	"math"
	"math/rand"
	"net/http"
	"net/url"
	"time"

	"github.com/gocov/gocov/internal/auth"
	blobmem "github.com/gocov/gocov/internal/blobstore/memory"
	"github.com/gocov/gocov/internal/config"
	"github.com/gocov/gocov/internal/forge"
	"github.com/gocov/gocov/internal/forge/bitbucket"
	forgefake "github.com/gocov/gocov/internal/forge/fake"
	"github.com/gocov/gocov/internal/forge/github"
	"github.com/gocov/gocov/internal/forge/gitlab"
	"github.com/gocov/gocov/internal/profile"
	"github.com/gocov/gocov/internal/server"
	"github.com/gocov/gocov/internal/store"
	storemem "github.com/gocov/gocov/internal/store/memory"
)

// devAuth is a sign-in provider that "authorizes" by bouncing straight
// back to the local callback, so the login, registration and workspace
// settings pages are previewable without a real OAuth consumer. Enable
// with GOCOV_PREVIEW_AUTH=1 (hosted mode; sign-in lands a member of the
// seeded acme workspace with the unregistered "personal" also on offer).
// The "github" instance signs in a member of the gh-* workspaces, which
// are seeded in the three GitHub App connection states. The dev user
// owns every workspace except the last of each list, so both the owner
// and the member view of the settings pages are previewable.
type devAuth struct {
	forge      string
	workspaces []string
}

func (a devAuth) Name() string { return a.forge }
func (a devAuth) AuthorizeURL(state, redirectURI string) string {
	return redirectURI + "?state=" + url.QueryEscape(state) + "&code=dev"
}
func (a devAuth) Identity(context.Context, string, string) (*auth.Identity, error) {
	return &auth.Identity{
		ForgeUUID: "{dev-" + a.forge + "}", DisplayName: "Dev User", Email: "dev@example.com",
		Workspaces:      a.workspaces,
		OwnedWorkspaces: a.workspaces[:len(a.workspaces)-1],
	}, nil
}

// devGitHubApp stubs server.GitHubApp so the settings/setup pages render
// the App cards; the connect flow itself needs no live GitHub either.
type devGitHubApp struct{ fg forge.Forge }

func (d devGitHubApp) ForgeClient(context.Context, int64) (forge.Forge, error) { return d.fg, nil }
func (devGitHubApp) InstallationAccount(context.Context, int64) (string, error) {
	return "gh-new", nil
}
func (devGitHubApp) InstallURL(context.Context) (string, error) {
	return "https://github.com/apps/gocov/installations/new", nil
}
func (devGitHubApp) VerifyRunClaim(context.Context, int64, github.RunClaim) error { return nil }

// devGLConnect stubs server.GitLabConnect the same way devBBConnect
// stubs Bitbucket: the consent bounce goes straight back to the local
// callback, so the whole connect loop is previewable without GitLab.
type devGLConnect struct{ fg forge.Forge }

func (devGLConnect) AuthorizeURL(state, redirectURI string) string {
	return redirectURI + "?state=" + url.QueryEscape(state) + "&code=dev"
}
func (devGLConnect) Exchange(context.Context, string, string) (*gitlab.Grant, error) {
	return &gitlab.Grant{Account: "gocov-bot", AccessToken: "at", RefreshToken: "rt", TTL: 2 * time.Hour}, nil
}
func (devGLConnect) Refresh(context.Context, string, string) (*gitlab.Grant, error) {
	return &gitlab.Grant{AccessToken: "at", RefreshToken: "rt", TTL: 2 * time.Hour}, nil
}
func (d devGLConnect) ForgeClient(string) forge.Forge { return d.fg }

// devBBConnect stubs server.BitbucketConnect: the consent bounce goes
// straight back to the local callback, so the whole connect loop is
// previewable without Bitbucket.
type devBBConnect struct{ fg forge.Forge }

func (devBBConnect) AuthorizeURL(state, redirectURI string) string {
	return redirectURI + "?state=" + url.QueryEscape(state) + "&code=dev"
}
func (devBBConnect) Exchange(context.Context, string, string) (*bitbucket.Grant, error) {
	return &bitbucket.Grant{Account: "gocov-bot", AccessToken: "at", RefreshToken: "rt", TTL: 2 * time.Hour}, nil
}
func (devBBConnect) Refresh(context.Context, string) (*bitbucket.Grant, error) {
	return &bitbucket.Grant{AccessToken: "at", RefreshToken: "rt", TTL: 2 * time.Hour}, nil
}
func (d devBBConnect) ForgeClient(string) forge.Forge { return d.fg }

func main() {
	ctx := context.Background()
	st := storemem.New()
	// Marked public so the anonymous read-only view (CTA band, hidden
	// settings) is previewable with GOCOV_PREVIEW_AUTH=1 in a second,
	// signed-out browser tab.
	repo := &store.Repo{
		Forge: "bitbucket", Slug: "acme/widgets", Token: "tok",
		DefaultBranch: "main", Gate: store.Gate{MinCoverage: new(float64(70))},
		IgnorePaths: []string{"**/*.pb.go", "cmd/preview/**"},
		Visibility:  store.VisibilityPublic,
	}
	if err := st.CreateRepo(ctx, repo); err != nil {
		log.Fatal(err)
	}
	if err := st.CreateWorkspace(ctx, &store.Workspace{
		Forge: "bitbucket", Prefix: "acme", Token: "ws-preview-token", DefaultBranch: "main",
	}); err != nil {
		log.Fatal(err)
	}

	// reportFor mirrors an upload into a single-part merged commit report, the
	// row the repo page reads for its verdict, trend and per-file breakdown.
	// The real upload pipeline builds these; the preview seeds uploads
	// directly, so it seeds the matching reports here.
	reportFor := func(u *store.Upload) *store.CommitReport {
		return &store.CommitReport{
			RepoID: u.RepoID, CommitSHA: u.CommitSHA, Branch: u.Branch, PRID: u.PRID,
			TotalPct: u.TotalPct, CoveredStmts: u.CoveredStmts, TotalStmts: u.TotalStmts,
			GateFailed: u.GateFailed, PartCount: 1, UploadID: u.ID,
			CreatedAt: u.CreatedAt, UpdatedAt: u.CreatedAt,
		}
	}

	// ~45 uploads drifting between ~68% and ~85%, a few gate failures,
	// a couple of PR uploads that must not appear in the trend.
	rnd := rand.New(rand.NewSource(42))
	base := time.Now().Add(-45 * 24 * time.Hour)
	pct := 74.0
	for i := range 45 {
		pct += rnd.Float64()*4 - 2 + 0.1*math.Sin(float64(i)/4)
		pct = max(66, min(88, pct))
		u := &store.Upload{
			RepoID:    repo.ID,
			CommitSHA: fmt.Sprintf("%040x", i),
			Branch:    "main",
			Format:    "go",
			TotalPct:  pct, CoveredStmts: int64(pct * 10), TotalStmts: 1000,
			CreatedAt: base.Add(time.Duration(i) * 24 * time.Hour),
		}
		if i%15 == 7 {
			u.PRID = "9"
			u.TotalPct = 20 // would be an obvious outlier if it leaked in
		}
		// Derive the gate result from the final total so the verdict card
		// and the coverage it shows never disagree.
		u.GateFailed = u.TotalPct < 70
		if err := st.CreateUpload(ctx, u, nil); err != nil {
			log.Fatal(err)
		}
		if err := st.UpsertCommitReport(ctx, reportFor(u)); err != nil {
			log.Fatal(err)
		}
	}

	// One upload carrying real per-file coverage and cached source, so the
	// source view (miss-map rail, jump-to-miss, uncovered-only filter) is
	// previewable. Its source is pre-seeded into the blobstore below.
	blobs := blobmem.New()
	// A prior baseline upload of the same file, covering three lines the
	// head commit later regressed — so the source view shows a coverage
	// delta and "newly uncovered" markers.
	baseUpload := &store.Upload{
		RepoID: repo.ID, CommitSHA: "3ab04c17e2f10000000000000000000000000000",
		Branch: "main", Format: "go",
		TotalPct: 84.0, CoveredStmts: 210, TotalStmts: 250,
		CreatedAt: base.Add(44 * 24 * time.Hour),
	}
	baseFiles := append([]*store.UploadFile{{
		Path: "internal/billing/charge.go", Pct: 49.0, CoveredStmts: 15, TotalStmts: 26,
		Blocks: chargeBaseBlocks(),
	}}, steadyFiles()...)
	if err := st.CreateUpload(ctx, baseUpload, baseFiles); err != nil {
		log.Fatal(err)
	}
	if err := st.UpsertCommitReport(ctx, reportFor(baseUpload)); err != nil {
		log.Fatal(err)
	}
	srcUpload := &store.Upload{
		RepoID: repo.ID, CommitSHA: "9f31c2ab7e5d0000000000000000000000000000",
		Branch: "main", Format: "go", Part: "unit",
		TotalPct: 82.0, CoveredStmts: 205, TotalStmts: 250,
		CreatedAt: base.Add(45 * 24 * time.Hour),
		Meta: store.UploadMeta{
			Uploader: "gocov v0.9.2", UploaderKind: "action",
			CIProvider: "github", CIRunURL: "https://github.com/acme/widgets/actions/runs/2481",
			CommitMessage: "Reconcile ledger entries before posting",
			CommitAuthor:  "devuser",
			ProfileName:   "coverage.out", ProfileBytes: 118 * 1024, ProcessMillis: 1800,
		},
	}
	file := &store.UploadFile{
		Path: "internal/billing/charge.go", Pct: 46.2, CoveredStmts: 12, TotalStmts: 26,
		Blocks: chargeBlocks(),
	}
	// charge.go is the only file this commit moved; the rest are unchanged
	// so the upload page tucks them behind "show all".
	srcFiles := append([]*store.UploadFile{file}, steadyFiles()...)
	if err := st.CreateUpload(ctx, srcUpload, srcFiles); err != nil {
		log.Fatal(err)
	}
	srcReport := reportFor(srcUpload)
	srcReport.PartCount = 2
	if err := st.UpsertCommitReport(ctx, srcReport); err != nil {
		log.Fatal(err)
	}
	// A second slice of the same commit, so the upload page shows a part chip
	// and "merged from 2 parts" instead of a single profile. The seeded merged
	// report above keeps the head commit's totals: the real pipeline sums the
	// parts, but here the report is written by hand and the rest of the
	// preview (trend, gate, dashboard) is calibrated to those numbers.
	intUpload := &store.Upload{
		RepoID: repo.ID, CommitSHA: srcUpload.CommitSHA,
		Branch: "main", Format: "go", Part: "integration",
		TotalPct: 71.4, CoveredStmts: 35, TotalStmts: 49,
		CreatedAt: srcUpload.CreatedAt.Add(90 * time.Second),
		Meta: store.UploadMeta{
			Uploader: "gocov v0.9.2", UploaderKind: "action",
			CIProvider: "github", CIRunURL: "https://github.com/acme/widgets/actions/runs/2481",
			CommitMessage: srcUpload.Meta.CommitMessage,
			CommitAuthor:  srcUpload.Meta.CommitAuthor,
			ProfileName:   "integration.out", ProfileBytes: 31 * 1024, ProcessMillis: 640,
		},
	}
	intFiles := []*store.UploadFile{
		{Path: "internal/api/routes.go", Pct: 82.1, CoveredStmts: 23, TotalStmts: 28,
			Blocks: []profile.Block{{StartLine: 1, EndLine: 8, NumStmts: 23, Count: 1}}},
		{Path: "internal/ledger/settle.go", Pct: 57.1, CoveredStmts: 12, TotalStmts: 21,
			Blocks: []profile.Block{{StartLine: 1, EndLine: 8, NumStmts: 12, Count: 1}}},
	}
	if err := st.CreateUpload(ctx, intUpload, intFiles); err != nil {
		log.Fatal(err)
	}

	// A tokenless fork-PR upload, so the "unverified contributor upload"
	// chip on the upload page is previewable.
	forkUpload := &store.Upload{
		RepoID: repo.ID, CommitSHA: "77aa11bb22cc0000000000000000000000000000",
		Branch: "fix-typo", PRID: "12", Format: "go",
		TotalPct: 81.0, CoveredStmts: 202, TotalStmts: 249,
		CreatedAt: srcUpload.CreatedAt.Add(4 * time.Hour),
		Meta: store.UploadMeta{
			Uploader: "gocov v0.9.2", UploaderKind: "action",
			CIProvider: "github", CIRunURL: "https://github.com/acme/widgets/actions/runs/2519",
			CommitMessage: "Fix off-by-one in ledger rounding",
			CommitAuthor:  "forkcontributor",
			ProfileName:   "coverage.out", ProfileBytes: 116 * 1024, ProcessMillis: 950,
			Tokenless: true,
		},
	}
	if err := st.CreateUpload(ctx, forkUpload, steadyFiles()); err != nil {
		log.Fatal(err)
	}
	if err := st.UpsertCommitReport(ctx, reportFor(forkUpload)); err != nil {
		log.Fatal(err)
	}

	blobKey := fmt.Sprintf("source/%d/%s/%s", repo.ID, srcUpload.CommitSHA, file.Path)
	if err := blobs.Put(ctx, blobKey, []byte(chargeSource)); err != nil {
		log.Fatal(err)
	}

	// GitHub and Bitbucket workspaces in the connection states One-Click
	// Connect adds, for the settings/setup page cards. The default acme
	// workspace stays unconnected — that is the Connect-button state.
	for _, ws := range []*store.Workspace{
		{Forge: "github", Prefix: "gh-new", Token: "gh-new-token", DefaultBranch: "main"},
		{Forge: "github", Prefix: "gh-connected", Token: "gh-conn-token", DefaultBranch: "main",
			GitHubInstallationID: 4242},
		{Forge: "github", Prefix: "gh-broken", Token: "gh-broken-token", DefaultBranch: "main",
			GitHubInstallationID: 4243, GitHubAppBroken: true},
		{Forge: "bitbucket", Prefix: "bb-connected", Token: "bb-conn-token", DefaultBranch: "main",
			BitbucketGrantAccount: "gocov-bot", BitbucketRefreshToken: "rt"},
		{Forge: "bitbucket", Prefix: "bb-broken", Token: "bb-broken-token", DefaultBranch: "main",
			BitbucketGrantAccount: "gocov-bot", BitbucketRefreshToken: "rt", BitbucketGrantBroken: true},
		// A GitLab workspace at subgroup depth, for the setup page's
		// .gitlab-ci.yml snippet and the %2F-encoded workspace routes,
		// plus the GitLab-connect states.
		{Forge: "gitlab", Prefix: "gl-group/platform", Token: "gl-token", DefaultBranch: "main"},
		{Forge: "gitlab", Prefix: "gl-connected", Token: "gl-conn-token", DefaultBranch: "main",
			GitLabGrantAccount: "gocov-bot", GitLabRefreshToken: "rt"},
		{Forge: "gitlab", Prefix: "gl-broken", Token: "gl-broken-token", DefaultBranch: "main",
			GitLabGrantAccount: "gocov-bot", GitLabRefreshToken: "rt", GitLabGrantBroken: true},
	} {
		if err := st.CreateWorkspace(ctx, ws); err != nil {
			log.Fatal(err)
		}
	}

	cfg, err := config.LoadPreview()
	if err != nil {
		log.Fatal(err)
	}
	var auths []auth.Provider
	hosted := false
	if cfg.Auth {
		auths = []auth.Provider{
			devAuth{forge: "bitbucket", workspaces: []string{"acme", "personal", "bb-connected", "bb-broken"}},
			devAuth{forge: "github", workspaces: []string{"gh-new", "gh-connected", "gh-broken"}},
			devAuth{forge: "gitlab", workspaces: []string{"gl-group/platform", "gl-connected", "gl-broken", "gl-personal"}},
		}
		hosted = true
		log.Println("preview auth on: sign-in via bitbucket lands in acme, via github in the gh-* workspaces, via gitlab in gl-group/platform")
	}
	srv := server.New(server.Config{
		Store: st, Blobs: blobs,
		Parsers:          map[string]profile.Parser{"go": profile.GoParser{}},
		BaseURL:          "http://localhost:" + cfg.Port,
		Auths:            auths,
		Hosted:           hosted,
		PublicReports:    true,
		GitHubApp:        devGitHubApp{fg: forgefake.New()},
		BitbucketConnect: devBBConnect{fg: forgefake.New()},
		GitLabConnect:    devGLConnect{fg: forgefake.New()},
		PostHog:          server.PostHog{Key: cfg.PostHogKey, Host: "https://eu.i.posthog.com"},
	})
	log.Println("preview on :" + cfg.Port)
	log.Fatal(http.ListenAndServe(":"+cfg.Port, srv))
}

//go:fix inline

// steadyFiles are files whose coverage this commit did not move — seeded
// identically into the baseline and head uploads so the upload page can
// demonstrate the "show all files" toggle over the one file that did change.
func steadyFiles() []*store.UploadFile {
	mk := func(path string, covered, total int64) *store.UploadFile {
		return &store.UploadFile{
			Path: path, Pct: 100 * float64(covered) / float64(total),
			CoveredStmts: covered, TotalStmts: total,
			Blocks: []profile.Block{{StartLine: 1, EndLine: 8, NumStmts: int(covered), Count: 1}},
		}
	}
	return []*store.UploadFile{
		mk("internal/api/handler.go", 64, 70),
		mk("internal/api/middleware.go", 43, 50),
		mk("internal/ledger/posting.go", 51, 75),
		mk("internal/util/strings.go", 20, 20),
	}
}

// chargeSource is the synthetic file rendered by the source-view preview.
const chargeSource = `package billing

import (
	"context"
	"errors"
	"fmt"

	"github.com/acme/payments/internal/gateway"
)

var (
	ErrNonPositiveAmount = errors.New("amount must be positive")
	ErrCardDeclined      = errors.New("card declined")
	ErrAlreadyReversed   = errors.New("transaction already reversed")
)

// Service settles and charges card transactions against the ledger.
type Service struct {
	cards   CardStore
	gateway *gateway.Client
	ledger  Ledger
	metrics *Metrics
}

func (s *Service) settle(ctx context.Context, tx *gateway.Tx) error {
	if tx.State == gateway.StateSettled {
		s.metrics.AlreadySettled.Inc()
		return nil
	}
	if tx.State == gateway.StateReversed {
		s.metrics.Reversed.Inc()
		return ErrAlreadyReversed
	}
	if err := s.ledger.Reserve(ctx, tx); err != nil {
		return fmt.Errorf("reserve: %w", err)
	}
	return s.ledger.Commit(ctx, tx)
}

func (s *Service) Charge(ctx context.Context, req ChargeRequest) error {
	if err := req.Validate(); err != nil {
		return fmt.Errorf("invalid charge: %w", err)
	}
	if req.Amount <= 0 {
		return ErrNonPositiveAmount
	}
	card, err := s.cards.Lookup(ctx, req.CardID)
	if err != nil {
		return fmt.Errorf("card lookup: %w", err)
	}
	tx, err := s.gateway.Authorize(ctx, card, req.Amount)
	if errors.Is(err, gateway.ErrDeclined) {
		s.metrics.Declined.Inc()
		return ErrCardDeclined
	}
	return s.ledger.Record(ctx, tx)
}

func (s *Service) refund(ctx context.Context, id string) error {
	tx, err := s.gateway.Lookup(ctx, id)
	if err != nil {
		return fmt.Errorf("refund lookup: %w", err)
	}
	return s.gateway.Refund(ctx, tx)
}
`

// chargeBlocks overlays coverage on chargeSource: hit statement lines carry
// a positive count, uncovered ones a zero. The zero runs (27–28, 30–32, 35,
// 45, 53–54, 60–64) are what the miss-map rail and jump-to-miss surface.
func chargeBlocks() []profile.Block {
	hit := map[int]bool{
		26: true, 34: true, 37: true, 41: true, 42: true, 44: true,
		47: true, 48: true, 49: true, 51: true, 52: true, 56: true,
	}
	miss := []int{27, 28, 30, 31, 32, 35, 45, 53, 54, 60, 61, 62, 63, 64}
	var blocks []profile.Block
	for ln := range hit {
		blocks = append(blocks, profile.Block{StartLine: ln, EndLine: ln, NumStmts: 1, Count: 8})
	}
	for _, ln := range miss {
		blocks = append(blocks, profile.Block{StartLine: ln, EndLine: ln, NumStmts: 1, Count: 0})
	}
	return blocks
}

// chargeBaseBlocks is the baseline coverage: like chargeBlocks but with
// lines 45, 53 and 54 still covered, so the head commit reads as having
// newly uncovered them.
func chargeBaseBlocks() []profile.Block {
	blocks := chargeBlocks()
	regressed := map[int]bool{45: true, 53: true, 54: true}
	for i := range blocks {
		if regressed[blocks[i].StartLine] {
			blocks[i].Count = 5
		}
	}
	return blocks
}
