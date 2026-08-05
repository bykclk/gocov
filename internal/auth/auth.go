// Package auth defines the sign-in provider seam for the web UI. Each
// forge's OAuth specifics (authorize URL, token exchange, identity and
// membership fetch) live behind Provider, so adding a provider is an
// addition, not a refactor.
package auth

import "context"

// Identity describes the account that completed a sign-in.
type Identity struct {
	// ForgeUUID is the forge's stable account identifier.
	ForgeUUID   string
	DisplayName string
	Email       string
	// Workspaces are the slugs of the workspaces the account is a member
	// of, used for the login-time authorization check.
	Workspaces []string
}

// Provider implements one forge's OAuth 2.0 authorization-code flow.
type Provider interface {
	// Name is the forge name, e.g. "bitbucket"; it doubles as the URL
	// segment of the login routes.
	Name() string
	// AuthorizeURL is the consent-screen URL the login button redirects to.
	AuthorizeURL(state, redirectURI string) string
	// Identity exchanges the callback code for the account's identity and
	// workspace memberships. The provider's access token stays internal to
	// this call and is discarded afterwards (an M1 decision — a future
	// milestone may add a way to retain tokens).
	Identity(ctx context.Context, code, redirectURI string) (*Identity, error)
}
