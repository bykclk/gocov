package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDetectBuild(t *testing.T) {
	tests := []struct {
		name  string
		env   map[string]string
		git   map[string]string // key: joined args, value: output; missing = error
		files map[string]string // key: path, value: content; missing = error
		want  buildInfo
	}{
		{
			name: "bitbucket pipelines env wins",
			env: map[string]string{
				"BITBUCKET_REPO_FULL_NAME": "acme/widgets",
				"BITBUCKET_COMMIT":         "abc123",
				"BITBUCKET_BRANCH":         "main",
				"BITBUCKET_PR_ID":          "7",
			},
			git:  map[string]string{"rev-parse HEAD": "should-not-be-used"},
			want: buildInfo{Repo: "acme/widgets", Commit: "abc123", Branch: "main", PRID: "7"},
		},
		{
			name: "git fallback",
			env:  map[string]string{},
			git: map[string]string{
				"rev-parse HEAD":              "deadbeef",
				"rev-parse --abbrev-ref HEAD": "feature/x",
				"remote get-url origin":       "git@bitbucket.org:acme/widgets.git",
			},
			want: buildInfo{Repo: "acme/widgets", Commit: "deadbeef", Branch: "feature/x"},
		},
		{
			name: "detached head branch omitted",
			env:  map[string]string{},
			git: map[string]string{
				"rev-parse HEAD":              "deadbeef",
				"rev-parse --abbrev-ref HEAD": "HEAD",
			},
			want: buildInfo{Commit: "deadbeef"},
		},
		{
			name: "partial env fills from git",
			env:  map[string]string{"BITBUCKET_COMMIT": "abc123"},
			git: map[string]string{
				"rev-parse --abbrev-ref HEAD": "main",
				"remote get-url origin":       "https://bitbucket.org/acme/widgets.git",
			},
			want: buildInfo{Repo: "acme/widgets", Commit: "abc123", Branch: "main"},
		},
		{
			name: "github actions push event",
			env: map[string]string{
				"GITHUB_ACTIONS":    "true",
				"GITHUB_REPOSITORY": "acme/widgets",
				"GITHUB_SHA":        "abc123",
				"GITHUB_REF":        "refs/heads/main",
				"GITHUB_REF_NAME":   "main",
			},
			git:  map[string]string{"rev-parse HEAD": "should-not-be-used"},
			want: buildInfo{Repo: "acme/widgets", Commit: "abc123", Branch: "main"},
		},
		{
			// pull_request run: GITHUB_SHA/REF_NAME describe the merge
			// commit; the event payload's head SHA and branch must win,
			// and the PR number comes from the payload too.
			name: "github actions pull_request event",
			env: map[string]string{
				"GITHUB_ACTIONS":    "true",
				"GITHUB_REPOSITORY": "acme/widgets",
				"GITHUB_SHA":        "mergesha",
				"GITHUB_REF":        "refs/pull/42/merge",
				"GITHUB_REF_NAME":   "42/merge",
				"GITHUB_HEAD_REF":   "feature/x",
				"GITHUB_EVENT_PATH": "/event.json",
			},
			files: map[string]string{
				"/event.json": `{"pull_request": {"number": 42, "head": {"sha": "headsha", "ref": "feature/x"}}}`,
			},
			want: buildInfo{Repo: "acme/widgets", Commit: "headsha", Branch: "feature/x", PRID: "42"},
		},
		{
			// Unreadable event payload: the PR number still comes from
			// GITHUB_REF and the branch from GITHUB_HEAD_REF; the merge
			// SHA is the best commit available.
			name: "github actions pull_request without event payload",
			env: map[string]string{
				"GITHUB_ACTIONS":    "true",
				"GITHUB_REPOSITORY": "acme/widgets",
				"GITHUB_SHA":        "mergesha",
				"GITHUB_REF":        "refs/pull/42/merge",
				"GITHUB_REF_NAME":   "42/merge",
				"GITHUB_HEAD_REF":   "feature/x",
				"GITHUB_EVENT_PATH": "/gone.json",
			},
			want: buildInfo{Repo: "acme/widgets", Commit: "mergesha", Branch: "feature/x", PRID: "42"},
		},
		{
			// GITHUB_* variables in a developer shell must not be trusted
			// without the GITHUB_ACTIONS marker.
			name: "github env ignored outside actions",
			env:  map[string]string{"GITHUB_REPOSITORY": "acme/widgets"},
			git:  map[string]string{"rev-parse HEAD": "deadbeef"},
			want: buildInfo{Commit: "deadbeef"},
		},
		{
			name: "no env no git",
			env:  map[string]string{},
			git:  map[string]string{},
			want: buildInfo{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := func(k string) string { return tt.env[k] }
			git := func(args ...string) (string, error) {
				out, ok := tt.git[strings.Join(args, " ")]
				if !ok {
					return "", fmt.Errorf("git failed")
				}
				return out, nil
			}
			readFile := func(path string) ([]byte, error) {
				out, ok := tt.files[path]
				if !ok {
					return nil, fmt.Errorf("read failed")
				}
				return []byte(out), nil
			}
			got := detectBuild(env, git, readFile)
			if got != tt.want {
				t.Errorf("detectBuild() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestSlugFromRemote(t *testing.T) {
	tests := []struct {
		remote string
		want   string
	}{
		{"git@bitbucket.org:acme/widgets.git", "acme/widgets"},
		{"https://bitbucket.org/acme/widgets.git", "acme/widgets"},
		{"https://user@bitbucket.org/acme/widgets", "acme/widgets"},
		{"https://bitbucket.org/acme/widgets/", "acme/widgets"},
		{"ssh://git@bitbucket.org/acme/widgets.git", "acme/widgets"},
		{"git@github.com:acme/widgets.git", "acme/widgets"},
		{"https://github.com/acme/widgets.git", "acme/widgets"},
		{"nonsense", ""},
	}
	for _, tt := range tests {
		if got := slugFromRemote(tt.remote); got != tt.want {
			t.Errorf("slugFromRemote(%q) = %q, want %q", tt.remote, got, tt.want)
		}
	}
}

func TestModuleFromGoMod(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	tests := []struct {
		name string
		path string
		want string
	}{
		{"simple", write("a.mod", "module github.com/x/y\n\ngo 1.26\n"), "github.com/x/y"},
		{"comment then module", write("b.mod", "// hi\nmodule example.com/m\n"), "example.com/m"},
		{"quoted", write("c.mod", `module "example.com/q"`+"\n"), "example.com/q"},
		{"missing file", filepath.Join(dir, "nope.mod"), ""},
		{"no module line", write("d.mod", "go 1.26\n"), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := moduleFromGoMod(tt.path); got != tt.want {
				t.Errorf("moduleFromGoMod() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestMerge(t *testing.T) {
	b := buildInfo{Repo: "a/b", Commit: "c1", Branch: "main"}
	b.merge(buildInfo{Commit: "c2", PRID: "5"})
	want := buildInfo{Repo: "a/b", Commit: "c2", Branch: "main", PRID: "5"}
	if b != want {
		t.Errorf("merge() = %+v, want %+v", b, want)
	}
}
