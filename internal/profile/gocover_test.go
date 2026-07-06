package profile

import (
	"strings"
	"testing"
)

func TestGoParserParse(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
		// per-file expectations, keyed by path
		want map[string]struct {
			covered, total int64
			blocks         int
		}
	}{
		{
			name: "single file set mode",
			input: `mode: set
example.com/m/a.go:10.2,12.3 2 1
example.com/m/a.go:14.2,15.3 3 0
`,
			want: map[string]struct {
				covered, total int64
				blocks         int
			}{
				"example.com/m/a.go": {covered: 2, total: 5, blocks: 2},
			},
		},
		{
			name: "multiple files sorted",
			input: `mode: count
example.com/m/b.go:1.1,2.2 1 5
example.com/m/a.go:1.1,2.2 4 0
`,
			want: map[string]struct {
				covered, total int64
				blocks         int
			}{
				"example.com/m/a.go": {covered: 0, total: 4, blocks: 1},
				"example.com/m/b.go": {covered: 1, total: 1, blocks: 1},
			},
		},
		{
			name: "duplicate blocks merged by summing counts",
			input: `mode: atomic
example.com/m/a.go:10.2,12.3 2 0
example.com/m/a.go:10.2,12.3 2 3
`,
			want: map[string]struct {
				covered, total int64
				blocks         int
			}{
				"example.com/m/a.go": {covered: 2, total: 2, blocks: 1},
			},
		},
		{
			name: "path containing colons",
			input: `mode: set
C:/work/a.go:1.1,2.2 1 1
`,
			want: map[string]struct {
				covered, total int64
				blocks         int
			}{
				"C:/work/a.go": {covered: 1, total: 1, blocks: 1},
			},
		},
		{
			name:  "blank lines tolerated",
			input: "mode: set\n\nexample.com/m/a.go:1.1,2.2 1 1\n\n",
			want: map[string]struct {
				covered, total int64
				blocks         int
			}{
				"example.com/m/a.go": {covered: 1, total: 1, blocks: 1},
			},
		},
		{name: "empty input", input: "", wantErr: true},
		{name: "missing mode line", input: "example.com/m/a.go:1.1,2.2 1 1\n", wantErr: true},
		{name: "malformed position", input: "mode: set\na.go:1,2.2 1 1\n", wantErr: true},
		{name: "missing fields", input: "mode: set\na.go:1.1,2.2 1\n", wantErr: true},
		{name: "non-numeric count", input: "mode: set\na.go:1.1,2.2 1 x\n", wantErr: true},
		{name: "negative statements", input: "mode: set\na.go:1.1,2.2 -1 1\n", wantErr: true},
		{name: "no colon", input: "mode: set\njunk line\n", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := GoParser{}.Parse(strings.NewReader(tt.input))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Parse() error = nil, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse() error = %v", err)
			}
			if len(p.Files) != len(tt.want) {
				t.Fatalf("got %d files, want %d", len(p.Files), len(tt.want))
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

func TestGoParserBlockDetails(t *testing.T) {
	input := "mode: set\na.go:10.2,12.30 4 7\n"
	p, err := GoParser{}.Parse(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	b := p.Files[0].Blocks[0]
	want := Block{StartLine: 10, StartCol: 2, EndLine: 12, EndCol: 30, NumStmts: 4, Count: 7}
	if b != want {
		t.Errorf("block = %+v, want %+v", b, want)
	}
}

func TestProfileCoverage(t *testing.T) {
	p := &Profile{Files: []File{
		{Path: "a.go", Blocks: []Block{{NumStmts: 3, Count: 1}, {NumStmts: 2, Count: 0}}},
		{Path: "b.go", Blocks: []Block{{NumStmts: 5, Count: 9}}},
	}}
	covered, total := p.Coverage()
	if covered != 8 || total != 10 {
		t.Errorf("Coverage() = %d/%d, want 8/10", covered, total)
	}
}

func TestPercent(t *testing.T) {
	tests := []struct {
		covered, total int64
		want           float64
	}{
		{0, 0, 0},
		{0, 10, 0},
		{5, 10, 50},
		{10, 10, 100},
		{1, 3, float64(1) / float64(3) * 100},
	}
	for _, tt := range tests {
		if got := Percent(tt.covered, tt.total); got != tt.want {
			t.Errorf("Percent(%d, %d) = %v, want %v", tt.covered, tt.total, got, tt.want)
		}
	}
}
