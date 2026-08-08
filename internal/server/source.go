package server

import (
	"bytes"
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
	if ok, err := s.canView(r, repo.Slug); err != nil {
		s.internalError(w, "checking access", err)
		return
	} else if !ok {
		http.NotFound(w, r)
		return
	}

	source, unavailable := s.fetchSource(r, repo, upload, file)
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
	s.render(w, r, "source.html", data)
}

// fetchSource returns the file content at the upload's commit, preferring
// the blobstore cache — commit content is immutable, so a cached copy
// never goes stale and keeps forge API usage down. On any failure it
// returns a human-readable reason instead; the page then falls back to
// the uncovered-ranges summary.
func (s *Server) fetchSource(r *http.Request, repo *store.Repo, u *store.Upload, f *store.UploadFile) ([]byte, string) {
	// Profile paths may be module-qualified; the forge wants repo paths.
	repoPath := f.Path
	if u.PathPrefix != "" {
		repoPath = strings.TrimPrefix(f.Path, u.PathPrefix+"/")
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
	// A commit's tree is immutable, so "not found" verdicts are cached
	// too — otherwise every view of an unresolvable file would replay
	// the whole probe sequence against the forge API, and this page
	// may be reachable without sign-in (auth is opt-in).
	missKey := fmt.Sprintf("source-miss/%d/%s/%s", repo.ID, u.CommitSHA, repoPath)
	notFound := fmt.Sprintf("%s was not found at commit %s on %s", repoPath, u.CommitSHA, repo.Forge)
	if _, err := s.blobs.Get(r.Context(), missKey); err == nil {
		return nil, notFound
	}

	fg, err := s.forgeFor(r.Context(), repo)
	if err != nil {
		return nil, "no working forge integration: " + err.Error()
	}
	if fg == nil {
		return nil, "no forge credentials are configured for this repo"
	}

	// Probing with trimmed prefixes exists for uploads whose stored
	// path_prefix could not map the recorded path to a repo path. When a
	// prefix was applied, the result is authoritative: a 404 then means
	// the file genuinely is not at that commit, and probing could only
	// ever surface a wrong same-suffix file.
	candidates := []string{repoPath}
	if u.PathPrefix == "" {
		candidates = sourceCandidates(repoPath)
	}
	var content []byte
	found := false
	var fetchErr error
	for _, cand := range candidates {
		b, err := fg.GetFileContent(r.Context(), repo.Slug, u.CommitSHA, cand)
		if err != nil {
			fetchErr = err
			if errors.Is(err, forge.ErrRepoNotFound) {
				continue
			}
			break
		}
		// A trimmed candidate is a guess; reject it when the file is
		// shorter than the lines the profile claims to cover — that is
		// a same-suffix collision with an unrelated file, and rendering
		// it would overlay meaningless coverage.
		if cand != repoPath && countLines(b) < maxBlockLine(f.Blocks) {
			s.log.Info("trimmed source candidate rejected as too short",
				"repo", repo.Slug, "recorded", repoPath, "candidate", cand)
			fetchErr = forge.ErrRepoNotFound
			continue
		}
		if cand != repoPath {
			s.log.Info("source resolved via trimmed path",
				"repo", repo.Slug, "recorded", repoPath, "resolved", cand)
		}
		content, found = b, true
		break
	}
	if !found {
		if fetchErr == nil || errors.Is(fetchErr, forge.ErrRepoNotFound) {
			if err := s.blobs.Put(r.Context(), missKey, []byte{'-'}); err != nil {
				s.log.Warn("cache source miss", "key", missKey, "err", err)
			}
			return nil, notFound
		}
		if errors.Is(fetchErr, forge.ErrNotImplemented) {
			return nil, "this forge does not support reading files"
		}
		// Forge error text can carry API URLs and response bodies; log
		// it but keep the page generic.
		s.log.Warn("fetch source", "repo", repo.Slug, "path", repoPath, "err", fetchErr)
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

// maxSourceProbes bounds forge API calls per source-view render: the
// recorded path plus a handful of trimmed variants.
const maxSourceProbes = 8

// sourceCandidates lists the repo paths to ask the forge for, in order.
// Profiles frequently record paths with extra leading directories that
// the stored path_prefix did not cover — a Go module path on uploads
// made before prefixes were stored, or a CI checkout directory in a
// Cobertura report. Trimming leading segments one at a time finds the
// repo path without any configuration. Trimmed candidates keep at
// least two segments so a bare filename cannot silently match an
// unrelated file at the repo root; every candidate is a suffix of the
// already-validated path, so it stays safe to request. When there are
// more suffixes than the probe budget, both ends are kept: short trims
// resolve module-qualified paths, deep trims resolve CI checkout
// prefixes — the middle is the least likely to be a repo root.
func sourceCandidates(repoPath string) []string {
	segs := strings.Split(repoPath, "/")
	var suffixes []string
	for i := 1; i <= len(segs)-2; i++ {
		suffixes = append(suffixes, strings.Join(segs[i:], "/"))
	}
	if len(suffixes) > maxSourceProbes-1 {
		head := suffixes[:(maxSourceProbes-1)/2]
		tail := suffixes[len(suffixes)-(maxSourceProbes-1-len(head)):]
		suffixes = append(head, tail...)
	}
	return append([]string{repoPath}, suffixes...)
}

// countLines reports how many lines content has, counting a trailing
// partial line.
func countLines(b []byte) int {
	n := bytes.Count(b, []byte{'\n'})
	if len(b) > 0 && b[len(b)-1] != '\n' {
		n++
	}
	return n
}

// maxBlockLine is the highest line the profile claims for the file.
func maxBlockLine(blocks []profile.Block) int {
	max := 0
	for _, b := range blocks {
		if b.EndLine > max {
			max = b.EndLine
		}
	}
	return max
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
