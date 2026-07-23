package profile

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"sort"
)

// JaCoCoParser parses JaCoCo XML reports (jacoco.xml), the standard
// coverage format of the JVM world — Maven/Gradle JaCoCo plugins, Kotlin
// and Android builds. Line elements carry covered (ci) and missed (mi)
// instruction counts; a line is covered when ci > 0. Aggregate reports
// with nested <group> elements are flattened.
type JaCoCoParser struct{}

type jacocoReport struct {
	XMLName  xml.Name        `xml:"report"`
	Groups   []jacocoGroup   `xml:"group"`
	Packages []jacocoPackage `xml:"package"`
}

type jacocoGroup struct {
	Groups   []jacocoGroup   `xml:"group"`
	Packages []jacocoPackage `xml:"package"`
}

type jacocoPackage struct {
	Name        string             `xml:"name,attr"`
	SourceFiles []jacocoSourceFile `xml:"sourcefile"`
}

type jacocoSourceFile struct {
	Name  string       `xml:"name,attr"`
	Lines []jacocoLine `xml:"line"`
}

type jacocoLine struct {
	Nr int `xml:"nr,attr"`
	MI int `xml:"mi,attr"`
	CI int `xml:"ci,attr"`
}

// Parse implements Parser. File paths are package-qualified
// ("com/example/Foo.java"); diff coverage matches them against
// repo-relative paths by suffix, so source roots like src/main/java
// need no configuration.
func (JaCoCoParser) Parse(r io.Reader) (*Profile, error) {
	var report jacocoReport
	if err := xml.NewDecoder(r).Decode(&report); err != nil {
		return nil, fmt.Errorf("jacoco: %w", err)
	}

	files := map[string]map[int]int{} // path -> line -> summed ci
	var walk func(groups []jacocoGroup, packages []jacocoPackage) error
	walk = func(groups []jacocoGroup, packages []jacocoPackage) error {
		for _, pkg := range packages {
			for _, sf := range pkg.SourceFiles {
				if sf.Name == "" {
					return errors.New("jacoco: sourcefile without a name")
				}
				path := sf.Name
				if pkg.Name != "" {
					path = pkg.Name + "/" + sf.Name
				}
				lines := files[path]
				if lines == nil {
					lines = map[int]int{}
					files[path] = lines
				}
				for _, l := range sf.Lines {
					if l.Nr <= 0 {
						return fmt.Errorf("jacoco: malformed line number %d in %s", l.Nr, path)
					}
					if l.MI == 0 && l.CI == 0 {
						continue // not executable
					}
					lines[l.Nr] += l.CI
				}
			}
		}
		for _, g := range groups {
			if err := walk(g.Groups, g.Packages); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(report.Groups, report.Packages); err != nil {
		return nil, err
	}

	p := &Profile{Files: make([]File, 0, len(files))}
	for path, lines := range files {
		if len(lines) == 0 {
			continue
		}
		p.Files = append(p.Files, File{Path: path, Blocks: blocksFromLineHits(lines)})
	}
	if len(p.Files) == 0 {
		return nil, errors.New("jacoco: no line coverage data found")
	}
	sort.Slice(p.Files, func(i, j int) bool { return p.Files[i].Path < p.Files[j].Path })
	return p, nil
}
