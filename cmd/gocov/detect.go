package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// buildInfo is the CI metadata attached to an upload.
type buildInfo struct {
	Repo   string
	Commit string
	Branch string
	PRID   string
}

// merge overrides fields with non-empty values from explicit flags.
func (b *buildInfo) merge(override buildInfo) {
	if override.Repo != "" {
		b.Repo = override.Repo
	}
	if override.Commit != "" {
		b.Commit = override.Commit
	}
	if override.Branch != "" {
		b.Branch = override.Branch
	}
	if override.PRID != "" {
		b.PRID = override.PRID
	}
}

type envFunc func(key string) string
type gitFunc func(args ...string) (string, error)
type readFileFunc func(path string) ([]byte, error)

// detectBuild resolves build metadata from the CI environment — Bitbucket
// Pipelines or GitHub Actions variables — falling back to git for
// anything missing.
func detectBuild(env envFunc, git gitFunc, readFile readFileFunc) buildInfo {
	b := buildInfo{
		Repo:   env("BITBUCKET_REPO_FULL_NAME"),
		Commit: env("BITBUCKET_COMMIT"),
		Branch: env("BITBUCKET_BRANCH"),
		PRID:   env("BITBUCKET_PR_ID"),
	}
	b.fill(githubBuild(env, readFile))
	if b.Commit == "" {
		if out, err := git("rev-parse", "HEAD"); err == nil {
			b.Commit = out
		}
	}
	if b.Branch == "" {
		if out, err := git("rev-parse", "--abbrev-ref", "HEAD"); err == nil && out != "HEAD" {
			b.Branch = out
		}
	}
	if b.Repo == "" {
		if out, err := git("remote", "get-url", "origin"); err == nil {
			b.Repo = slugFromRemote(out)
		}
	}
	return b
}

// githubPRRefRe extracts the PR number from GITHUB_REF, which is
// "refs/pull/{n}/merge" for pull_request workflow runs.
var githubPRRefRe = regexp.MustCompile(`^refs/pull/(\d+)/`)

// githubBuild reads GitHub Actions environment variables. For
// pull_request events GITHUB_SHA and GITHUB_REF_NAME describe the
// throwaway merge commit ("42/merge"), which no status or comment can
// reach — the event payload's head SHA and branch, and GITHUB_HEAD_REF,
// take precedence for those runs.
func githubBuild(env envFunc, readFile readFileFunc) buildInfo {
	if env("GITHUB_ACTIONS") == "" {
		return buildInfo{}
	}
	b := buildInfo{
		Repo:   env("GITHUB_REPOSITORY"),
		Commit: env("GITHUB_SHA"),
		Branch: env("GITHUB_REF_NAME"),
	}
	if head := env("GITHUB_HEAD_REF"); head != "" {
		b.Branch = head
	}
	if m := githubPRRefRe.FindStringSubmatch(env("GITHUB_REF")); m != nil {
		b.PRID = m[1]
	}
	if path := env("GITHUB_EVENT_PATH"); path != "" && readFile != nil {
		if data, err := readFile(path); err == nil {
			var ev struct {
				PullRequest struct {
					Number int64 `json:"number"`
					Head   struct {
						SHA string `json:"sha"`
						Ref string `json:"ref"`
					} `json:"head"`
				} `json:"pull_request"`
			}
			if json.Unmarshal(data, &ev) == nil && ev.PullRequest.Head.SHA != "" {
				b.Commit = ev.PullRequest.Head.SHA
				if ev.PullRequest.Head.Ref != "" {
					b.Branch = ev.PullRequest.Head.Ref
				}
				if ev.PullRequest.Number > 0 {
					b.PRID = strconv.FormatInt(ev.PullRequest.Number, 10)
				}
			}
		}
	}
	return b
}

// fill copies non-empty fields from other into b's empty ones — the
// opposite precedence of merge, for stacking detection sources.
func (b *buildInfo) fill(other buildInfo) {
	if b.Repo == "" {
		b.Repo = other.Repo
	}
	if b.Commit == "" {
		b.Commit = other.Commit
	}
	if b.Branch == "" {
		b.Branch = other.Branch
	}
	if b.PRID == "" {
		b.PRID = other.PRID
	}
}

// remoteSlugRe extracts "workspace/repo" from SSH and HTTPS remote URLs:
// git@bitbucket.org:acme/widgets.git, https://bitbucket.org/acme/widgets.git,
// https://user@github.com/acme/widgets.
var remoteSlugRe = regexp.MustCompile(`[:/]([^/:]+/[^/:]+?)(?:\.git)?/?$`)

func slugFromRemote(remote string) string {
	m := remoteSlugRe.FindStringSubmatch(strings.TrimSpace(remote))
	if m == nil {
		return ""
	}
	return m[1]
}

// runGit is the production gitFunc.
func runGit(args ...string) (string, error) {
	out, err := exec.Command("git", args...).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// moduleFromGoMod reads the module path from a go.mod file, so the server
// can map module-qualified profile paths to repo-relative diff paths.
// Returns "" when the file is missing or has no module directive.
func moduleFromGoMod(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(line, "module"); ok {
			rest = strings.TrimSpace(rest)
			if rest == "" {
				continue
			}
			return strings.Trim(rest, `"`)
		}
	}
	return ""
}
