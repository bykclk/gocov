// Package diffcov computes diff coverage: which changed lines of a pull
// request are covered by tests. It intersects the added lines of a unified
// diff with the per-file statement blocks of the normalized coverage model.
package diffcov

import (
	"bufio"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/bykclk/gocov/internal/profile"
)

// FileBlocks is the per-file coverage input, decoupled from the store.
type FileBlocks struct {
	Path   string // as reported by the coverage profile (may be module-qualified)
	Blocks []profile.Block
}

// FileCoverage is the diff coverage of one changed file.
type FileCoverage struct {
	// Path is the repo-relative path from the diff (not the profile path).
	Path           string `json:"path"`
	CoveredLines   int64  `json:"covered_lines"`
	TotalLines     int64  `json:"total_lines"`
	UncoveredLines []int  `json:"uncovered_lines,omitempty"`
}

// Result is the diff coverage of a whole pull request.
type Result struct {
	// Files lists changed files that contain executable changed lines.
	Files        []FileCoverage `json:"files"`
	CoveredLines int64          `json:"covered_lines"`
	TotalLines   int64          `json:"total_lines"`
	// UnmatchedFiles are diff paths with added lines but no coverage data.
	// Callers decide which of these matter (e.g. only source files).
	UnmatchedFiles []string `json:"unmatched_files,omitempty"`
}

// Percent returns the diff coverage percentage. A diff with no executable
// changed lines is fully covered by definition.
func (r *Result) Percent() float64 {
	if r.TotalLines == 0 {
		return 100
	}
	return float64(r.CoveredLines) / float64(r.TotalLines) * 100
}

var hunkRe = regexp.MustCompile(`^@@ -\d+(?:,(\d+))? \+(\d+)(?:,(\d+))? @@`)

// ParseUnifiedDiff extracts the added line numbers (new-file numbering) per
// file from a unified diff, as produced by git or the Bitbucket diff API.
// Deleted files are skipped; a/ and b/ prefixes are stripped.
//
// Hunk extents are tracked via the line counts in the @@ header, so hunk
// content that itself looks like diff syntax (e.g. a removed SQL comment
// rendered as "--- foo", or added text rendered as "+++ bar") is never
// mistaken for file headers.
func ParseUnifiedDiff(r io.Reader) (map[string][]int, error) {
	added := map[string][]int{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	var (
		file           string // current target file; "" when none (e.g. deleted file)
		line           int    // next new-file line number within the current hunk
		oldRem, newRem int    // content lines remaining in the current hunk
		lineNo         int
	)
	for sc.Scan() {
		lineNo++
		l := sc.Text()

		if oldRem > 0 || newRem > 0 {
			// Inside a hunk: every line is content, never a header.
			switch {
			case strings.HasPrefix(l, "+"):
				if file != "" {
					added[file] = append(added[file], line)
				}
				line++
				newRem--
			case strings.HasPrefix(l, "-"):
				oldRem--
			case strings.HasPrefix(l, `\`):
				// "\ No newline at end of file" — not counted
			default:
				// context line (starts with ' '; tolerate empty lines too)
				line++
				oldRem--
				newRem--
			}
			continue
		}

		switch {
		case strings.HasPrefix(l, "diff "):
			file = ""
		case strings.HasPrefix(l, "+++ "):
			file = targetPath(l)
		case strings.HasPrefix(l, "@@"):
			m := hunkRe.FindStringSubmatch(l)
			if m == nil {
				return nil, fmt.Errorf("line %d: malformed hunk header %q", lineNo, l)
			}
			start, err := strconv.Atoi(m[2])
			if err != nil {
				return nil, fmt.Errorf("line %d: malformed hunk header %q", lineNo, l)
			}
			line = start
			oldRem = hunkCount(m[1])
			newRem = hunkCount(m[3])
		default:
			// diff metadata (---, index, mode, rename, binary) — ignore.
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	for _, lines := range added {
		sort.Ints(lines)
	}
	return added, nil
}

// hunkCount parses an optional hunk length; "@@ -1 +1 @@" means count 1.
func hunkCount(s string) int {
	if s == "" {
		return 1
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 1
	}
	return n
}

// targetPath extracts the new-file path from a "+++ b/..." line.
// Returns "" for /dev/null (deleted files).
func targetPath(l string) string {
	p := strings.TrimPrefix(l, "+++ ")
	// git may append "\t<timestamp>" after the path
	if i := strings.IndexByte(p, '\t'); i >= 0 {
		p = p[:i]
	}
	p = strings.TrimSpace(p)
	// With core.quotePath (the default), git C-quotes paths containing
	// non-ASCII or special bytes: +++ "b/caf\303\251.go". Go's quoted
	// string syntax is a superset of git's, so Unquote decodes them.
	if strings.HasPrefix(p, `"`) {
		if uq, err := strconv.Unquote(p); err == nil {
			p = uq
		} else {
			p = strings.Trim(p, `"`)
		}
	}
	if p == "/dev/null" {
		return ""
	}
	return strings.TrimPrefix(p, "b/")
}

// Compute intersects added diff lines with coverage blocks. Profile paths
// are module-qualified while diff paths are repo-relative; pathPrefix (e.g.
// the Go module path) makes the mapping exact: profile path must equal
// pathPrefix+"/"+diffPath. When pathPrefix is empty a suffix heuristic is
// used instead, which requires the diff path to have at least two path
// components — a bare basename match would happily bind a changed file to
// an unrelated one.
func Compute(files []FileBlocks, added map[string][]int, pathPrefix string) *Result {
	res := &Result{}
	diffPaths := make([]string, 0, len(added))
	for p := range added {
		diffPaths = append(diffPaths, p)
	}
	sort.Strings(diffPaths)

	for _, dp := range diffPaths {
		lines := added[dp]
		if len(lines) == 0 {
			continue
		}
		fb := matchFile(files, dp, pathPrefix)
		if fb == nil {
			res.UnmatchedFiles = append(res.UnmatchedFiles, dp)
			continue
		}

		changed := make(map[int]bool, len(lines))
		for _, l := range lines {
			changed[l] = true
		}
		// executable[l] exists if line l is inside any block;
		// value true if any such block has Count > 0.
		executable := map[int]bool{}
		for _, b := range fb.Blocks {
			for l := b.StartLine; l <= b.EndLine; l++ {
				if !changed[l] {
					continue
				}
				executable[l] = executable[l] || b.Count > 0
			}
		}
		if len(executable) == 0 {
			continue // no executable changed lines in this file
		}

		fc := FileCoverage{Path: dp, TotalLines: int64(len(executable))}
		for l, covered := range executable {
			if covered {
				fc.CoveredLines++
			} else {
				fc.UncoveredLines = append(fc.UncoveredLines, l)
			}
		}
		sort.Ints(fc.UncoveredLines)
		res.Files = append(res.Files, fc)
		res.CoveredLines += fc.CoveredLines
		res.TotalLines += fc.TotalLines
	}
	return res
}

// matchFile finds the coverage entry for a repo-relative diff path.
// With a pathPrefix the match is exact; without one a suffix heuristic
// applies (shortest profile path wins, being the least ambiguous), and
// bare basenames never suffix-match.
func matchFile(files []FileBlocks, diffPath, pathPrefix string) *FileBlocks {
	if pathPrefix != "" {
		want := strings.TrimSuffix(pathPrefix, "/") + "/" + diffPath
		for i := range files {
			if files[i].Path == want || files[i].Path == diffPath {
				return &files[i]
			}
		}
		return nil
	}

	var best *FileBlocks
	for i := range files {
		fb := &files[i]
		if fb.Path == diffPath {
			return fb
		}
		if strings.Contains(diffPath, "/") && strings.HasSuffix(fb.Path, "/"+diffPath) {
			if best == nil || len(fb.Path) < len(best.Path) {
				best = fb
			}
		}
	}
	return best
}

// Clone returns a deep copy, so stored results cannot alias caller slices.
func (r *Result) Clone() *Result {
	if r == nil {
		return nil
	}
	cp := *r
	cp.Files = make([]FileCoverage, len(r.Files))
	for i, f := range r.Files {
		cp.Files[i] = f
		cp.Files[i].UncoveredLines = append([]int(nil), f.UncoveredLines...)
	}
	cp.UnmatchedFiles = append([]string(nil), r.UnmatchedFiles...)
	return &cp
}

// Ranges renders sorted line numbers as compact ranges: "45-47, 52".
func Ranges(lines []int) string {
	if len(lines) == 0 {
		return ""
	}
	var sb strings.Builder
	start, prev := lines[0], lines[0]
	flush := func() {
		if sb.Len() > 0 {
			sb.WriteString(", ")
		}
		if start == prev {
			fmt.Fprintf(&sb, "%d", start)
		} else {
			fmt.Fprintf(&sb, "%d-%d", start, prev)
		}
	}
	for _, l := range lines[1:] {
		if l == prev || l == prev+1 {
			prev = l
			continue
		}
		flush()
		start, prev = l, l
	}
	flush()
	return sb.String()
}
