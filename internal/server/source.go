package server

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/bykclk/gocov/internal/forge"
	"github.com/bykclk/gocov/internal/profile"
	"github.com/bykclk/gocov/internal/store"
)

// maxSourceBytes bounds source files rendered by the source view.
const maxSourceBytes = 1 << 20

// sourceLine is one rendered line of the source view.
type sourceLine struct {
	No    int
	Class string // "hit", "miss" or "" for non-executable lines
	Hits  string // "3×", "✗" or ""
	Text  string
}

// handleSource implements GET /uploads/{id}/files/{path...} — the file's
// source at the upload's commit with per-line coverage overlay. Only paths
// recorded in the upload can be viewed.
func (s *Server) handleSource(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	path := r.PathValue("path")
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
	var file *store.UploadFile
	for _, f := range files {
		if f.Path == path {
			file = f
			break
		}
	}
	if file == nil {
		http.NotFound(w, r)
		return
	}
	repo, err := s.store.RepoByID(r.Context(), upload.RepoID)
	if err != nil {
		s.internalError(w, "loading repo for upload", err)
		return
	}

	source, unavailable := s.fetchSource(r, repo, upload, file.Path)
	data := map[string]any{
		"Repo":        repo,
		"Upload":      upload,
		"File":        file,
		"Uncovered":   uncoveredRanges(file.Blocks),
		"Unavailable": unavailable, // reason string when no source could be shown
		"Lines":       nil,
	}
	if unavailable == "" {
		data["Lines"] = renderSourceLines(source, file.Blocks)
	}
	s.render(w, "source.html", data)
}

// fetchSource returns the file content at the upload's commit, preferring
// the blobstore cache — commit content is immutable, so a cached copy
// never goes stale and keeps forge API usage down. On any failure it
// returns a human-readable reason instead; the page then falls back to
// the uncovered-ranges summary.
func (s *Server) fetchSource(r *http.Request, repo *store.Repo, u *store.Upload, profilePath string) ([]byte, string) {
	// Profile paths may be module-qualified; the forge wants repo paths.
	repoPath := profilePath
	if u.PathPrefix != "" {
		repoPath = strings.TrimPrefix(profilePath, u.PathPrefix+"/")
	}
	// Profile content is attacker-influencable by any token holder; a
	// path with dot segments could normalize into a different forge API
	// endpoint fetched with the bot's credentials.
	if !safeRepoPath(repoPath) {
		return nil, "the recorded file path cannot be requested from the forge"
	}

	cacheKey := fmt.Sprintf("source/%d/%s/%s", repo.ID, u.CommitSHA, repoPath)
	if cached, err := s.blobs.Get(r.Context(), cacheKey); err == nil {
		return s.validateSource(cached)
	}

	fg, err := s.forgeFor(repo)
	if err != nil {
		return nil, "no working forge integration: " + err.Error()
	}
	if fg == nil {
		return nil, "no forge credentials are configured for this repo"
	}
	content, err := fg.GetFileContent(r.Context(), repo.Slug, u.CommitSHA, repoPath)
	if errors.Is(err, forge.ErrRepoNotFound) {
		return nil, fmt.Sprintf("%s was not found at commit %s on %s", repoPath, u.CommitSHA, repo.Forge)
	}
	if errors.Is(err, forge.ErrNotImplemented) {
		return nil, "this forge does not support reading files"
	}
	if err != nil {
		// Forge error text can carry API URLs and response bodies; log it
		// but keep the page generic.
		s.log.Warn("fetch source", "repo", repo.Slug, "path", repoPath, "err", err)
		return nil, "fetching the file from the forge failed"
	}
	content, reason := s.validateSource(content)
	if reason != "" {
		return nil, reason
	}
	if err := s.blobs.Put(r.Context(), cacheKey, content); err != nil {
		s.log.Warn("cache source", "key", cacheKey, "err", err)
	}
	return content, ""
}

// safeRepoPath accepts only plain relative paths: no empty, "." or ".."
// segments and no leading slash.
func safeRepoPath(p string) bool {
	if p == "" || strings.HasPrefix(p, "/") {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

func (s *Server) validateSource(content []byte) ([]byte, string) {
	if len(content) > maxSourceBytes {
		return nil, "the file is too large to display"
	}
	if !utf8.Valid(content) {
		return nil, "the file is not valid UTF-8 text"
	}
	return content, ""
}

// renderSourceLines overlays coverage blocks onto source lines. A line is
// executable when any block spans it; it is covered when any such block
// has a positive count — the same rule diff coverage uses.
func renderSourceLines(source []byte, blocks []profile.Block) []sourceLine {
	text := strings.TrimSuffix(string(source), "\n")
	rawLines := strings.Split(text, "\n")

	covered := map[int]bool{}
	counts := map[int]int{}
	for _, b := range blocks {
		// Parsers validate line ranges, but old rows predate that; never
		// let a bogus range drive a long loop.
		for l := max(b.StartLine, 1); l <= b.EndLine && l <= len(rawLines); l++ {
			covered[l] = covered[l] || b.Count > 0
			if b.Count > counts[l] {
				counts[l] = b.Count
			}
		}
	}

	lines := make([]sourceLine, 0, len(rawLines))
	for i, raw := range rawLines {
		no := i + 1
		line := sourceLine{No: no, Text: strings.TrimSuffix(raw, "\r")}
		if hit, executable := covered[no]; executable {
			if hit {
				line.Class = "hit"
				line.Hits = fmt.Sprintf("%d×", counts[no])
			} else {
				line.Class = "miss"
				line.Hits = "✗"
			}
		}
		lines = append(lines, line)
	}
	return lines
}
