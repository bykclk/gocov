// Package github implements auth.Provider for GitHub (cloud github.com)
// using only the standard library. The OAuth app needs the read-only
// "read:org" and "user:email" scopes, nothing broader.
package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/bykclk/gocov/internal/auth"
)

// Default endpoints. The OAuth pages live on the website host, the
// identity endpoints on the API host.
const (
	DefaultAuthBaseURL = "https://github.com/login/oauth"
	DefaultAPIBaseURL  = "https://api.github.com"
)

// maxOrgPages bounds the membership listing; nobody authorized to sign
// in is plausibly beyond 10 pages of 100 organizations.
const maxOrgPages = 10

// Provider is a GitHub OAuth app. Key (client id) and Secret are
// required; the base URLs default to the public github.com endpoints and
// exist as fields so tests can point them at a local server.
type Provider struct {
	Key         string
	Secret      string
	AuthBaseURL string
	APIBaseURL  string
	HTTPClient  *http.Client
}

// New builds a Provider for the public github.com.
func New(key, secret string) *Provider {
	return &Provider{
		Key:        key,
		Secret:     secret,
		HTTPClient: &http.Client{Timeout: 15 * time.Second},
	}
}

func (p *Provider) Name() string { return "github" }

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
		"client_id":    {p.Key},
		"state":        {state},
		"redirect_uri": {redirectURI},
		// read:org exposes the account's full org membership (private
		// memberships included) for the login-time authorization check;
		// user:email covers accounts whose email is private.
		"scope": {"read:org user:email"},
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
		ID    int64  `json:"id"`
		Login string `json:"login"`
		Name  string `json:"name"`
		Email string `json:"email"`
	}
	if err := p.get(ctx, token, "/user", &user); err != nil {
		return nil, err
	}
	if user.ID == 0 {
		return nil, fmt.Errorf("github: /user returned no account id")
	}
	// The numeric account id is the stable identifier; logins can be
	// renamed and released to others.
	id := &auth.Identity{
		ForgeUUID:   strconv.FormatInt(user.ID, 10),
		DisplayName: user.Name,
		Email:       user.Email,
	}
	if id.DisplayName == "" {
		id.DisplayName = user.Login
	}
	if id.Email == "" {
		id.Email, err = p.primaryEmail(ctx, token)
		if err != nil {
			return nil, err
		}
	}
	orgs, err := p.orgs(ctx, token)
	if err != nil {
		return nil, err
	}
	// The login counts as a workspace of its own, so repos under the
	// user's personal namespace admit their owner.
	id.Workspaces = append(orgs, user.Login)
	return id, nil
}

// exchange trades the authorization code for an access token.
func (p *Provider) exchange(ctx context.Context, code, redirectURI string) (string, error) {
	form := url.Values{
		"client_id":     {p.Key},
		"client_secret": {p.Secret},
		"code":          {code},
		"redirect_uri":  {redirectURI},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		p.authBase()+"/access_token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// Without this GitHub answers in the legacy query-string format.
	req.Header.Set("Accept", "application/json")
	resp, err := p.client().Do(req)
	if err != nil {
		return "", fmt.Errorf("github: token exchange: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("github: token exchange: status %d: %s", resp.StatusCode, body)
	}
	// A bad code still comes back 200, with the error in the body.
	var tok struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tok); err != nil {
		return "", fmt.Errorf("github: token exchange: %w", err)
	}
	if tok.Error != "" {
		return "", fmt.Errorf("github: token exchange: %s", tok.Error)
	}
	if tok.AccessToken == "" {
		return "", fmt.Errorf("github: token exchange returned no access token")
	}
	return tok.AccessToken, nil
}

// primaryEmail returns the account's primary email address, or "" when
// there is none ("user:email" scope, GET /user/emails).
func (p *Provider) primaryEmail(ctx context.Context, token string) (string, error) {
	var emails []struct {
		Email   string `json:"email"`
		Primary bool   `json:"primary"`
	}
	if err := p.get(ctx, token, "/user/emails?per_page=100", &emails); err != nil {
		return "", err
	}
	for _, e := range emails {
		if e.Primary {
			return e.Email, nil
		}
	}
	if len(emails) > 0 {
		return emails[0].Email, nil
	}
	return "", nil
}

// orgs lists the login slugs of the organizations the account belongs to
// ("read:org" scope, GET /user/orgs), following Link-header pagination.
func (p *Provider) orgs(ctx context.Context, token string) ([]string, error) {
	var out []string
	next := p.apiBase() + "/user/orgs?per_page=100"
	for range maxOrgPages {
		var page []struct {
			Login string `json:"login"`
		}
		link, err := p.getURL(ctx, token, next, &page)
		if err != nil {
			return nil, err
		}
		for _, o := range page {
			if o.Login != "" {
				out = append(out, o.Login)
			}
		}
		next = nextLink(link)
		if next == "" {
			return out, nil
		}
	}
	return out, nil
}

// nextLink extracts the rel="next" URL from a Link response header, or ""
// when there is no next page.
func nextLink(header string) string {
	for _, part := range strings.Split(header, ",") {
		u, rel, ok := strings.Cut(part, ";")
		if !ok || !strings.Contains(rel, `rel="next"`) {
			continue
		}
		u = strings.TrimSpace(u)
		if strings.HasPrefix(u, "<") && strings.HasSuffix(u, ">") {
			return u[1 : len(u)-1]
		}
	}
	return ""
}

func (p *Provider) get(ctx context.Context, token, path string, dst any) error {
	_, err := p.getURL(ctx, token, p.apiBase()+path, dst)
	return err
}

// getURL fetches u into dst and returns the response's Link header for
// pagination.
func (p *Provider) getURL(ctx context.Context, token, u string, dst any) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := p.client().Do(req)
	if err != nil {
		return "", fmt.Errorf("github: GET %s: %w", u, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("github: GET %s: status %d: %s", u, resp.StatusCode, body)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(dst); err != nil {
		return "", fmt.Errorf("github: GET %s: %w", u, err)
	}
	return resp.Header.Get("Link"), nil
}

// ensure interface compliance
var _ auth.Provider = (*Provider)(nil)
