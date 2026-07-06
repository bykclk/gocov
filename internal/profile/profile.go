// Package profile defines the normalized coverage model shared by every
// coverage format, and the Parser interface that format-specific parsers
// implement. Nothing outside this package may depend on a concrete format.
package profile

import "io"

// Block is a contiguous range of statements with a hit count. Line and
// column positions are 1-based; the end position is exclusive of the
// statement following the block, matching Go cover profile semantics.
type Block struct {
	StartLine int `json:"start_line"`
	StartCol  int `json:"start_col"`
	EndLine   int `json:"end_line"`
	EndCol    int `json:"end_col"`
	NumStmts  int `json:"num_stmts"`
	Count     int `json:"count"`
}

// File is the coverage data for a single source file.
type File struct {
	Path   string
	Blocks []Block
}

// Coverage returns the number of covered and total statements in the file.
func (f *File) Coverage() (covered, total int64) {
	for _, b := range f.Blocks {
		total += int64(b.NumStmts)
		if b.Count > 0 {
			covered += int64(b.NumStmts)
		}
	}
	return covered, total
}

// Profile is a parsed coverage profile normalized across formats.
type Profile struct {
	Files []File
}

// Coverage returns the number of covered and total statements in the profile.
func (p *Profile) Coverage() (covered, total int64) {
	for i := range p.Files {
		c, t := p.Files[i].Coverage()
		covered += c
		total += t
	}
	return covered, total
}

// Percent converts a covered/total statement pair to a percentage.
// A profile with no statements is 0% covered.
func Percent(covered, total int64) float64 {
	if total == 0 {
		return 0
	}
	return float64(covered) / float64(total) * 100
}

// Parser turns a raw coverage report into the normalized model.
// Implementations exist per format ("go" first; lcov, cobertura later).
type Parser interface {
	Parse(r io.Reader) (*Profile, error)
}
