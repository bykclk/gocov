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
	"strconv"
	"strings"
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
	return c.send(ctx, http.MethodPost, path, body)
}

// PostPRComment adds a comment via
// POST /repositories/{slug}/pullrequests/{id}/comments.
func (c *Client) PostPRComment(ctx context.Context, repoSlug, prID, body string) error {
	payload := map[string]any{
		"content": map[string]string{"raw": body},
	}
	path := fmt.Sprintf("/repositories/%s/pullrequests/%s/comments",
		repoSlug, url.PathEscape(prID))
	return c.send(ctx, http.MethodPost, path, payload)
}

// maxCommentPages bounds pagination when searching for an earlier comment.
const maxCommentPages = 10

// bitbucketUser identifies an account; either field may be empty depending
// on the credential type.
type bitbucketUser struct {
	AccountID string `json:"account_id"`
	UUID      string `json:"uuid"`
}

func (u bitbucketUser) is(other bitbucketUser) bool {
	if u.AccountID != "" && u.AccountID == other.AccountID {
		return true
	}
	return u.UUID != "" && u.UUID == other.UUID
}

// currentUser resolves the authenticated account via GET /user.
func (c *Client) currentUser(ctx context.Context) (bitbucketUser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/user", nil)
	if err != nil {
		return bitbucketUser{}, err
	}
	req.SetBasicAuth(c.Username, c.AppPassword)
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return bitbucketUser{}, fmt.Errorf("bitbucket: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return bitbucketUser{}, fmt.Errorf("bitbucket: /user returned %d: %s", resp.StatusCode, msg)
	}
	var u bitbucketUser
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&u); err != nil {
		return bitbucketUser{}, fmt.Errorf("bitbucket: decoding /user: %w", err)
	}
	if u.AccountID == "" && u.UUID == "" {
		return bitbucketUser{}, fmt.Errorf("bitbucket: /user returned no account identity")
	}
	return u, nil
}

// FindPRComment returns the id of the newest top-level PR comment that was
// authored by the credential account and starts with prefix. Comments are
// requested newest first, so the page cap only bounds how far back the
// search goes; replies, inline comments and other authors never match —
// a stranger's "**gocov**" comment must not be able to capture the slot.
func (c *Client) FindPRComment(ctx context.Context, repoSlug, prID, prefix string) (string, error) {
	self, err := c.currentUser(ctx)
	if err != nil {
		return "", err
	}
	next := fmt.Sprintf("%s/repositories/%s/pullrequests/%s/comments?pagelen=100&sort=-created_on",
		c.BaseURL, repoSlug, url.PathEscape(prID))
	for page := 0; next != "" && page < maxCommentPages; page++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, next, nil)
		if err != nil {
			return "", err
		}
		req.SetBasicAuth(c.Username, c.AppPassword)
		resp, err := c.HTTPClient.Do(req)
		if err != nil {
			return "", fmt.Errorf("bitbucket: %w", err)
		}
		var body struct {
			Values []struct {
				ID      int64 `json:"id"`
				Deleted bool  `json:"deleted"`
				Inline  *struct {
					Path string `json:"path"`
				} `json:"inline"`
				Parent *struct {
					ID int64 `json:"id"`
				} `json:"parent"`
				User    bitbucketUser `json:"user"`
				Content struct {
					Raw string `json:"raw"`
				} `json:"content"`
			} `json:"values"`
			Next string `json:"next"`
		}
		if resp.StatusCode >= 300 {
			msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			return "", fmt.Errorf("bitbucket: listing PR comments returned %d: %s", resp.StatusCode, msg)
		}
		err = json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&body)
		resp.Body.Close()
		if err != nil {
			return "", fmt.Errorf("bitbucket: decoding PR comments: %w", err)
		}
		for _, v := range body.Values {
			if v.Deleted || v.Inline != nil || v.Parent != nil || !self.is(v.User) {
				continue
			}
			if strings.HasPrefix(v.Content.Raw, prefix) {
				return strconv.FormatInt(v.ID, 10), nil
			}
		}
		next = body.Next
	}
	return "", nil
}

// UpdatePRComment replaces a comment's body via
// PUT /repositories/{slug}/pullrequests/{id}/comments/{comment_id}.
func (c *Client) UpdatePRComment(ctx context.Context, repoSlug, prID, commentID, body string) error {
	payload := map[string]any{
		"content": map[string]string{"raw": body},
	}
	path := fmt.Sprintf("/repositories/%s/pullrequests/%s/comments/%s",
		repoSlug, url.PathEscape(prID), url.PathEscape(commentID))
	return c.send(ctx, http.MethodPut, path, payload)
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

// maxFileBytes bounds source files fetched for the source view.
const maxFileBytes = 2 << 20

// GetFileContent reads a file at a commit via
// GET /repositories/{slug}/src/{commit}/{path}.
func (c *Client) GetFileContent(ctx context.Context, repoSlug, commitSHA, path string) ([]byte, error) {
	segments := strings.Split(path, "/")
	for i, s := range segments {
		segments[i] = url.PathEscape(s)
	}
	reqURL := fmt.Sprintf("%s/repositories/%s/src/%s/%s",
		c.BaseURL, repoSlug, url.PathEscape(commitSHA), strings.Join(segments, "/"))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(c.Username, c.AppPassword)

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("bitbucket: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%w: %s at %s", forge.ErrRepoNotFound, path, commitSHA)
	}
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("bitbucket: reading %s returned %d: %s", path, resp.StatusCode, msg)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("bitbucket: reading %s: %w", path, err)
	}
	if len(data) > maxFileBytes {
		return nil, fmt.Errorf("bitbucket: %s is larger than %d MiB", path, maxFileBytes>>20)
	}
	return data, nil
}

// reportID is the stable Code Insights report id: one report per commit,
// so repeated uploads replace instead of stacking.
const reportID = "gocov"

var reportResults = map[string]string{
	forge.ReportPassed: "PASSED",
	forge.ReportFailed: "FAILED",
}

var reportDataTypes = map[string]string{
	forge.DataPercentage: "PERCENTAGE",
	forge.DataNumber:     "NUMBER",
	forge.DataText:       "TEXT",
}

// PublishReport writes a Code Insights report onto a commit via
// PUT /repositories/{slug}/commit/{sha}/reports/gocov, then bulk-uploads
// the annotations. The report is deleted first: annotation bulk upload
// only upserts by external_id, so without the delete a re-upload with
// fewer findings would leave stale annotations in the PR diff.
func (c *Client) PublishReport(ctx context.Context, repoSlug, commitSHA string, report forge.Report, annotations []forge.Annotation) error {
	base := fmt.Sprintf("/repositories/%s/commit/%s/reports/%s",
		repoSlug, url.PathEscape(commitSHA), reportID)
	if err := c.deleteReport(ctx, base); err != nil {
		return err
	}

	payload := map[string]any{
		"title":       report.Title,
		"details":     report.Details,
		"report_type": "COVERAGE",
		"reporter":    "gocov",
	}
	if report.Link != "" {
		payload["link"] = report.Link
	}
	if report.Result != "" {
		result, ok := reportResults[report.Result]
		if !ok {
			return fmt.Errorf("bitbucket: unknown report result %q", report.Result)
		}
		payload["result"] = result
	}
	data := make([]map[string]any, 0, len(report.Data))
	for _, d := range report.Data {
		typeName, ok := reportDataTypes[d.Type]
		if !ok {
			return fmt.Errorf("bitbucket: unknown report data type %q", d.Type)
		}
		data = append(data, map[string]any{
			"title": d.Title,
			"type":  typeName,
			"value": d.Value,
		})
	}
	payload["data"] = data
	if err := c.send(ctx, http.MethodPut, base, payload); err != nil {
		return err
	}

	if len(annotations) == 0 {
		return nil
	}
	// One bulk request; the API accepts at most 100 annotations per POST
	// and the server never sends more.
	items := make([]map[string]any, 0, len(annotations))
	for i, a := range annotations {
		// An untested line is a smell, not an incident: CODE_SMELL at
		// MEDIUM, never the vulnerability/critical tier.
		items = append(items, map[string]any{
			"external_id":     fmt.Sprintf("gocov-%03d", i+1),
			"annotation_type": "CODE_SMELL",
			"severity":        "MEDIUM",
			"path":            a.Path,
			"line":            a.Line,
			"summary":         a.Summary,
		})
	}
	return c.send(ctx, http.MethodPost, base+"/annotations", items)
}

// deleteReport removes the commit's existing gocov report and, with it,
// its annotations. A 404 just means there is nothing to delete yet.
func (c *Client) deleteReport(ctx context.Context, path string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.BaseURL+path, nil)
	if err != nil {
		return err
	}
	req.SetBasicAuth(c.Username, c.AppPassword)
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("bitbucket: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode != http.StatusNotFound {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("bitbucket: deleting %s returned %d: %s", path, resp.StatusCode, msg)
	}
	return nil
}

func (c *Client) send(ctx context.Context, method, path string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, bytes.NewReader(data))
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
