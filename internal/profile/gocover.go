package profile

import (
	"bufio"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// GoParser parses Go cover profiles as written by `go test -coverprofile`:
//
//	mode: set|count|atomic
//	file.go:startLine.startCol,endLine.endCol numStmts count
type GoParser struct{}

type blockKey struct {
	startLine, startCol, endLine, endCol int
}

// Parse implements Parser. Duplicate blocks (from merged runs or count mode)
// are collapsed by summing their counts.
func (GoParser) Parse(r io.Reader) (*Profile, error) {
	files := map[string]map[blockKey]*Block{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	lineNo := 0
	seenMode := false
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "mode:") {
			seenMode = true
			continue
		}
		if !seenMode {
			return nil, fmt.Errorf("line %d: profile must start with a mode line", lineNo)
		}
		path, b, err := parseGoLine(line)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", lineNo, err)
		}
		blocks := files[path]
		if blocks == nil {
			blocks = map[blockKey]*Block{}
			files[path] = blocks
		}
		key := blockKey{b.StartLine, b.StartCol, b.EndLine, b.EndCol}
		if existing, ok := blocks[key]; ok {
			existing.Count += b.Count
		} else {
			blocks[key] = &b
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if !seenMode {
		return nil, fmt.Errorf("empty profile: missing mode line")
	}

	p := &Profile{}
	for path, blocks := range files {
		f := File{Path: path, Blocks: make([]Block, 0, len(blocks))}
		for _, b := range blocks {
			f.Blocks = append(f.Blocks, *b)
		}
		sort.Slice(f.Blocks, func(i, j int) bool {
			a, b := f.Blocks[i], f.Blocks[j]
			if a.StartLine != b.StartLine {
				return a.StartLine < b.StartLine
			}
			return a.StartCol < b.StartCol
		})
		p.Files = append(p.Files, f)
	}
	sort.Slice(p.Files, func(i, j int) bool { return p.Files[i].Path < p.Files[j].Path })
	return p, nil
}

// parseGoLine parses "path:startLine.startCol,endLine.endCol numStmts count".
// The path may contain colons, so the position spec is found from the last colon.
func parseGoLine(line string) (string, Block, error) {
	colon := strings.LastIndex(line, ":")
	if colon <= 0 {
		return "", Block{}, fmt.Errorf("malformed line %q", line)
	}
	path := line[:colon]
	rest := line[colon+1:]

	fields := strings.Fields(rest)
	if len(fields) != 3 {
		return "", Block{}, fmt.Errorf("malformed line %q: want 'pos numStmts count'", line)
	}
	start, end, ok := strings.Cut(fields[0], ",")
	if !ok {
		return "", Block{}, fmt.Errorf("malformed position %q", fields[0])
	}
	var b Block
	var err error
	if b.StartLine, b.StartCol, err = parsePos(start); err != nil {
		return "", Block{}, err
	}
	if b.EndLine, b.EndCol, err = parsePos(end); err != nil {
		return "", Block{}, err
	}
	if b.NumStmts, err = strconv.Atoi(fields[1]); err != nil {
		return "", Block{}, fmt.Errorf("malformed statement count %q", fields[1])
	}
	if b.Count, err = strconv.Atoi(fields[2]); err != nil {
		return "", Block{}, fmt.Errorf("malformed hit count %q", fields[2])
	}
	if b.NumStmts < 0 || b.Count < 0 {
		return "", Block{}, fmt.Errorf("negative value in line %q", line)
	}
	return path, b, nil
}

func parsePos(s string) (line, col int, err error) {
	l, c, ok := strings.Cut(s, ".")
	if !ok {
		return 0, 0, fmt.Errorf("malformed position %q", s)
	}
	if line, err = strconv.Atoi(l); err != nil {
		return 0, 0, fmt.Errorf("malformed position %q", s)
	}
	if col, err = strconv.Atoi(c); err != nil {
		return 0, 0, fmt.Errorf("malformed position %q", s)
	}
	return line, col, nil
}
