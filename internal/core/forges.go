// The forge connections a workspace has, and keeping them alive.
//
// A repo reaches its forge through its workspace's one-click connection —
// a GitHub App installation, or a Bitbucket or GitLab grant — and never
// through credentials of its own. Resolving that client is most of what
// this file does; the rest is the upkeep it needs: refreshing a grant
// before its access token expires, persisting the rotated refresh token,
// caching the access token in memory (never in the store), and marking a
// connection broken when the forge says it is gone so the settings page
// can ask for a reconnect.

package core

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/gocov/gocov/internal/forge"
	"github.com/gocov/gocov/internal/forge/github"
	"github.com/gocov/gocov/internal/store"
)

// Forges owns the deployment's forge connectors and the live state that
// goes with them. The connectors are whatever the deployment configured;
// a nil one simply means that forge offers no one-click connect here.
type Forges struct {
	Store   store.Store
	Log     *slog.Logger
	BaseURL string

	GitHubApp GitHubApp
	// grants holds the configured grant-backed connectors (Bitbucket,
	// GitLab) by forge name, each with the token cache and workspace
	// columns that go with it.
	grants map[string]*grant
}

// NewForges wires the connectors together with the token caches they
// need. The caches live for the process: they hold access tokens only,
// so losing them costs one refresh, never a reconnect.
func NewForges(st store.Store, log *slog.Logger, baseURL string, app GitHubApp, bb BitbucketConnect, gl GitLabConnect) *Forges {
	f := &Forges{
		Store: st, Log: log, BaseURL: baseURL, GitHubApp: app,
		grants: map[string]*grant{},
	}
	if bb != nil {
		f.grants["bitbucket"] = &grant{
			forge:   "bitbucket",
			connect: bb,
			tokens:  newTokenCache(),
			refresh: bb.Refresh,
			state: func(ws *store.Workspace) grantState {
				return grantState{ws.BitbucketGrantAccount, ws.BitbucketRefreshToken, ws.BitbucketGrantBroken}
			},
			set: grantWriter.SetWorkspaceBitbucketGrant,
		}
	}
	if gl != nil {
		f.grants["gitlab"] = &grant{
			forge:   "gitlab",
			connect: gl,
			tokens:  newTokenCache(),
			refresh: func(ctx context.Context, refreshToken string) (*forge.Grant, error) {
				return gl.Refresh(ctx, refreshToken, RedirectURI(f.BaseURL, "gitlab"))
			},
			state: func(ws *store.Workspace) grantState {
				return grantState{ws.GitLabGrantAccount, ws.GitLabRefreshToken, ws.GitLabGrantBroken}
			},
			set: grantWriter.SetWorkspaceGitLabGrant,
		}
	}
	return f
}

// grant is one forge's workspace-connect grant as the upkeep code sees
// it. Bitbucket and GitLab differ only in the connector, the refresh
// call's shape, and the workspace columns the grant lives in; everything
// from refresh to revocation runs once through this table.
type grant struct {
	forge   string
	connect GrantConnect
	tokens  *tokenCache
	// refresh trades the stored refresh token for a fresh token set.
	refresh func(ctx context.Context, refreshToken string) (*forge.Grant, error)
	// state reads the grant's columns off a workspace row.
	state func(ws *store.Workspace) grantState
	// set writes them back — through the store, or a grant-lock
	// transaction.
	set func(w grantWriter, ctx context.Context, workspaceID int64, account, refreshToken string, broken bool) error
}

// grantState is a grant as stored on the workspace row.
type grantState struct {
	account      string
	refreshToken string
	broken       bool
}

// grantWriter is the slice of the store the grant upkeep writes through;
// store.Store and store.GrantTx both are one.
type grantWriter interface {
	SetWorkspaceBitbucketGrant(ctx context.Context, workspaceID int64, account, refreshToken string, broken bool) error
	SetWorkspaceGitLabGrant(ctx context.Context, workspaceID int64, account, refreshToken string, broken bool) error
}

// Connector returns the grant-backed connector for the forge — the
// consent and code-exchange half the connect handlers drive — or nil
// when the deployment offers no such grant.
func (f *Forges) Connector(forgeName string) GrantConnect {
	if g := f.grants[forgeName]; g != nil {
		return g.connect
	}
	return nil
}

// CacheGrantToken remembers the access token a fresh consent just
// handed out, so the first upload after connecting does not spend a
// refresh on a token we already hold. ttl is what the forge said.
func (f *Forges) CacheGrantToken(forgeName string, workspaceID int64, token string, ttl time.Duration) {
	if g := f.grants[forgeName]; g != nil {
		g.tokens.put(workspaceID, token, ttl)
	}
}

// DropGrantToken forgets a workspace's cached access token — on
// disconnect, so a cached token cannot outlive the grant behind it.
func (f *Forges) DropGrantToken(forgeName string, workspaceID int64) {
	if g := f.grants[forgeName]; g != nil {
		g.tokens.drop(workspaceID)
	}
}

// RedirectURI is the callback a forge must be configured with, and the
// one every OAuth call has to repeat verbatim — the forges match it
// exactly. Sign-in and workspace connect share it per forge.
func RedirectURI(baseURL, forgeName string) string {
	return strings.TrimSuffix(baseURL, "/") + "/oauth/" + forgeName + "/callback"
}

// GrantConnect is what every grant-backed connector shares: the consent
// page, the code exchange and the client an access token drives. The
// per-forge interfaces add Refresh in the shape their token endpoint
// wants.
type GrantConnect interface {
	// AuthorizeURL is the consent page for the connect grant.
	AuthorizeURL(state, redirectURI string) string
	// Exchange trades the consent code for the grant, including the
	// granting account's username.
	Exchange(ctx context.Context, code, redirectURI string) (*forge.Grant, error)
	// ForgeClient returns a forge client acting through the access token.
	ForgeClient(accessToken string) forge.Forge
}

// BitbucketConnect runs the Bitbucket OAuth grants for workspace
// connect. Errors wrapping forge.ErrCredentialsRevoked mean the grant
// is gone (revoked, or the refresh token aged out unused).
type BitbucketConnect interface {
	GrantConnect
	// Refresh trades a refresh token for a fresh access token and — the
	// tokens rotate — a new refresh token to persist.
	Refresh(ctx context.Context, refreshToken string) (*forge.Grant, error)
}

// GitLabConnect runs the GitLab OAuth grants for workspace connect —
// BitbucketConnect's twin (consent scope api). Errors wrapping
// forge.ErrCredentialsRevoked mean the grant is gone (revoked on the
// account's applications page).
type GitLabConnect interface {
	GrantConnect
	// Refresh trades a refresh token for a fresh access token and — the
	// tokens rotate — a new refresh token to persist. GitLab's token
	// endpoint wants the redirect URI on refreshes too.
	Refresh(ctx context.Context, refreshToken, redirectURI string) (*forge.Grant, error)
}

// GitHubApp mints installation-scoped forge clients and answers the two
// questions the connect flow needs. Errors wrapping
// forge.ErrCredentialsRevoked mean the installation (or the app's own
// credentials) no longer exists on GitHub.
type GitHubApp interface {
	// ForgeClient returns a forge client authenticated as the given
	// installation.
	ForgeClient(ctx context.Context, installationID int64) (forge.Forge, error)
	// InstallationAccount returns the login of the org or user account
	// the installation lives on.
	InstallationAccount(ctx context.Context, installationID int64) (string, error)
	// InstallURL is the app's public install page on GitHub.
	InstallURL(ctx context.Context) (string, error)
	// VerifyRunClaim checks a tokenless upload's claim against GitHub,
	// authenticated as the installation: repo public, workflow run real
	// and in progress, PR open at the claimed head. A
	// *github.ClaimRejectedError is a definitive verdict; any other
	// error is transient.
	VerifyRunClaim(ctx context.Context, installationID int64, claim github.RunClaim) error
}

// tokenLeeway retires cached access tokens before their 2h expiry.
const tokenLeeway = 5 * time.Minute

// tokenCache holds grant access tokens in memory — never the store.
// Refreshes are not serialized here: that lock has to hold across
// every instance sharing the database, so it is the store's
// WithGrantLock, not a mutex in this process.
type tokenCache struct {
	mu     sync.Mutex
	tokens map[int64]cachedToken
}

type cachedToken struct {
	value     string
	expiresAt time.Time
}

func newTokenCache() *tokenCache {
	return &tokenCache{tokens: map[int64]cachedToken{}}
}

func (c *tokenCache) get(workspaceID int64) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.tokens[workspaceID]
	if !ok || time.Until(t.expiresAt) < tokenLeeway {
		return "", false
	}
	return t.value, true
}

func (c *tokenCache) put(workspaceID int64, token string, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tokens[workspaceID] = cachedToken{value: token, expiresAt: time.Now().Add(ttl)}
}

func (c *tokenCache) drop(workspaceID int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.tokens, workspaceID)
}

// For builds a forge client for the repo through the workspace's
// one-click connection (GitHub App installation, Bitbucket grant or
// GitLab grant). Returns (nil, nil) when the repo's workspace has no
// connection — there is no manual-credential fallback.
func (f *Forges) For(ctx context.Context, repo *store.Repo) (forge.Forge, error) {
	// The workspace is looked up lazily: only when a connection could
	// apply, so a forge that supports no one-click connect skips the
	// query entirely.
	if f.Capable(repo.Forge) {
		ws := f.WorkspaceFor(ctx, repo.Slug, repo.Forge)
		if fg := f.Connected(ctx, ws, repo.Forge); fg != nil {
			return fg, nil
		}
	}
	return nil, nil
}

// Capable reports whether a one-click connection could supply
// credentials for the forge — the gate for the extra workspace lookup.
func (f *Forges) Capable(forgeName string) bool {
	return (f.GitHubApp != nil && forgeName == "github") || f.grants[forgeName] != nil
}

// Connected returns the workspace's one-click-connected client —
// GitHub App installation, Bitbucket grant or GitLab grant — or nil,
// the top link of the credential chain (D4/D7).
func (f *Forges) Connected(ctx context.Context, ws *store.Workspace, forgeName string) forge.Forge {
	if fg := f.installationForge(ctx, ws, forgeName); fg != nil {
		return fg
	}
	return f.grantForge(ctx, ws, forgeName)
}

// WorkspaceFor returns the workspace on the given forge owning the slug's
// prefix, nil when there is none. A lookup failure only degrades down
// the credential chain — forge surfaces are best-effort everywhere else
// too — so it is logged here and answered as "none"; a caller whose
// response depends on the difference uses LookupWorkspace.
func (f *Forges) WorkspaceFor(ctx context.Context, slug, forgeName string) *store.Workspace {
	ws, err := f.LookupWorkspace(ctx, slug, forgeName)
	if err != nil {
		f.Log.Error("workspace lookup", "repo", slug, "forge", forgeName, "err", err)
		return nil
	}
	return ws
}

// LookupWorkspace is WorkspaceFor with the store error kept: nil, nil when
// no workspace on the forge owns the slug. Prefixes are tried longest
// first, so a repo below a registered GitLab subgroup resolves to that
// subgroup's workspace, not a same-named ancestor. Workspace names are
// scoped per forge, so a same-named workspace on another forge is simply
// not consulted and cannot lend its secrets or its installation.
func (f *Forges) LookupWorkspace(ctx context.Context, slug, forgeName string) (*store.Workspace, error) {
	for _, prefix := range SlugPrefixes(slug) {
		ws, err := f.Store.WorkspaceByPrefix(ctx, forgeName, prefix)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		return ws, nil
	}
	return nil, nil
}

// installationForge returns the App-backed client when the workspace is
// connected to a GitHub App installation — the top link of the credential
// chain (D4), which also makes check runs first-class (the App is never
// hit by the classic-PAT 403). A refused mint marks the connection broken
// (lazy uninstall detection, D3) and returns nil, so the upload degrades
// exactly like missing credentials: skipped, never failed, with stored
// tokens still honored further down the chain. A transient mint failure
// only logs and falls through the same way.
func (f *Forges) installationForge(ctx context.Context, ws *store.Workspace, forgeName string) forge.Forge {
	if f.GitHubApp == nil || ws == nil || forgeName != "github" ||
		ws.Forge != "github" || ws.GitHubInstallationID == 0 {
		return nil
	}
	fg, err := f.GitHubApp.ForgeClient(ctx, ws.GitHubInstallationID)
	if err != nil {
		if errors.Is(err, forge.ErrCredentialsRevoked) {
			f.markAppBroken(ctx, ws, err)
		} else {
			f.Log.Warn("github app installation token", "workspace", ws.Prefix, "err", err)
		}
		return nil
	}
	if ws.GitHubAppBroken {
		// Minting works again (a lifted suspension, a restored key) —
		// heal the flag so settings stops asking for a reconnect.
		ws.GitHubAppBroken = false
		if err := f.Store.UpdateWorkspace(ctx, ws); err != nil {
			f.Log.Error("clearing github app broken flag", "workspace", ws.Prefix, "err", err)
		}
	}
	return fg
}

// VerifyGitHubRunClaim verifies a tokenless upload's workflow-run claim
// through the workspace's installation. Connection upkeep matches
// installationForge: a refused mint marks the connection broken (lazy
// uninstall detection, D3) — the caller still sees the error, because
// unlike a forge push, tokenless authentication cannot degrade.
func (f *Forges) VerifyGitHubRunClaim(ctx context.Context, ws *store.Workspace, claim github.RunClaim) error {
	err := f.GitHubApp.VerifyRunClaim(ctx, ws.GitHubInstallationID, claim)
	if errors.Is(err, forge.ErrCredentialsRevoked) {
		f.markAppBroken(ctx, ws, err)
	}
	return err
}

// markAppBroken records that the workspace's installation stopped
// working so the settings page can show "reconnect" (D3). The id is
// kept — only flagged — since a reinstall arrives through the setup
// redirect and overwrites it anyway.
func (f *Forges) markAppBroken(ctx context.Context, ws *store.Workspace, cause error) {
	f.Log.Warn("github app installation revoked", "workspace", ws.Prefix,
		"installation", ws.GitHubInstallationID, "err", cause)
	if ws.GitHubAppBroken {
		return
	}
	ws.GitHubAppBroken = true
	if err := f.Store.UpdateWorkspace(ctx, ws); err != nil {
		f.Log.Error("marking github app broken", "workspace", ws.Prefix, "err", err)
	}
}

// InstallURL resolves the app's public install page, best effort: a
// GitHub hiccup must not take a settings page down with it. Empty string
// when unavailable; templates then render the state without a link.
func (f *Forges) InstallURL(ctx context.Context) string {
	if f.GitHubApp == nil {
		return ""
	}
	u, err := f.GitHubApp.InstallURL(ctx)
	if err != nil {
		f.Log.Warn("github app install url", "err", err)
		return ""
	}
	return u
}

// grantForge returns the grant-backed client when the workspace is
// connected through a Bitbucket or GitLab grant — the top link of the
// credential chain (D4/D7). A revoked grant marks the connection broken
// (lazy detection, D7: the connecting member leaving revokes it) and
// returns nil, so the upload degrades exactly like missing credentials;
// transient trouble only logs and falls through the same way.
func (f *Forges) grantForge(ctx context.Context, ws *store.Workspace, forgeName string) forge.Forge {
	g := f.grants[forgeName]
	if g == nil || ws == nil || ws.Forge != forgeName || g.state(ws).account == "" {
		return nil
	}
	token, err := f.accessToken(ctx, g, ws)
	if err != nil {
		if errors.Is(err, forge.ErrCredentialsRevoked) {
			f.markGrantBroken(ctx, g, ws, err)
		} else {
			f.Log.Warn(g.forge+" grant token", "workspace", ws.Prefix, "err", err)
		}
		return nil
	}
	return g.connect.ForgeClient(token)
}

// accessToken returns a live access token for the workspace's grant,
// refreshing when the in-memory cache is empty or near expiry.
// Refreshes are serialized per workspace across every instance sharing
// the store (WithGrantLock) and re-read the stored refresh token under
// the lock, because every refresh rotates it: the rotated token is
// persisted (narrow UPDATE, broken flag cleared) before the access token
// is handed out.
func (f *Forges) accessToken(ctx context.Context, g *grant, ws *store.Workspace) (string, error) {
	if token, ok := g.tokens.get(ws.ID); ok {
		return token, nil
	}
	var token string
	err := f.Store.WithGrantLock(ctx, ws.ID, func(ctx context.Context, tx store.GrantTx) error {
		// A request in this process that held the lock before us has
		// filled the cache; one in another instance has rotated the
		// stored token, which is why it is re-read here.
		if t, ok := g.tokens.get(ws.ID); ok {
			token = t
			return nil
		}
		fresh, err := tx.WorkspaceByPrefix(ctx, ws.Forge, ws.Prefix)
		if err != nil {
			return err
		}
		stored := g.state(fresh)
		if stored.refreshToken == "" {
			// Disconnected under our feet, or the stored token could not
			// be decrypted (rotated GOCOV_SECRET_KEY) — either way a
			// reconnect is the fix.
			return fmt.Errorf("%w: workspace %s has no usable grant", forge.ErrCredentialsRevoked, ws.Prefix)
		}
		got, err := g.refresh(ctx, stored.refreshToken)
		if err != nil {
			return err
		}
		// Defensive: a non-rotating answer keeps the stored token.
		newRefresh := cmp.Or(got.RefreshToken, stored.refreshToken)
		if err := g.set(tx, ctx, ws.ID, stored.account, newRefresh, false); err != nil {
			// The old token is already invalidated by the rotation;
			// losing the new one breaks the next refresh, not this
			// upload — loud log so the operator sees it before the 2h
			// cache runs out.
			f.Log.Error("persisting rotated "+g.forge+" refresh token", "workspace", ws.Prefix, "err", err)
		}
		g.tokens.put(ws.ID, got.AccessToken, got.TTL)
		token = got.AccessToken
		return nil
	})
	return token, err
}

// markGrantBroken records the revoked grant so the settings page shows
// "reconnect" (D7). The account name is kept — it says who to replace.
func (f *Forges) markGrantBroken(ctx context.Context, g *grant, ws *store.Workspace, cause error) {
	stored := g.state(ws)
	f.Log.Warn(g.forge+" grant revoked", "workspace", ws.Prefix,
		"account", stored.account, "err", cause)
	if stored.broken {
		return
	}
	if err := g.set(f.Store, ctx, ws.ID, stored.account, stored.refreshToken, true); err != nil {
		f.Log.Error("marking "+g.forge+" grant broken", "workspace", ws.Prefix, "err", err)
	}
}

// SetInstallationBroken marks every workspace on the given installation
// broken (or healed). No store index exists on the installation id, but
// the workspace set is small.
func (f *Forges) SetInstallationBroken(ctx context.Context, installationID int64, broken bool) {
	all, err := f.Store.ListWorkspaces(ctx)
	if err != nil {
		f.Log.Error("github webhook: listing workspaces", "err", err)
		return
	}
	for _, ws := range all {
		if ws.GitHubInstallationID != installationID || ws.GitHubAppBroken == broken {
			continue
		}
		ws.GitHubAppBroken = broken
		if err := f.Store.UpdateWorkspace(ctx, ws); err != nil {
			f.Log.Error("github webhook: updating workspace", "workspace", ws.Prefix, "err", err)
			continue
		}
		f.Log.Info("github installation flag set via webhook",
			"workspace", ws.Prefix, "installation", installationID, "broken", broken)
	}
}
