package diffcov

import (
	"reflect"
	"strings"
	"testing"

	"github.com/bykclk/gocov/internal/profile"
)

func TestParseUnifiedDiff(t *testing.T) {
	tests := []struct {
		name    string
		diff    string
		want    map[string][]int
		wantErr bool
	}{
		{
			name: "single hunk with adds and context",
			diff: `diff --git a/internal/a.go b/internal/a.go
index 111..222 100644
--- a/internal/a.go
+++ b/internal/a.go
@@ -10,4 +10,6 @@ func f() {
 context
+added 11
+added 12
 context
-removed
+added 14
`,
			want: map[string][]int{"internal/a.go": {11, 12, 14}},
		},
		{
			name: "multiple files and hunks",
			diff: `diff --git a/a.go b/a.go
--- a/a.go
+++ b/a.go
@@ -1,2 +1,3 @@
 x
+new 2
 y
@@ -10,1 +11,2 @@
 z
+new 12
diff --git a/b.go b/b.go
--- a/b.go
+++ b/b.go
@@ -1 +1,2 @@
 a
+new 2
`,
			want: map[string][]int{"a.go": {2, 12}, "b.go": {2}},
		},
		{
			name: "new file",
			diff: `diff --git a/new.go b/new.go
new file mode 100644
--- /dev/null
+++ b/new.go
@@ -0,0 +1,3 @@
+l1
+l2
+l3
`,
			want: map[string][]int{"new.go": {1, 2, 3}},
		},
		{
			name: "deleted file skipped",
			diff: `diff --git a/gone.go b/gone.go
deleted file mode 100644
--- a/gone.go
+++ /dev/null
@@ -1,2 +0,0 @@
-x
-y
`,
			want: map[string][]int{},
		},
		{
			name: "rename with edit",
			diff: `diff --git a/old.go b/renamed.go
similarity index 90%
rename from old.go
rename to renamed.go
--- a/old.go
+++ b/renamed.go
@@ -5,2 +5,3 @@
 ctx
+added 6
 ctx
`,
			want: map[string][]int{"renamed.go": {6}},
		},
		{
			name: "no newline marker ignored",
			diff: `--- a/a.go
+++ b/a.go
@@ -1 +1,2 @@
 x
+y
\ No newline at end of file
`,
			want: map[string][]int{"a.go": {2}},
		},
		{
			name: "binary file ignored",
			diff: `diff --git a/img.png b/img.png
Binary files a/img.png and b/img.png differ
`,
			want: map[string][]int{},
		},
		{
			name:    "malformed hunk header",
			diff:    "--- a/a.go\n+++ b/a.go\n@@ garbage @@\n+x\n",
			wantErr: true,
		},
		{
			name: "empty diff",
			diff: "",
			want: map[string][]int{},
		},
		{
			// A removed SQL comment renders as "--- old comment" inside the
			// hunk; it must be treated as a removed line, not a file header.
			name: "removed line rendering as --- header",
			diff: `diff --git a/migrations/0001_init.sql b/migrations/0001_init.sql
--- a/migrations/0001_init.sql
+++ b/migrations/0001_init.sql
@@ -1,4 +1,5 @@
 CREATE TABLE t (
--- old comment
+-- new comment
+  id int,
   name text
 );
`,
			want: map[string][]int{"migrations/0001_init.sql": {2, 3}},
		},
		{
			// An added line whose content starts with "++ " renders as
			// "+++ show y"; it must not clobber the current file path.
			name: "added line rendering as +++ header",
			diff: `diff --git a/a.hs b/a.hs
--- a/a.hs
+++ b/a.hs
@@ -1,2 +1,4 @@
 msg = "x"
+++ show y
+more
 end
@@ -10,1 +12,2 @@
 ctx
+tail line
`,
			want: map[string][]int{"a.hs": {2, 3, 13}},
		},
		{
			// git C-quotes non-ASCII paths: +++ "b/caf\303\251.go"
			name: "quoted non-ascii path unescaped",
			diff: "diff --git \"a/caf\\303\\251.go\" \"b/caf\\303\\251.go\"\n" +
				"--- \"a/caf\\303\\251.go\"\n" +
				"+++ \"b/caf\\303\\251.go\"\n" +
				"@@ -1 +1,2 @@\n x\n+y\n",
			want: map[string][]int{"café.go": {2}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseUnifiedDiff(strings.NewReader(tt.diff))
			if tt.wantErr {
				if err == nil {
					t.Fatal("want error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func blocks(bs ...[4]int) []profile.Block {
	// each entry: startLine, endLine, numStmts, count
	out := make([]profile.Block, 0, len(bs))
	for _, b := range bs {
		out = append(out, profile.Block{StartLine: b[0], EndLine: b[1], NumStmts: b[2], Count: b[3]})
	}
	return out
}

func TestCompute(t *testing.T) {
	files := []FileBlocks{
		{
			Path: "github.com/x/mod/internal/a.go",
			Blocks: blocks(
				[4]int{10, 12, 2, 1}, // covered lines 10-12
				[4]int{14, 16, 2, 0}, // uncovered lines 14-16
			),
		},
		{
			Path:   "github.com/x/mod/b.go",
			Blocks: blocks([4]int{1, 5, 3, 7}),
		},
	}

	t.Run("intersection and suffix matching", func(t *testing.T) {
		added := map[string][]int{
			// 10,11 covered; 14 uncovered; 20 not executable (outside blocks)
			"internal/a.go":  {10, 11, 14, 20},
			"docs/readme.md": {1, 2},
		}
		res := Compute(files, added, "")
		if res.TotalLines != 3 || res.CoveredLines != 2 {
			t.Fatalf("totals = %d/%d, want 2/3", res.CoveredLines, res.TotalLines)
		}
		if len(res.Files) != 1 || res.Files[0].Path != "internal/a.go" {
			t.Fatalf("files = %+v", res.Files)
		}
		if !reflect.DeepEqual(res.Files[0].UncoveredLines, []int{14}) {
			t.Errorf("uncovered = %v, want [14]", res.Files[0].UncoveredLines)
		}
		if !reflect.DeepEqual(res.UnmatchedFiles, []string{"docs/readme.md"}) {
			t.Errorf("unmatched = %v", res.UnmatchedFiles)
		}
		if got := res.Percent(); got != float64(2)/float64(3)*100 {
			t.Errorf("percent = %v", got)
		}
	})

	t.Run("line in overlapping blocks covered if any counts", func(t *testing.T) {
		f := []FileBlocks{{Path: "a.go", Blocks: blocks(
			[4]int{5, 5, 1, 0},
			[4]int{5, 7, 1, 3},
		)}}
		res := Compute(f, map[string][]int{"a.go": {5}}, "")
		if res.CoveredLines != 1 || res.TotalLines != 1 {
			t.Errorf("totals = %d/%d, want 1/1", res.CoveredLines, res.TotalLines)
		}
	})

	t.Run("no executable changed lines", func(t *testing.T) {
		res := Compute(files, map[string][]int{"internal/a.go": {100, 200}}, "")
		if res.TotalLines != 0 || len(res.Files) != 0 {
			t.Errorf("res = %+v", res)
		}
		if res.Percent() != 100 {
			t.Errorf("empty diff percent = %v, want 100", res.Percent())
		}
	})

	t.Run("shortest profile path wins on ambiguous suffix", func(t *testing.T) {
		f := []FileBlocks{
			{Path: "github.com/x/vendor/github.com/y/pkg/a.go", Blocks: blocks([4]int{1, 1, 1, 0})},
			{Path: "github.com/y/pkg/a.go", Blocks: blocks([4]int{1, 1, 1, 5})},
		}
		res := Compute(f, map[string][]int{"pkg/a.go": {1}}, "")
		if res.CoveredLines != 1 {
			t.Errorf("matched the wrong file: %+v", res)
		}
	})

	t.Run("path prefix prevents false suffix match", func(t *testing.T) {
		// A brand-new untested util/errors.go must NOT bind to the covered
		// internal/util/errors.go just because the suffix matches.
		f := []FileBlocks{
			{Path: "example.com/mod/internal/util/errors.go", Blocks: blocks([4]int{1, 100, 5, 3})},
		}
		res := Compute(f, map[string][]int{"util/errors.go": {1, 2, 3}}, "example.com/mod")
		if res.TotalLines != 0 {
			t.Errorf("untested new file counted as covered: %+v", res)
		}
		if !reflect.DeepEqual(res.UnmatchedFiles, []string{"util/errors.go"}) {
			t.Errorf("unmatched = %v, want [util/errors.go]", res.UnmatchedFiles)
		}

		// The real file matches exactly through the prefix.
		res = Compute(f, map[string][]int{"internal/util/errors.go": {1, 2}}, "example.com/mod")
		if res.CoveredLines != 2 || res.TotalLines != 2 || len(res.UnmatchedFiles) != 0 {
			t.Errorf("prefixed match failed: %+v", res)
		}
	})

	t.Run("prefix with trailing slash", func(t *testing.T) {
		f := []FileBlocks{{Path: "example.com/mod/a.go", Blocks: blocks([4]int{1, 5, 1, 1})}}
		res := Compute(f, map[string][]int{"a.go": {2}}, "example.com/mod/")
		if res.CoveredLines != 1 {
			t.Errorf("trailing-slash prefix failed: %+v", res)
		}
	})

	t.Run("reverse suffix matches package-qualified paths", func(t *testing.T) {
		// JaCoCo layout: profile path is package-qualified, the diff path
		// carries the source root.
		f := []FileBlocks{
			{Path: "com/example/app/Foo.java", Blocks: blocks([4]int{10, 10, 1, 3}, [4]int{12, 12, 1, 0})},
		}
		res := Compute(f, map[string][]int{
			"src/main/java/com/example/app/Foo.java": {10, 12},
		}, "")
		if res.CoveredLines != 1 || res.TotalLines != 2 {
			t.Errorf("totals = %d/%d, want 1/2", res.CoveredLines, res.TotalLines)
		}
	})

	t.Run("reverse suffix picks the most specific profile path", func(t *testing.T) {
		f := []FileBlocks{
			{Path: "app/Foo.java", Blocks: blocks([4]int{1, 1, 1, 0})},
			{Path: "com/example/app/Foo.java", Blocks: blocks([4]int{1, 1, 1, 5})},
		}
		res := Compute(f, map[string][]int{"src/main/java/com/example/app/Foo.java": {1}}, "")
		if res.CoveredLines != 1 {
			t.Errorf("matched the wrong file: %+v", res)
		}
	})

	t.Run("bare profile basename never reverse-matches", func(t *testing.T) {
		// A default-package Main.java must not bind to every Main.java in
		// the diff.
		f := []FileBlocks{{Path: "Main.java", Blocks: blocks([4]int{1, 1, 1, 1})}}
		res := Compute(f, map[string][]int{"src/main/java/Main.java": {1}}, "")
		if res.TotalLines != 0 {
			t.Errorf("bare basename reverse match: %+v", res)
		}
	})

	t.Run("bare basename never suffix-matches without prefix", func(t *testing.T) {
		// A repo-root main.go absent from the profile must not bind to
		// some package's main.go by basename alone.
		f := []FileBlocks{{Path: "example.com/mod/cmd/x/main.go", Blocks: blocks([4]int{1, 50, 5, 2})}}
		res := Compute(f, map[string][]int{"main.go": {1, 2}}, "")
		if res.TotalLines != 0 {
			t.Errorf("basename false match: %+v", res)
		}
		if !reflect.DeepEqual(res.UnmatchedFiles, []string{"main.go"}) {
			t.Errorf("unmatched = %v, want [main.go]", res.UnmatchedFiles)
		}
	})
}

func TestResultClone(t *testing.T) {
	if (*Result)(nil).Clone() != nil {
		t.Error("nil.Clone() should be nil")
	}
	orig := &Result{
		Files: []FileCoverage{
			{Path: "a.go", CoveredLines: 1, TotalLines: 2, UncoveredLines: []int{7}},
		},
		CoveredLines:   1,
		TotalLines:     2,
		UnmatchedFiles: []string{"b.go"},
	}
	cp := orig.Clone()
	if !reflect.DeepEqual(orig, cp) {
		t.Fatalf("clone differs: %+v vs %+v", orig, cp)
	}
	cp.Files[0].UncoveredLines[0] = 99
	cp.UnmatchedFiles[0] = "mutated"
	if orig.Files[0].UncoveredLines[0] != 7 || orig.UnmatchedFiles[0] != "b.go" {
		t.Error("clone aliases the original's slices")
	}
}

func TestRanges(t *testing.T) {
	tests := []struct {
		in   []int
		want string
	}{
		{nil, ""},
		{[]int{5}, "5"},
		{[]int{5, 6, 7}, "5-7"},
		{[]int{5, 7}, "5, 7"},
		{[]int{45, 46, 47, 52, 60, 61}, "45-47, 52, 60-61"},
		{[]int{1, 1, 2}, "1-2"},
	}
	for _, tt := range tests {
		if got := Ranges(tt.in); got != tt.want {
			t.Errorf("Ranges(%v) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
