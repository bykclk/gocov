package profile

import (
	"reflect"
	"strings"
	"testing"
)

func TestLCOVParserParse(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
		want    map[string]struct {
			covered, total int64
			blocks         int
		}
	}{
		{
			name: "jest style tracefile",
			input: `TN:
SF:src/app.js
FN:3,handler
FNF:1
FNH:1
FNDA:5,handler
DA:1,1
DA:2,1
DA:3,5
DA:5,0
DA:6,0
LF:5
LH:3
BRDA:3,0,0,2
BRF:1
BRH:1
end_of_record
`,
			want: map[string]struct {
				covered, total int64
				blocks         int
			}{
				// lines 1,2 (count 1) collapse; line 3 (count 5); 5,6 (0) collapse
				"src/app.js": {covered: 3, total: 5, blocks: 3},
			},
		},
		{
			name: "multiple files sorted and ./ stripped",
			input: `SF:./src/b.ts
DA:1,0
end_of_record
SF:src/a.ts
DA:1,2
end_of_record
`,
			want: map[string]struct {
				covered, total int64
				blocks         int
			}{
				"src/a.ts": {covered: 1, total: 1, blocks: 1},
				"src/b.ts": {covered: 0, total: 1, blocks: 1},
			},
		},
		{
			name: "same file across test suites merges by summing",
			input: `SF:src/a.js
DA:1,1
DA:2,0
end_of_record
SF:src/a.js
DA:1,2
DA:2,3
end_of_record
`,
			want: map[string]struct {
				covered, total int64
				blocks         int
			}{
				// line 1 -> 3, line 2 -> 3: equal counts, consecutive -> 1 block
				"src/a.js": {covered: 2, total: 2, blocks: 1},
			},
		},
		{
			name: "gap between equal counts stays two blocks",
			input: `SF:a.js
DA:5,2
DA:7,2
end_of_record
`,
			want: map[string]struct {
				covered, total int64
				blocks         int
			}{
				"a.js": {covered: 2, total: 2, blocks: 2},
			},
		},
		{
			name:  "crlf line endings",
			input: "SF:a.js\r\nDA:1,1\r\nend_of_record\r\n",
			want: map[string]struct {
				covered, total int64
				blocks         int
			}{
				"a.js": {covered: 1, total: 1, blocks: 1},
			},
		},
		{
			name: "DA with checksum field",
			input: `SF:a.js
DA:1,4,abcDEF==
end_of_record
`,
			want: map[string]struct {
				covered, total int64
				blocks         int
			}{
				"a.js": {covered: 1, total: 1, blocks: 1},
			},
		},
		{name: "empty input", input: "", wantErr: true},
		{name: "no SF records", input: "TN:foo\n", wantErr: true},
		{name: "DA outside SF", input: "DA:1,1\n", wantErr: true},
		{name: "malformed DA", input: "SF:a.js\nDA:1\nend_of_record\n", wantErr: true},
		{name: "non-numeric DA line", input: "SF:a.js\nDA:x,1\nend_of_record\n", wantErr: true},
		{name: "negative count", input: "SF:a.js\nDA:1,-2\nend_of_record\n", wantErr: true},
		{name: "empty SF path", input: "SF:\nDA:1,1\nend_of_record\n", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := LCOVParser{}.Parse(strings.NewReader(tt.input))
			if tt.wantErr {
				if err == nil {
					t.Fatal("Parse() error = nil, want error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(p.Files) != len(tt.want) {
				t.Fatalf("got %d files, want %d: %+v", len(p.Files), len(tt.want), p.Files)
			}
			for i := 1; i < len(p.Files); i++ {
				if p.Files[i-1].Path >= p.Files[i].Path {
					t.Errorf("files not sorted: %q >= %q", p.Files[i-1].Path, p.Files[i].Path)
				}
			}
			for _, f := range p.Files {
				w, ok := tt.want[f.Path]
				if !ok {
					t.Errorf("unexpected file %q", f.Path)
					continue
				}
				covered, total := f.Coverage()
				if covered != w.covered || total != w.total || len(f.Blocks) != w.blocks {
					t.Errorf("%s: covered/total/blocks = %d/%d/%d, want %d/%d/%d",
						f.Path, covered, total, len(f.Blocks), w.covered, w.total, w.blocks)
				}
			}
		})
	}
}

func TestDetect(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"go profile", "mode: set\na.go:1.1,2.2 1 1\n", "go"},
		{"go profile with leading blank", "\n\nmode: atomic\n", "go"},
		{"lcov starting with TN", "TN:\nSF:a.js\nDA:1,1\n", "lcov"},
		{"lcov starting with SF", "SF:src/a.ts\nDA:1,0\n", "lcov"},
		{"lcov with crlf", "TN:suite\r\nSF:a.js\r\n", "lcov"},
		{"empty", "", ""},
		{"garbage", "hello world\nnot coverage\n", ""},
		{"json", "{\"foo\": 1}", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Detect([]byte(tt.input)); got != tt.want {
				t.Errorf("Detect() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestLCOVBlockBoundaries(t *testing.T) {
	// Collapsed blocks must never span a line that had no DA record.
	input := "SF:a.js\nDA:1,1\nDA:2,1\nDA:4,1\nend_of_record\n"
	p, err := LCOVParser{}.Parse(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	blocks := p.Files[0].Blocks
	want := []Block{
		{StartLine: 1, StartCol: 1, EndLine: 2, EndCol: 1, NumStmts: 2, Count: 1},
		{StartLine: 4, StartCol: 1, EndLine: 4, EndCol: 1, NumStmts: 1, Count: 1},
	}
	if !reflect.DeepEqual(blocks, want) {
		t.Errorf("blocks = %+v, want %+v", blocks, want)
	}
}
