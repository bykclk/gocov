package profile

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// LCOVParser parses LCOV tracefiles (lcov.info) as produced by JavaScript
// and TypeScript coverage tools — Jest, Vitest, nyc/Istanbul, c8 — and by
// lcov itself. Only line records (DA:) contribute: gocov tracks line and
// statement coverage, so function (FN*) and branch (BRDA/BRF/BRH) records
// are ignored.
type LCOVParser struct{}

// Parse implements Parser. Repeated SF blocks for the same file (one per
// test suite) are merged by summing per-line hit counts, and consecutive
// lines with equal counts collapse into a single block.
func (LCOVParser) Parse(r io.Reader) (*Profile, error) {
	files := map[string]map[int]int{} // path -> line -> summed hit count
	var cur map[int]int

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		l := strings.TrimRight(sc.Text(), "\r")
		switch {
		case strings.HasPrefix(l, "SF:"):
			path := strings.TrimPrefix(strings.TrimSpace(l[3:]), "./")
			if path == "" {
				return nil, fmt.Errorf("line %d: empty SF record", lineNo)
			}
			if files[path] == nil {
				files[path] = map[int]int{}
			}
			cur = files[path]
		case strings.HasPrefix(l, "DA:"):
			if cur == nil {
				return nil, fmt.Errorf("line %d: DA record outside an SF block", lineNo)
			}
			// DA:<line>,<count>[,<checksum>]
			fields := strings.Split(l[3:], ",")
			if len(fields) < 2 {
				return nil, fmt.Errorf("line %d: malformed DA record %q", lineNo, l)
			}
			ln, err := strconv.Atoi(strings.TrimSpace(fields[0]))
			if err != nil || ln <= 0 {
				return nil, fmt.Errorf("line %d: malformed DA line number %q", lineNo, fields[0])
			}
			count, err := strconv.Atoi(strings.TrimSpace(fields[1]))
			if err != nil || count < 0 {
				return nil, fmt.Errorf("line %d: malformed DA hit count %q", lineNo, fields[1])
			}
			cur[ln] += count
		case l == "end_of_record":
			cur = nil
		default:
			// TN:, FN*, BRDA:, LF:, LH:, VER:, blank lines — ignored.
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, errors.New("lcov: no SF records found")
	}

	p := &Profile{Files: make([]File, 0, len(files))}
	for path, lines := range files {
		p.Files = append(p.Files, File{Path: path, Blocks: lcovBlocks(lines)})
	}
	sort.Slice(p.Files, func(i, j int) bool { return p.Files[i].Path < p.Files[j].Path })
	return p, nil
}

// lcovBlocks converts per-line hit counts to blocks, collapsing only
// strictly consecutive lines with equal counts — a gap must never end up
// inside a block, or diff coverage would treat non-executable lines as
// executable.
func lcovBlocks(lines map[int]int) []Block {
	nums := make([]int, 0, len(lines))
	for ln := range lines {
		nums = append(nums, ln)
	}
	sort.Ints(nums)

	var blocks []Block
	for _, ln := range nums {
		count := lines[ln]
		if n := len(blocks); n > 0 {
			last := &blocks[n-1]
			if ln == last.EndLine+1 && count == last.Count {
				last.EndLine = ln
				last.NumStmts++
				continue
			}
		}
		blocks = append(blocks, Block{
			StartLine: ln, StartCol: 1,
			EndLine: ln, EndCol: 1,
			NumStmts: 1, Count: count,
		})
	}
	return blocks
}
