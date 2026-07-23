package profile

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
)

// CoberturaParser parses Cobertura XML reports — the format emitted by
// Python's coverage.py/pytest-cov (coverage.xml), PHPUnit, .NET coverlet
// and gcovr among others. Per-line hit counts come from <line> elements
// directly under each <class>; method-level line lists duplicate those and
// are ignored.
type CoberturaParser struct{}

type coberturaCoverage struct {
	XMLName  xml.Name           `xml:"coverage"`
	Packages []coberturaPackage `xml:"packages>package"`
}

type coberturaPackage struct {
	Classes []coberturaClass `xml:"classes>class"`
}

type coberturaClass struct {
	Filename string          `xml:"filename,attr"`
	Lines    []coberturaLine `xml:"lines>line"`
}

type coberturaLine struct {
	Number int `xml:"number,attr"`
	Hits   int `xml:"hits,attr"`
}

// Parse implements Parser. Multiple classes mapping to the same filename
// (nested/inner classes) merge by summing per-line hits.
func (CoberturaParser) Parse(r io.Reader) (*Profile, error) {
	var report coberturaCoverage
	if err := xml.NewDecoder(r).Decode(&report); err != nil {
		return nil, fmt.Errorf("cobertura: %w", err)
	}

	files := map[string]map[int]int{} // path -> line -> summed hits
	for _, pkg := range report.Packages {
		for _, cls := range pkg.Classes {
			if cls.Filename == "" {
				return nil, errors.New("cobertura: class without a filename")
			}
			// Windows-generated reports may use backslashes.
			path := strings.TrimPrefix(strings.ReplaceAll(cls.Filename, `\`, "/"), "./")
			lines := files[path]
			if lines == nil {
				lines = map[int]int{}
				files[path] = lines
			}
			for _, l := range cls.Lines {
				if l.Number <= 0 {
					return nil, fmt.Errorf("cobertura: malformed line number %d in %s", l.Number, path)
				}
				if l.Hits < 0 {
					return nil, fmt.Errorf("cobertura: negative hit count in %s line %d", path, l.Number)
				}
				lines[l.Number] += l.Hits
			}
		}
	}

	p := &Profile{Files: make([]File, 0, len(files))}
	for path, lines := range files {
		if len(lines) == 0 {
			continue
		}
		p.Files = append(p.Files, File{Path: path, Blocks: blocksFromLineHits(lines)})
	}
	if len(p.Files) == 0 {
		return nil, errors.New("cobertura: no line coverage data found")
	}
	sort.Slice(p.Files, func(i, j int) bool { return p.Files[i].Path < p.Files[j].Path })
	return p, nil
}
