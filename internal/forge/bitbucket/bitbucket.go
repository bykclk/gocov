// Package bitbucket implements forge.Forge for Bitbucket Cloud.
package bitbucket

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/bykclk/gocov/internal/forge"
)

// DefaultBaseURL is the Bitbucket Cloud REST API root.
const DefaultBaseURL = "https://api.bitbucket.org/2.0"

// Client implements forge.Forge against the Bitbucket Cloud API using an
// app password for authentication.
type Client struct {
	BaseURL     string
	Username    string
	AppPassword string
	HTTPClient  *http.Client
}

// Factory builds a Client from repo credentials. Required keys:
// "username" and "app_password".
func Factory(creds map[string]string) (forge.Forge, error) {
	username, password := creds["username"], creds["app_password"]
	if username == "" || password == "" {
		return nil, fmt.Errorf("bitbucket: credentials must include username and app_password")
	}
	return &Client{
		BaseURL:     DefaultBaseURL,
		Username:    username,
		AppPassword: password,
		HTTPClient:  &http.Client{Timeout: 15 * time.Second},
	}, nil
}

var stateNames = map[string]string{
	forge.StateSuccessful: "SUCCESSFUL",
	forge.StateFailed:     "FAILED",
	forge.StateInProgress: "INPROGRESS",
}

// PostBuildStatus writes a build status onto a commit via
// POST /repositories/{slug}/commit/{sha}/statuses/build.
func (c *Client) PostBuildStatus(ctx context.Context, repoSlug, commitSHA string, status forge.BuildStatus) error {
	state, ok := stateNames[status.State]
	if !ok {
		return fmt.Errorf("bitbucket: unknown build status state %q", status.State)
	}
	body := map[string]string{
		"key":         status.Key,
		"state":       state,
		"name":        status.Name,
		"description": status.Description,
		"url":         status.URL,
	}
	path := fmt.Sprintf("/repositories/%s/commit/%s/statuses/build",
		repoSlug, url.PathEscape(commitSHA))
	return c.post(ctx, path, body)
}

// PostPRComment adds a comment via
// POST /repositories/{slug}/pullrequests/{id}/comments.
func (c *Client) PostPRComment(ctx context.Context, repoSlug, prID, body string) error {
	payload := map[string]any{
		"content": map[string]string{"raw": body},
	}
	path := fmt.Sprintf("/repositories/%s/pullrequests/%s/comments",
		repoSlug, url.PathEscape(prID))
	return c.post(ctx, path, payload)
}

// GetPRDiff fetches the unified diff of a pull request via
// GET /repositories/{slug}/pullrequests/{id}/diff. Bitbucket answers with a
// redirect to the diff blob, which the HTTP client follows transparently.
func (c *Client) GetPRDiff(ctx context.Context, repoSlug, prID string) (string, error) {
	path := fmt.Sprintf("/repositories/%s/pullrequests/%s/diff",
		repoSlug, url.PathEscape(prID))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return "", err
	}
	req.SetBasicAuth(c.Username, c.AppPassword)

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("bitbucket: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("bitbucket: %s returned %d: %s", path, resp.StatusCode, msg)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxDiffBytes+1))
	if err != nil {
		return "", fmt.Errorf("bitbucket: reading diff: %w", err)
	}
	if len(body) > maxDiffBytes {
		// A truncated diff would silently produce wrong coverage numbers.
		return "", fmt.Errorf("bitbucket: PR diff larger than %d MiB", maxDiffBytes>>20)
	}
	return string(body), nil
}

// maxDiffBytes bounds PR diffs; larger diffs error instead of truncating.
const maxDiffBytes = 32 << 20

// GetDefaultBranch reads the repo's main branch via GET /repositories/{slug}.
func (c *Client) GetDefaultBranch(ctx context.Context, repoSlug string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/repositories/"+repoSlug, nil)
	if err != nil {
		return "", err
	}
	req.SetBasicAuth(c.Username, c.AppPassword)

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("bitbucket: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", fmt.Errorf("%w: %s", forge.ErrRepoNotFound, repoSlug)
	}
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("bitbucket: /repositories/%s returned %d: %s", repoSlug, resp.StatusCode, msg)
	}
	var body struct {
		MainBranch struct {
			Name string `json:"name"`
		} `json:"mainbranch"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return "", fmt.Errorf("bitbucket: decoding repository: %w", err)
	}
	if body.MainBranch.Name == "" {
		return "", fmt.Errorf("bitbucket: repository %s has no main branch", repoSlug)
	}
	return body.MainBranch.Name, nil
}

func (c *Client) post(ctx context.Context, path string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.SetBasicAuth(c.Username, c.AppPassword)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("bitbucket: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("bitbucket: %s returned %d: %s", path, resp.StatusCode, msg)
	}
	return nil
}

var _ forge.Forge = (*Client)(nil)
