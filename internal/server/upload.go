package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"

	"github.com/bykclk/gocov/internal/diffcov"
	"github.com/bykclk/gocov/internal/forge"
	"github.com/bykclk/gocov/internal/profile"
	"github.com/bykclk/gocov/internal/store"
)

// maxUploadBytes bounds the whole multipart request body.
const maxUploadBytes = 64 << 20

type uploadResponse struct {
	ID           int64    `json:"id"`
	TotalPct     float64  `json:"total_pct"`
	CoveredStmts int64    `json:"covered_stmts"`
	TotalStmts   int64    `json:"total_stmts"`
	DeltaPct     *float64 `json:"delta_pct,omitempty"`
	BuildStatus  string   `json:"build_status"` // "posted", "skipped" or "error: ..."
	// RepoCreated reports that this upload auto-registered the repo
	// through a workspace token.
	RepoCreated bool `json:"repo_created,omitempty"`

	// PR-only fields, set when pr_id was part of the upload.
	DiffPct          *float64 `json:"diff_pct,omitempty"`
	DiffCoveredLines *int64   `json:"diff_covered_lines,omitempty"`
	DiffTotalLines   *int64   `json:"diff_total_lines,omitempty"`
	DiffStatus       string   `json:"diff_status,omitempty"` // "computed", "skipped: ..." or "error: ..."
	PRComment        string   `json:"pr_comment,omitempty"`  // "posted", "skipped" or "error: ..."
}

// handleUpload implements POST /api/v1/upload.
//
// Auth: Bearer token — either a per-repo token or a workspace token.
// With a workspace token the repo field is required; unknown repos under
// the workspace prefix are registered automatically. Multipart form: file
// field "profile"; value fields repo, commit (required), branch (defaults
// to the repo's default branch), pr_id (optional), format (default "go").
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || token == "" {
		httpError(w, http.StatusUnauthorized, "missing bearer token")
		return
	}
	// Authenticate before touching the body so invalid tokens cost a
	// lookup, not a 64MB multipart parse.
	authedRepo, ws, ok := s.lookupUploadToken(w, r, token)
	if !ok {
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
	if err := r.ParseMultipartForm(maxUploadBytes); err != nil {
		httpError(w, http.StatusBadRequest, "invalid multipart form: %v", err)
		return
	}

	repo, repoCreated, ok := s.resolveUploadRepo(w, r, authedRepo, ws, r.FormValue("repo"))
	if !ok {
		return
	}
	commit := r.FormValue("commit")
	if commit == "" {
		httpError(w, http.StatusBadRequest, "missing field: commit")
		return
	}
	branch := r.FormValue("branch")
	if branch == "" {
		branch = repo.DefaultBranch
	}
	prID := r.FormValue("pr_id")

	file, _, err := r.FormFile("profile")
	if err != nil {
		httpError(w, http.StatusBadRequest, "missing file field: profile")
		return
	}
	defer file.Close()
	raw, err := io.ReadAll(file)
	if err != nil {
		httpError(w, http.StatusBadRequest, "reading profile: %v", err)
		return
	}

	// An explicit format wins; otherwise sniff the content, keeping "go"
	// as the historical default for unrecognizable input.
	format := r.FormValue("format")
	if format == "" {
		if detected := profile.Detect(raw); detected != "" {
			format = detected
		} else {
			format = "go"
		}
	}
	parser, ok := s.parsers[format]
	if !ok {
		httpError(w, http.StatusBadRequest, "unsupported format %q", format)
		return
	}

	prof, err := parser.Parse(bytes.NewReader(raw))
	if err != nil {
		httpError(w, http.StatusUnprocessableEntity, "parsing %s profile: %v", format, err)
		return
	}

	covered, total := prof.Coverage()
	totalPct := profile.Percent(covered, total)

	// Delta vs the previous upload on the same branch, falling back to the
	// default branch for first-time feature branches.
	var deltaPct *float64
	prev, err := s.store.LatestUpload(r.Context(), repo.ID, branch)
	if errors.Is(err, store.ErrNotFound) && branch != repo.DefaultBranch {
		prev, err = s.store.LatestUpload(r.Context(), repo.ID, repo.DefaultBranch)
	}
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		s.internalError(w, "loading previous upload", err)
		return
	}
	if prev != nil {
		d := totalPct - prev.TotalPct
		deltaPct = &d
	}

	blobKey, err := s.storeRawProfile(r, repo.ID, raw)
	if err != nil {
		s.internalError(w, "storing raw profile", err)
		return
	}

	// Forge client for build status, PR comment and diff coverage; nil when
	// the repo has no credentials configured.
	fg, fgErr := s.forgeFor(repo)

	var diffResult *diffcov.Result
	var diffStatus string
	if prID != "" {
		diffResult, diffStatus = s.computeDiffCoverage(r.Context(), fg, fgErr, repo, prID, prof, format, r.FormValue("path_prefix"))
	}

	upload := &store.Upload{
		RepoID:       repo.ID,
		CommitSHA:    commit,
		Branch:       branch,
		PRID:         prID,
		Format:       format,
		TotalPct:     totalPct,
		CoveredStmts: covered,
		TotalStmts:   total,
		RawBlobKey:   blobKey,
		DiffCoverage: diffResult,
	}
	files := make([]*store.UploadFile, 0, len(prof.Files))
	for i := range prof.Files {
		f := &prof.Files[i]
		c, t := f.Coverage()
		files = append(files, &store.UploadFile{
			Path:         f.Path,
			Pct:          profile.Percent(c, t),
			CoveredStmts: c,
			TotalStmts:   t,
			Blocks:       f.Blocks,
		})
	}
	if err := s.store.CreateUpload(r.Context(), upload, files); err != nil {
		// The raw profile was already written; don't leave it orphaned.
		if delErr := s.blobs.Delete(r.Context(), blobKey); delErr != nil {
			s.log.Error("cleaning up blob after failed upload", "key", blobKey, "err", delErr)
		}
		s.internalError(w, "saving upload", err)
		return
	}

	resp := uploadResponse{
		ID:           upload.ID,
		TotalPct:     totalPct,
		CoveredStmts: covered,
		TotalStmts:   total,
		DeltaPct:     deltaPct,
		BuildStatus:  s.pushBuildStatus(r.Context(), fg, fgErr, repo, upload, deltaPct),
		RepoCreated:  repoCreated,
		DiffStatus:   diffStatus,
		PRComment:    s.pushPRComment(r.Context(), fg, fgErr, repo, upload, deltaPct),
	}
	if diffResult != nil {
		pct := diffResult.Percent()
		resp.DiffPct = &pct
		resp.DiffCoveredLines = &diffResult.CoveredLines
		resp.DiffTotalLines = &diffResult.TotalLines
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(resp)
}

// forgeFor builds a forge client from the repo's stored credentials,
// falling back to the server-wide default credentials for the repo's
// forge. Returns (nil, nil) when neither is configured.
func (s *Server) forgeFor(repo *store.Repo) (forge.Forge, error) {
	creds := repo.ForgeCredentials
	if len(creds) == 0 {
		creds = s.defaultCreds[repo.Forge]
	}
	return s.forgeFromCreds(repo.Forge, creds)
}

// forgeFromCreds builds a forge client for the named forge with the given
// credentials; (nil, nil) when there are no credentials.
func (s *Server) forgeFromCreds(forgeName string, creds map[string]string) (forge.Forge, error) {
	if len(creds) == 0 {
		return nil, nil
	}
	factory, ok := s.forges[forgeName]
	if !ok {
		return nil, fmt.Errorf("no integration for forge %q", forgeName)
	}
	return factory(creds)
}

// sourceExts maps a profile format to the extensions of source files whose
// absence from the coverage report is worth flagging in diff coverage.
var sourceExts = map[string][]string{
	"go":     {".go"},
	"lcov":   {".js", ".jsx", ".ts", ".tsx", ".mjs", ".cjs", ".vue", ".svelte"},
	"jacoco": {".java", ".kt", ".kts", ".scala", ".groovy"},
}

// computeDiffCoverage fetches the PR diff from the forge and intersects it
// with the parsed profile. Best effort: any failure is reported in the
// returned status, never as an upload error.
func (s *Server) computeDiffCoverage(ctx context.Context, fg forge.Forge, fgErr error, repo *store.Repo, prID string, prof *profile.Profile, format, pathPrefix string) (*diffcov.Result, string) {
	if fgErr != nil {
		return nil, "error: " + fgErr.Error()
	}
	if fg == nil {
		return nil, "skipped: no forge credentials"
	}
	diffText, err := fg.GetPRDiff(ctx, repo.Slug, prID)
	if errors.Is(err, forge.ErrNotImplemented) {
		return nil, "skipped: diff not supported by forge"
	}
	if err != nil {
		s.log.Error("fetch PR diff", "repo", repo.Slug, "pr", prID, "err", err)
		return nil, "error: fetching PR diff: " + err.Error()
	}
	added, err := diffcov.ParseUnifiedDiff(strings.NewReader(diffText))
	if err != nil {
		s.log.Error("parse PR diff", "repo", repo.Slug, "pr", prID, "err", err)
		return nil, "error: parsing PR diff: " + err.Error()
	}

	files := make([]diffcov.FileBlocks, 0, len(prof.Files))
	for _, f := range prof.Files {
		files = append(files, diffcov.FileBlocks{Path: f.Path, Blocks: f.Blocks})
	}
	result := diffcov.Compute(files, added, pathPrefix)

	// Keep only source files in the "changed but no coverage data" list;
	// docs, configs etc. are expected to be absent from the profile.
	if exts := sourceExts[format]; len(exts) > 0 {
		var src []string
		for _, p := range result.UnmatchedFiles {
			for _, ext := range exts {
				if strings.HasSuffix(p, ext) {
					src = append(src, p)
					break
				}
			}
		}
		result.UnmatchedFiles = src
	}
	return result, "computed"
}

// lookupUploadToken authenticates the Bearer token as either a per-repo
// token or a workspace token, writing the error response itself. Runs
// before the request body is parsed.
func (s *Server) lookupUploadToken(w http.ResponseWriter, r *http.Request, token string) (*store.Repo, *store.Workspace, bool) {
	ctx := r.Context()
	repo, err := s.store.RepoByToken(ctx, token)
	if err == nil {
		return repo, nil, true
	}
	if !errors.Is(err, store.ErrNotFound) {
		s.internalError(w, "looking up token", err)
		return nil, nil, false
	}
	ws, err := s.store.WorkspaceByToken(ctx, token)
	if err == nil {
		return nil, ws, true
	}
	if errors.Is(err, store.ErrNotFound) {
		httpError(w, http.StatusUnauthorized, "invalid token")
		return nil, nil, false
	}
	s.internalError(w, "looking up workspace token", err)
	return nil, nil, false
}

// repoNameRe bounds the repo part of auto-registered slugs: one path
// segment, conservative charset, sane length.
var repoNameRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)

// resolveUploadRepo maps the authenticated token to the target repo,
// writing the error response itself on failure. Workspace tokens require
// the repo slug, must match the workspace prefix, and register unknown
// repos on the fly.
func (s *Server) resolveUploadRepo(w http.ResponseWriter, r *http.Request, repo *store.Repo, ws *store.Workspace, slug string) (_ *store.Repo, created, ok bool) {
	ctx := r.Context()
	if repo != nil {
		if slug != "" && slug != repo.Slug {
			httpError(w, http.StatusForbidden, "token is for repo %q, not %q", repo.Slug, slug)
			return nil, false, false
		}
		return repo, false, true
	}

	if slug == "" {
		httpError(w, http.StatusBadRequest, "workspace tokens require the repo field")
		return nil, false, false
	}
	prefix, name, found := strings.Cut(slug, "/")
	if !found || prefix != ws.Prefix {
		httpError(w, http.StatusForbidden, "token is for workspace %q, not %q", ws.Prefix, slug)
		return nil, false, false
	}
	if !repoNameRe.MatchString(name) {
		httpError(w, http.StatusBadRequest, "invalid repo name %q: want %s/<name> with a single path segment", slug, ws.Prefix)
		return nil, false, false
	}

	repo, err := s.store.RepoBySlug(ctx, slug)
	if err == nil {
		return repo, false, true
	}
	if !errors.Is(err, store.ErrNotFound) {
		s.internalError(w, "looking up repo", err)
		return nil, false, false
	}
	repo, err = s.autoCreateRepo(ctx, ws, slug)
	if errors.Is(err, forge.ErrRepoNotFound) {
		httpError(w, http.StatusNotFound, "repo %q not found on %s", slug, ws.Forge)
		return nil, false, false
	}
	if err != nil {
		s.internalError(w, "auto-registering repo", err)
		return nil, false, false
	}
	return repo, true, true
}

// autoCreateRepo registers a repo first seen through a workspace token.
// The default branch is asked from the forge when a client can be built
// (repo-less, so global credentials only), then falls back to the
// workspace default and finally to "main". A forge that positively says
// the repo does not exist aborts the registration (ErrRepoNotFound), so a
// leaked workspace token cannot fill the dashboard with invented repos.
func (s *Server) autoCreateRepo(ctx context.Context, ws *store.Workspace, slug string) (*store.Repo, error) {
	branch := ""
	if fg, err := s.forgeFromCreds(ws.Forge, s.defaultCreds[ws.Forge]); err == nil && fg != nil {
		b, err := fg.GetDefaultBranch(ctx, slug)
		switch {
		case err == nil && b != "":
			branch = b
		case errors.Is(err, forge.ErrRepoNotFound):
			return nil, err
		case err != nil && !errors.Is(err, forge.ErrNotImplemented):
			// Transient forge trouble must not block a legitimate first
			// upload; fall back to the workspace default branch.
			s.log.Warn("get default branch", "repo", slug, "err", err)
		}
	}
	if branch == "" {
		branch = ws.DefaultBranch
	}
	if branch == "" {
		branch = "main"
	}

	token, err := newToken()
	if err != nil {
		return nil, err
	}
	repo := &store.Repo{
		Forge:         ws.Forge,
		Slug:          slug,
		Token:         token,
		DefaultBranch: branch,
	}
	if err := s.store.CreateRepo(ctx, repo); err != nil {
		// A concurrent first upload may have won the race; use its repo.
		if existing, lookupErr := s.store.RepoBySlug(ctx, slug); lookupErr == nil {
			return existing, nil
		}
		return nil, err
	}
	s.log.Info("auto-registered repo", "slug", slug, "default_branch", branch, "workspace", ws.Prefix)
	return repo, nil
}

// newToken generates an upload token for auto-registered repos.
func newToken() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func (s *Server) storeRawProfile(r *http.Request, repoID int64, raw []byte) (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	key := fmt.Sprintf("profiles/%d/%s", repoID, hex.EncodeToString(buf))
	if err := s.blobs.Put(r.Context(), key, raw); err != nil {
		return "", err
	}
	return key, nil
}

// pushBuildStatus posts a "coverage: X% (±Y)" build status to the repo's
// forge. Best effort: failures are reported in the response but do not fail
// the upload.
func (s *Server) pushBuildStatus(ctx context.Context, fg forge.Forge, fgErr error, repo *store.Repo, u *store.Upload, deltaPct *float64) string {
	if fgErr != nil {
		return "error: " + fgErr.Error()
	}
	if fg == nil {
		return "skipped"
	}

	desc := fmt.Sprintf("coverage: %.1f%%", u.TotalPct)
	if deltaPct != nil {
		desc += fmt.Sprintf(" (%+.1f%%)", *deltaPct)
	}
	status := forge.BuildStatus{
		Key:         "gocov/coverage",
		State:       forge.StateSuccessful,
		Name:        "gocov",
		Description: desc,
		URL:         s.uploadURL(u),
	}
	if err := fg.PostBuildStatus(ctx, repo.Slug, u.CommitSHA, status); err != nil {
		s.log.Error("post build status", "repo", repo.Slug, "commit", u.CommitSHA, "err", err)
		return "error: " + err.Error()
	}
	return "posted"
}

// pushPRComment posts a coverage summary comment on the pull request.
// Returns "" for non-PR uploads so the field is omitted from the response.
func (s *Server) pushPRComment(ctx context.Context, fg forge.Forge, fgErr error, repo *store.Repo, u *store.Upload, deltaPct *float64) string {
	if u.PRID == "" {
		return ""
	}
	if fgErr != nil {
		return "error: " + fgErr.Error()
	}
	if fg == nil {
		return "skipped"
	}
	if err := fg.PostPRComment(ctx, repo.Slug, u.PRID, s.prCommentBody(u, deltaPct)); err != nil {
		s.log.Error("post PR comment", "repo", repo.Slug, "pr", u.PRID, "err", err)
		return "error: " + err.Error()
	}
	return "posted"
}

// prCommentMaxFiles caps the uncovered-lines table in PR comments.
const prCommentMaxFiles = 20

func (s *Server) prCommentBody(u *store.Upload, deltaPct *float64) string {
	var sb strings.Builder
	short := u.CommitSHA
	if len(short) > 12 {
		short = short[:12]
	}
	fmt.Fprintf(&sb, "**gocov** report for `%s`\n\n", short)
	fmt.Fprintf(&sb, "- Total coverage: **%.1f%%**", u.TotalPct)
	if deltaPct != nil {
		fmt.Fprintf(&sb, " (%+.1f%%)", *deltaPct)
	}
	sb.WriteString("\n")

	if dc := u.DiffCoverage; dc != nil {
		if dc.TotalLines == 0 {
			sb.WriteString("- Diff coverage: no executable lines changed\n")
		} else {
			fmt.Fprintf(&sb, "- Diff coverage: **%.1f%%** (%d/%d changed lines covered)\n",
				dc.Percent(), dc.CoveredLines, dc.TotalLines)
		}

		var uncovered []diffcov.FileCoverage
		for _, f := range dc.Files {
			if len(f.UncoveredLines) > 0 {
				uncovered = append(uncovered, f)
			}
		}
		if len(uncovered) > 0 {
			sb.WriteString("\nUncovered changed lines:\n\n| File | Lines |\n| --- | --- |\n")
			for i, f := range uncovered {
				if i == prCommentMaxFiles {
					fmt.Fprintf(&sb, "| … | and %d more files |\n", len(uncovered)-prCommentMaxFiles)
					break
				}
				fmt.Fprintf(&sb, "| `%s` | %s |\n", mdPath(f.Path), diffcov.Ranges(f.UncoveredLines))
			}
		}
		if n := len(dc.UnmatchedFiles); n > 0 {
			shown := dc.UnmatchedFiles
			if n > prCommentMaxFiles {
				shown = shown[:prCommentMaxFiles]
			}
			escaped := make([]string, len(shown))
			for i, p := range shown {
				escaped[i] = mdPath(p)
			}
			fmt.Fprintf(&sb, "\nChanged files without coverage data: `%s`",
				strings.Join(escaped, "`, `"))
			if n > prCommentMaxFiles {
				fmt.Fprintf(&sb, " and %d more", n-prCommentMaxFiles)
			}
			sb.WriteString("\n")
		}
	}

	fmt.Fprintf(&sb, "\n[Full report](%s)\n", s.uploadURL(u))
	return sb.String()
}

func (s *Server) uploadURL(u *store.Upload) string {
	return fmt.Sprintf("%s/uploads/%d", strings.TrimSuffix(s.baseURL, "/"), u.ID)
}

// mdPath neutralizes characters that would break the markdown table or the
// surrounding code span in PR comments. Paths come from the PR diff.
var mdPathReplacer = strings.NewReplacer("`", "'", "|", "\\|", "\n", " ", "\r", " ")

func mdPath(p string) string {
	return mdPathReplacer.Replace(p)
}

func httpError(w http.ResponseWriter, code int, format string, args ...any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": fmt.Sprintf(format, args...)})
}

func (s *Server) internalError(w http.ResponseWriter, msg string, err error) {
	s.log.Error(msg, "err", err)
	httpError(w, http.StatusInternalServerError, "internal error")
}
