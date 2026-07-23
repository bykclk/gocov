package profile

import (
	"strings"
	"testing"
)

const coberturaSample = `<?xml version="1.0" ?>
<!DOCTYPE coverage SYSTEM 'http://cobertura.sourceforge.net/xml/coverage-04.dtd'>
<coverage version="7.4.1" timestamp="1700000000" lines-valid="5" lines-covered="3" line-rate="0.6">
  <sources><source>/home/runner/project</source></sources>
  <packages>
    <package name="myapp" line-rate="0.6">
      <classes>
        <class name="app.py" filename="myapp/app.py" line-rate="0.75">
          <methods>
            <method name="handler" signature="">
              <lines><line number="2" hits="4"/></lines>
            </method>
          </methods>
          <lines>
            <line number="1" hits="1"/>
            <line number="2" hits="4"/>
            <line number="3" hits="4"/>
            <line number="6" hits="0" branch="true" condition-coverage="50% (1/2)"/>
          </lines>
        </class>
        <class name="util.py" filename="myapp/util.py" line-rate="0">
          <lines>
            <line number="1" hits="0"/>
          </lines>
        </class>
      </classes>
    </package>
  </packages>
</coverage>
`

func TestCoberturaParserParse(t *testing.T) {
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
			name:  "pytest-cov style report",
			input: coberturaSample,
			want: map[string]struct {
				covered, total int64
				blocks         int
			}{
				// method-level line 2 must not double-count: lines 1 (1),
				// 2-3 (4) collapse, 6 (0) -> 3 blocks. Covered 3 of 4.
				"myapp/app.py":  {covered: 3, total: 4, blocks: 3},
				"myapp/util.py": {covered: 0, total: 1, blocks: 1},
			},
		},
		{
			name: "classes sharing a filename merge",
			input: `<coverage><packages><package>
  <classes>
    <class name="A" filename="src/a.py"><lines><line number="1" hits="1"/></lines></class>
    <class name="A.Inner" filename="src/a.py"><lines><line number="1" hits="2"/><line number="2" hits="0"/></lines></class>
  </classes>
</package></packages></coverage>`,
			want: map[string]struct {
				covered, total int64
				blocks         int
			}{
				"src/a.py": {covered: 1, total: 2, blocks: 2},
			},
		},
		{
			name: "windows backslash paths normalized",
			input: `<coverage><packages><package><classes>
  <class name="A" filename="src\app\a.cs"><lines><line number="1" hits="1"/></lines></class>
</classes></package></packages></coverage>`,
			want: map[string]struct {
				covered, total int64
				blocks         int
			}{
				"src/app/a.cs": {covered: 1, total: 1, blocks: 1},
			},
		},
		{name: "empty input", input: "", wantErr: true},
		{name: "not xml", input: "hello", wantErr: true},
		{name: "wrong root element", input: "<report></report>", wantErr: true},
		{name: "no line data", input: `<coverage><packages><package><classes><class name="A" filename="a.py"/></classes></package></packages></coverage>`, wantErr: true},
		{name: "malformed line number", input: `<coverage><packages><package><classes><class name="A" filename="a.py"><lines><line number="0" hits="1"/></lines></class></classes></package></packages></coverage>`, wantErr: true},
		{name: "negative hits", input: `<coverage><packages><package><classes><class name="A" filename="a.py"><lines><line number="1" hits="-1"/></lines></class></classes></package></packages></coverage>`, wantErr: true},
		{name: "class without filename", input: `<coverage><packages><package><classes><class name="A"><lines><line number="1" hits="1"/></lines></class></classes></package></packages></coverage>`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := CoberturaParser{}.Parse(strings.NewReader(tt.input))
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

func TestDetectCobertura(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"full report with doctype", coberturaSample, "cobertura"},
		{"bare coverage element", `<coverage line-rate="0.5"><packages/></coverage>`, "cobertura"},
		{"xml prolog then coverage", "<?xml version=\"1.0\"?>\n<coverage>", "cobertura"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Detect([]byte(tt.input)); got != tt.want {
				t.Errorf("Detect() = %q, want %q", got, tt.want)
			}
		})
	}
}
