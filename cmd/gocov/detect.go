package main

import (
	"os"
	"os/exec"
	"regexp"
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

// detectBuild resolves build metadata from Bitbucket Pipelines environment
// variables, falling back to git for anything missing.
func detectBuild(env envFunc, git gitFunc) buildInfo {
	b := buildInfo{
		Repo:   env("BITBUCKET_REPO_FULL_NAME"),
		Commit: env("BITBUCKET_COMMIT"),
		Branch: env("BITBUCKET_BRANCH"),
		PRID:   env("BITBUCKET_PR_ID"),
	}
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

// remoteSlugRe extracts "workspace/repo" from SSH and HTTPS remote URLs:
// git@bitbucket.org:acme/widgets.git, https://bitbucket.org/acme/widgets.git,
// https://user@bitbucket.org/acme/widgets.
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
