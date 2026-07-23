package profile

import (
	"reflect"
	"strings"
	"testing"
)

const jacocoSample = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<!DOCTYPE report PUBLIC "-//JACOCO//DTD Report 1.1//EN" "report.dtd">
<report name="app">
  <sessioninfo id="host-1" start="1" dump="2"/>
  <package name="com/example/app">
    <class name="com/example/app/Foo" sourcefilename="Foo.java">
      <counter type="LINE" missed="1" covered="2"/>
    </class>
    <sourcefile name="Foo.java">
      <line nr="10" mi="0" ci="4" mb="0" cb="2"/>
      <line nr="11" mi="0" ci="4"/>
      <line nr="13" mi="2" ci="0"/>
      <counter type="LINE" missed="1" covered="2"/>
    </sourcefile>
  </package>
  <package name="com/example/util">
    <sourcefile name="Util.kt">
      <line nr="5" mi="0" ci="1"/>
    </sourcefile>
  </package>
  <counter type="LINE" missed="1" covered="3"/>
</report>
`

func TestJaCoCoParserParse(t *testing.T) {
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
			name:  "maven style report",
			input: jacocoSample,
			want: map[string]struct {
				covered, total int64
				blocks         int
			}{
				// lines 10,11 (ci 4) collapse; line 13 uncovered
				"com/example/app/Foo.java": {covered: 2, total: 3, blocks: 2},
				"com/example/util/Util.kt": {covered: 1, total: 1, blocks: 1},
			},
		},
		{
			name: "aggregate report with nested groups",
			input: `<report name="agg">
  <group name="module-a">
    <package name="com/a">
      <sourcefile name="A.java"><line nr="1" mi="0" ci="2"/></sourcefile>
    </package>
    <group name="inner">
      <package name="com/b">
        <sourcefile name="B.java"><line nr="1" mi="1" ci="0"/></sourcefile>
      </package>
    </group>
  </group>
</report>`,
			want: map[string]struct {
				covered, total int64
				blocks         int
			}{
				"com/a/A.java": {covered: 1, total: 1, blocks: 1},
				"com/b/B.java": {covered: 0, total: 1, blocks: 1},
			},
		},
		{
			name: "default package",
			input: `<report name="r"><package name="">
  <sourcefile name="Main.java"><line nr="3" mi="0" ci="1"/></sourcefile>
</package></report>`,
			want: map[string]struct {
				covered, total int64
				blocks         int
			}{
				"Main.java": {covered: 1, total: 1, blocks: 1},
			},
		},
		{
			name: "non executable lines skipped",
			input: `<report name="r"><package name="p">
  <sourcefile name="A.java">
    <line nr="1" mi="0" ci="0"/>
    <line nr="2" mi="0" ci="1"/>
  </sourcefile>
</package></report>`,
			want: map[string]struct {
				covered, total int64
				blocks         int
			}{
				"p/A.java": {covered: 1, total: 1, blocks: 1},
			},
		},
		{name: "empty input", input: "", wantErr: true},
		{name: "not xml", input: "hello", wantErr: true},
		{name: "wrong root element", input: "<coverage></coverage>", wantErr: true},
		{name: "no line data", input: `<report name="r"><package name="p"><sourcefile name="A.java"/></package></report>`, wantErr: true},
		{name: "malformed line nr", input: `<report name="r"><package name="p"><sourcefile name="A.java"><line nr="0" ci="1"/></sourcefile></package></report>`, wantErr: true},
		{name: "sourcefile without name", input: `<report name="r"><package name="p"><sourcefile><line nr="1" ci="1"/></sourcefile></package></report>`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := JaCoCoParser{}.Parse(strings.NewReader(tt.input))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Parse() error = nil, want error; got %+v", p)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(p.Files) != len(tt.want) {
				t.Fatalf("got %d files, want %d: %+v", len(p.Files), len(tt.want), p.Files)
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

func TestJaCoCoBlockBoundaries(t *testing.T) {
	input := `<report name="r"><package name="p">
  <sourcefile name="A.java">
    <line nr="1" ci="1"/>
    <line nr="2" ci="1"/>
    <line nr="4" ci="1"/>
  </sourcefile>
</package></report>`
	p, err := JaCoCoParser{}.Parse(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	want := []Block{
		{StartLine: 1, StartCol: 1, EndLine: 2, EndCol: 1, NumStmts: 2, Count: 1},
		{StartLine: 4, StartCol: 1, EndLine: 4, EndCol: 1, NumStmts: 1, Count: 1},
	}
	if !reflect.DeepEqual(p.Files[0].Blocks, want) {
		t.Errorf("blocks = %+v, want %+v", p.Files[0].Blocks, want)
	}
}

func TestDetectJaCoCo(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"full report with doctype", jacocoSample, "jacoco"},
		{"bare report element", `<report name="x"><package name="p"/></report>`, "jacoco"},
		{"xml prolog then report", "<?xml version=\"1.0\"?>\n<report name=\"x\">", "jacoco"},
		{"other xml", "<?xml version=\"1.0\"?>\n<data></data>", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Detect([]byte(tt.input)); got != tt.want {
				t.Errorf("Detect() = %q, want %q", got, tt.want)
			}
		})
	}
}
