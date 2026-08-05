// Package bitbucket implements auth.Provider for Bitbucket Cloud using
// only the standard library. The OAuth consumer needs the read-only
// "account" and "email" scopes, nothing broader.
package bitbucket

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/bykclk/gocov/internal/auth"
)

// Default endpoints. The OAuth pages live on the website host, the
// identity endpoints on the API host.
const (
	DefaultAuthBaseURL = "https://bitbucket.org/site/oauth2"
	DefaultAPIBaseURL  = "https://api.bitbucket.org/2.0"
)

// maxWorkspacePages bounds the membership listing; nobody authorized to
// sign in is plausibly beyond 10 pages of 100 workspaces.
const maxWorkspacePages = 10

// Provider is a Bitbucket OAuth consumer. Key and Secret are required;
// the base URLs default to the public Bitbucket Cloud endpoints and exist
// as fields so tests can point them at a local server.
type Provider struct {
	Key         string
	Secret      string
	AuthBaseURL string
	APIBaseURL  string
	HTTPClient  *http.Client
}

// New builds a Provider for the public Bitbucket Cloud.
func New(key, secret string) *Provider {
	return &Provider{
		Key:        key,
		Secret:     secret,
		HTTPClient: &http.Client{Timeout: 15 * time.Second},
	}
}

func (p *Provider) Name() string { return "bitbucket" }

func (p *Provider) authBase() string {
	if p.AuthBaseURL != "" {
		return p.AuthBaseURL
	}
	return DefaultAuthBaseURL
}

func (p *Provider) apiBase() string {
	if p.APIBaseURL != "" {
		return p.APIBaseURL
	}
	return DefaultAPIBaseURL
}

func (p *Provider) client() *http.Client {
	if p.HTTPClient != nil {
		return p.HTTPClient
	}
	return &http.Client{Timeout: 15 * time.Second}
}

func (p *Provider) AuthorizeURL(state, redirectURI string) string {
	q := url.Values{
		"client_id":     {p.Key},
		"response_type": {"code"},
		"state":         {state},
		"redirect_uri":  {redirectURI},
	}
	return p.authBase() + "/authorize?" + q.Encode()
}

func (p *Provider) Identity(ctx context.Context, code, redirectURI string) (*auth.Identity, error) {
	token, err := p.exchange(ctx, code, redirectURI)
	if err != nil {
		return nil, err
	}
	// The token lives only in this frame; it is never persisted.
	var user struct {
		UUID        string `json:"uuid"`
		DisplayName string `json:"display_name"`
	}
	if err := p.get(ctx, token, "/user", &user); err != nil {
		return nil, err
	}
	if user.UUID == "" {
		return nil, fmt.Errorf("bitbucket: /user returned no uuid")
	}
	id := &auth.Identity{ForgeUUID: user.UUID, DisplayName: user.DisplayName}
	id.Email, err = p.primaryEmail(ctx, token)
	if err != nil {
		return nil, err
	}
	id.Workspaces, err = p.workspaces(ctx, token)
	if err != nil {
		return nil, err
	}
	return id, nil
}

// exchange trades the authorization code for an access token.
func (p *Provider) exchange(ctx context.Context, code, redirectURI string) (string, error) {
	form := url.Values{
		"grant_type":   {"authorization_code"},
		"code":         {code},
		"redirect_uri": {redirectURI},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		p.authBase()+"/access_token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(p.Key, p.Secret)
	resp, err := p.client().Do(req)
	if err != nil {
		return "", fmt.Errorf("bitbucket: token exchange: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("bitbucket: token exchange: status %d: %s", resp.StatusCode, body)
	}
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tok); err != nil {
		return "", fmt.Errorf("bitbucket: token exchange: %w", err)
	}
	if tok.AccessToken == "" {
		return "", fmt.Errorf("bitbucket: token exchange returned no access token")
	}
	return tok.AccessToken, nil
}

// primaryEmail returns the account's primary email address, or "" when
// none is confirmed ("email" scope, GET /user/emails).
func (p *Provider) primaryEmail(ctx context.Context, token string) (string, error) {
	var page struct {
		Values []struct {
			Email     string `json:"email"`
			IsPrimary bool   `json:"is_primary"`
		} `json:"values"`
	}
	if err := p.get(ctx, token, "/user/emails", &page); err != nil {
		return "", err
	}
	for _, v := range page.Values {
		if v.IsPrimary {
			return v.Email, nil
		}
	}
	if len(page.Values) > 0 {
		return page.Values[0].Email, nil
	}
	return "", nil
}

// workspaces lists the slugs of the workspaces the account is a member of
// ("account" scope, GET /user/workspaces), following pagination. This is
// the CHANGE-3022 replacement API; the older GET /workspaces and
// GET /user/permissions/workspaces listings were sunset by CHANGE-2770
// and answer 410.
func (p *Provider) workspaces(ctx context.Context, token string) ([]string, error) {
	var out []string
	next := p.apiBase() + "/user/workspaces?" + url.Values{"pagelen": {"100"}}.Encode()
	for range maxWorkspacePages {
		var page struct {
			Values []struct {
				Workspace struct {
					Slug string `json:"slug"`
				} `json:"workspace"`
			} `json:"values"`
			Next string `json:"next"`
		}
		if err := p.getURL(ctx, token, next, &page); err != nil {
			return nil, err
		}
		for _, v := range page.Values {
			if v.Workspace.Slug != "" {
				out = append(out, v.Workspace.Slug)
			}
		}
		if page.Next == "" {
			return out, nil
		}
		next = page.Next
	}
	return out, nil
}

func (p *Provider) get(ctx context.Context, token, path string, dst any) error {
	return p.getURL(ctx, token, p.apiBase()+path, dst)
}

func (p *Provider) getURL(ctx context.Context, token, u string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := p.client().Do(req)
	if err != nil {
		return fmt.Errorf("bitbucket: GET %s: %w", u, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("bitbucket: GET %s: status %d: %s", u, resp.StatusCode, body)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(dst); err != nil {
		return fmt.Errorf("bitbucket: GET %s: %w", u, err)
	}
	return nil
}

// ensure interface compliance
var _ auth.Provider = (*Provider)(nil)
