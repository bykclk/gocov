package server

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

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
		s.internalError(w, "saving upload", err)
		return
	}

	resp := uploadResponse{
		ID:           upload.ID,
		TotalPct:     totalPct,
		CoveredStmts: covered,
		TotalStmts:   total,
		DeltaPct:     deltaPct,
		BuildStatus:  s.pushBuildStatus(r, repo, upload, deltaPct),
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(resp)
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
func (s *Server) pushBuildStatus(r *http.Request, repo *store.Repo, u *store.Upload, deltaPct *float64) string {
	if len(repo.ForgeCredentials) == 0 {
		return "skipped"
	}
	factory, ok := s.forges[repo.Forge]
	if !ok {
		return fmt.Sprintf("error: no integration for forge %q", repo.Forge)
	}
	f, err := factory(repo.ForgeCredentials)
	if err != nil {
		return "error: " + err.Error()
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
		URL:         fmt.Sprintf("%s/uploads/%d", strings.TrimSuffix(s.baseURL, "/"), u.ID),
	}
	if err := f.PostBuildStatus(r.Context(), repo.Slug, u.CommitSHA, status); err != nil {
		s.log.Error("post build status", "repo", repo.Slug, "commit", u.CommitSHA, "err", err)
		return "error: " + err.Error()
	}
	return "posted"
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
