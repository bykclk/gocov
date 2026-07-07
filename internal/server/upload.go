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

	// PR-only fields, set when pr_id was part of the upload.
	DiffPct          *float64 `json:"diff_pct,omitempty"`
	DiffCoveredLines *int64   `json:"diff_covered_lines,omitempty"`
	DiffTotalLines   *int64   `json:"diff_total_lines,omitempty"`
	DiffStatus       string   `json:"diff_status,omitempty"` // "computed", "skipped: ..." or "error: ..."
	PRComment        string   `json:"pr_comment,omitempty"`  // "posted", "skipped" or "error: ..."
}

// handleUpload implements POST /api/v1/upload.
//
// Auth: Bearer <per-repo token>. Multipart form: file field "profile";
// value fields repo (optional, must match the token's repo), commit
// (required), branch (defaults to the repo's default branch), pr_id
// (optional), format (default "go").
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.authRepo(w, r)
	if !ok {
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
	if err := r.ParseMultipartForm(maxUploadBytes); err != nil {
		httpError(w, http.StatusBadRequest, "invalid multipart form: %v", err)
		return
	}

	if slug := r.FormValue("repo"); slug != "" && slug != repo.Slug {
		httpError(w, http.StatusForbidden, "token is for repo %q, not %q", repo.Slug, slug)
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
	format := r.FormValue("format")
	if format == "" {
		format = "go"
	}
	parser, ok := s.parsers[format]
	if !ok {
		httpError(w, http.StatusBadRequest, "unsupported format %q", format)
		return
	}

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

// forgeFor builds a forge client from the repo's stored credentials.
// Returns (nil, nil) when the repo has no credentials configured.
func (s *Server) forgeFor(repo *store.Repo) (forge.Forge, error) {
	if len(repo.ForgeCredentials) == 0 {
		return nil, nil
	}
	factory, ok := s.forges[repo.Forge]
	if !ok {
		return nil, fmt.Errorf("no integration for forge %q", repo.Forge)
	}
	return factory(repo.ForgeCredentials)
}

// sourceExt maps a profile format to the extension of source files whose
// absence from the coverage report is worth flagging in diff coverage.
var sourceExt = map[string]string{"go": ".go"}

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
	if ext := sourceExt[format]; ext != "" {
		var src []string
		for _, p := range result.UnmatchedFiles {
			if strings.HasSuffix(p, ext) {
				src = append(src, p)
			}
		}
		result.UnmatchedFiles = src
	}
	return result, "computed"
}

// authRepo resolves the Bearer token to a repo, writing the error response
// itself when authentication fails.
func (s *Server) authRepo(w http.ResponseWriter, r *http.Request) (*store.Repo, bool) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || token == "" {
		httpError(w, http.StatusUnauthorized, "missing bearer token")
		return nil, false
	}
	repo, err := s.store.RepoByToken(r.Context(), token)
	if errors.Is(err, store.ErrNotFound) {
		httpError(w, http.StatusUnauthorized, "invalid token")
		return nil, false
	}
	if err != nil {
		s.internalError(w, "looking up token", err)
		return nil, false
	}
	return repo, true
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
