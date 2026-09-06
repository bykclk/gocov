package config

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// Server modes. Private keeps sign-in limited to members of tracked
// workspaces; hosted opens self-service registration to any forge account.
const (
	ModePrivate = "private"
	ModeHosted  = "hosted"
)

// Server is gocov-server's configuration.
type Server struct {
	// required,notEmpty: the two together reject both an unset variable
	// and one passed through as "", which compose files and CI runners do
	// routinely. required alone would let the empty string past.
	DatabaseURL string `env:"DATABASE_URL,required,notEmpty"`
	Addr        string `env:"GOCOV_ADDR" envDefault:":8080"`
	BaseURL     string `env:"GOCOV_BASE_URL" envDefault:"http://localhost:8080"`
	Mode        string `env:"GOCOV_MODE" envDefault:"private"`

	// SecretKey is the at-rest cipher key for stored grant refresh
	// tokens; see Validate for why its shape is not negotiable.
	SecretKey string `env:"GOCOV_SECRET_KEY"`

	// AllowedWorkspaces optionally narrows which workspace/org members
	// may sign in. Empty means "members of any tracked workspace".
	AllowedWorkspaces []string `env:"GOCOV_ALLOWED_WORKSPACES" envSeparator:","`

	// PublicReports is the instance-level switch for anonymous read-only
	// report pages on public repos: "on" (default) or "off". An operator
	// running gocov inside a private network turns the whole feature off
	// in one move; per-repo control stays in repo settings.
	PublicReports string `env:"GOCOV_PUBLIC_REPORTS" envDefault:"on"`

	// OIDCIssuers sets the trusted GitLab OIDC issuer(s) for tokenless
	// uploads. Unset means gitlab.com; set replaces that default with the
	// listed self-managed GitLab instance issuer(s) — because a gocov
	// deployment connects to one GitLab, and trusting gitlab.com and a
	// self-managed instance at once would let a token from either
	// authenticate an upload to a same-named project on the other. Each
	// entry is the instance's issuer URL (its base URL, https), whose CI ID
	// tokens name repos by project_path exactly as gitlab.com's do. GitHub
	// Actions and Bitbucket Pipelines are always trusted, independently.
	OIDCIssuers []string `env:"GOCOV_OIDC_ISSUERS" envSeparator:","`

	Bitbucket OAuthApp `envPrefix:"GOCOV_OAUTH_BITBUCKET_"`
	GitHub    OAuthApp `envPrefix:"GOCOV_OAUTH_GITHUB_"`
	GitLab    OAuthApp `envPrefix:"GOCOV_OAUTH_GITLAB_"`

	// GitHubAppPrivateKey holds either the PEM itself or a path to a PEM
	// file; the caller reads the file when it is a path.
	GitHubAppID         string `env:"GOCOV_GITHUB_APP_ID"`
	GitHubAppPrivateKey string `env:"GOCOV_GITHUB_APP_PRIVATE_KEY"`

	GitHubWebhookSecret string `env:"GOCOV_GITHUB_WEBHOOK_SECRET"`

	// PostHog is the opt-in browser analytics for the web UI. Unset (the
	// default) keeps the pages free of third-party scripts, which is the
	// promise docs/self-hosting.md makes to operators; gocov's own hosted
	// instance sets it.
	PostHog PostHog `envPrefix:"GOCOV_POSTHOG_"`
}

// PostHog is the browser-analytics pair: the project API key that turns
// the web UI snippet on, and the ingestion host it reports to.
type PostHog struct {
	Key string `env:"KEY"`
	// Host is the PostHog ingestion endpoint the browser loads the
	// snippet from and sends events to. The EU cloud is the default so
	// that switching analytics on never moves visitor data out of the EU
	// by accident; a self-hosted PostHog goes here too.
	Host string `env:"HOST" envDefault:"https://eu.i.posthog.com"`
}

// Configured reports whether the analytics snippet is on. The host has a
// default, so the key alone is the switch.
func (p PostHog) Configured() bool { return p.Key != "" }

// LoadServer reads and validates the server configuration from the
// process environment.
func LoadServer() (Server, error) { return LoadServerFrom(nil) }

// LoadServerFrom reads the server configuration from an explicit
// environment map. A nil map means the process environment; a non-nil
// one is used as given, which is how the tests supply an environment.
//
// A validation failure still returns the parsed config alongside the
// error, because its Warnings remain worth reporting: an operator whose
// GOCOV_MODE is a typo should hear about a half-configured OAuth pair in
// the same boot, not after fixing the first problem and restarting. The
// config is not usable in that state — callers must check the error
// before wiring anything up, and may use only Warnings until they have.
func LoadServerFrom(environ map[string]string) (Server, error) {
	cfg, err := parse[Server](environ)
	if err != nil {
		// Nothing parsed, so there is nothing to warn about either.
		return Server{}, err
	}
	cfg.normalize()
	return cfg, cfg.validate()
}

// normalize trims the values where surrounding whitespace is a paste
// artefact rather than part of the setting.
func (c *Server) normalize() {
	c.SecretKey = strings.TrimSpace(c.SecretKey)
	c.Mode = strings.TrimSpace(c.Mode)
	c.PublicReports = strings.ToLower(strings.TrimSpace(c.PublicReports))
	c.PostHog.Key = strings.TrimSpace(c.PostHog.Key)
	c.PostHog.Host = strings.TrimRight(strings.TrimSpace(c.PostHog.Host), "/")
	c.AllowedWorkspaces = cleanList(c.AllowedWorkspaces, strings.TrimSpace)
	c.OIDCIssuers = cleanList(c.OIDCIssuers, func(iss string) string {
		return strings.TrimRight(strings.TrimSpace(iss), "/")
	})
}

// cleanList applies clean to each entry of a comma-split list and drops
// the entries it leaves empty (a trailing comma, a blank between two).
// An empty result is nil, so "unset" and "set to nothing" compare equal.
func cleanList(entries []string, clean func(string) string) []string {
	out := entries[:0]
	for _, e := range entries {
		if e = clean(e); e != "" {
			out = append(out, e)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// secretKeyPattern is the required shape of GOCOV_SECRET_KEY: exactly 64
// hex characters (256 bits), either case.
var secretKeyPattern = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)

// validate reports the misconfigurations that must stop the process and
// that the struct tags cannot express: a value's shape, and rules that
// span more than one variable. Presence and emptiness are the tags' job
// (see DatabaseURL), so they are not repeated here.
func (c Server) validate() error {
	// GOCOV_SECRET_KEY is hex-decoded straight into the at-rest AES key
	// (secretbox.New), so the value itself must carry the full 256 bits
	// of entropy — a guessable passphrase would be brute-forceable
	// offline against a leaked database. secretbox refuses a key of any
	// other shape too; we repeat the check here so a typo stops the boot
	// with a message that names the variable and how to generate one.
	// An unset or blank value is not an error: it just leaves workspace
	// connect disabled.
	if c.SecretKey != "" && !secretKeyPattern.MatchString(c.SecretKey) {
		return errors.New("GOCOV_SECRET_KEY must be 64 hex characters.\nGenerate one with: openssl rand -hex 32")
	}
	switch c.Mode {
	case ModePrivate:
	case ModeHosted:
		// Self-service registration is meaningless without a forge
		// identity to derive claimable workspaces from; fail fast.
		if !c.SignInEnabled() {
			return errors.New("GOCOV_MODE=hosted requires a sign-in provider (set GOCOV_OAUTH_*_KEY/SECRET)")
		}
	default:
		return fmt.Errorf("GOCOV_MODE=%q: want private or hosted", c.Mode)
	}
	switch c.PublicReports {
	case "on", "off":
	default:
		return fmt.Errorf("GOCOV_PUBLIC_REPORTS=%q: want on or off", c.PublicReports)
	}
	// OIDC issuers are trusted to hand out signing keys over their discovery
	// endpoint, so they must be https URLs — a bare host or an http:// entry
	// would have gocov fetch keys over a channel it never meant to trust.
	for _, iss := range c.OIDCIssuers {
		u, err := url.Parse(iss)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return fmt.Errorf("GOCOV_OIDC_ISSUERS entry %q must be an https URL (the GitLab instance's base URL)", iss)
		}
	}
	// The host ends up in a <script src> on every page, so a value that
	// is not an absolute URL would break the UI silently rather than the
	// boot loudly. Only checked once the key turns the feature on: the
	// default host is fine to carry around unused.
	if c.PostHog.Configured() {
		u, err := url.Parse(c.PostHog.Host)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return fmt.Errorf("GOCOV_POSTHOG_HOST=%q: want an absolute URL such as https://eu.i.posthog.com", c.PostHog.Host)
		}
	}
	return nil
}

// Warnings lists the misconfigurations that are survivable: the caller
// logs them and carries on with that feature switched off. Half a
// credential pair is the whole list — it reads as "I meant to enable
// this", so staying silent about it would hide a typo'd variable name.
func (c Server) Warnings() []string {
	var warnings []string
	for _, forge := range []struct {
		name string
		app  OAuthApp
	}{
		{"BITBUCKET", c.Bitbucket},
		{"GITHUB", c.GitHub},
		{"GITLAB", c.GitLab},
	} {
		if forge.app.Partial() {
			warnings = append(warnings, fmt.Sprintf(
				"GOCOV_OAUTH_%[1]s_KEY and GOCOV_OAUTH_%[1]s_SECRET must both be set; ignoring", forge.name))
		}
	}
	if (c.GitHubAppID == "") != (c.GitHubAppPrivateKey == "") {
		warnings = append(warnings,
			"GOCOV_GITHUB_APP_ID and GOCOV_GITHUB_APP_PRIVATE_KEY must both be set; ignoring")
	}
	return warnings
}

// Hosted reports whether the instance runs in self-service mode.
func (c Server) Hosted() bool { return c.Mode == ModeHosted }

// PublicReportsEnabled reports whether anonymous read-only report pages
// for public repos are allowed on this instance.
func (c Server) PublicReportsEnabled() bool { return c.PublicReports == "on" }

// SignInEnabled reports whether any forge can sign users in. Configuring
// an OAuth consumer/app is the switch that turns sign-in on; without one
// the UI stays open and shows a banner saying so.
func (c Server) SignInEnabled() bool {
	return c.Bitbucket.Configured() || c.GitHub.Configured() || c.GitLab.Configured()
}

// GitHubAppEnabled reports whether the GitHub App integration is fully
// configured. A half-configured pair is a warning, not an error.
func (c Server) GitHubAppEnabled() bool {
	return c.GitHubAppID != "" && c.GitHubAppPrivateKey != ""
}

// BitbucketConnectEnabled reports whether Bitbucket workspace connect is
// on. It rides the sign-in consumer and needs the at-rest cipher for the
// refresh token it stores, so both must be configured.
func (c Server) BitbucketConnectEnabled() bool {
	return c.Bitbucket.Configured() && c.SecretKey != ""
}

// GitLabConnectEnabled reports whether GitLab workspace connect is on.
// It rides the sign-in application the same way Bitbucket's does; the
// application must additionally carry the "api" scope.
func (c Server) GitLabConnectEnabled() bool {
	return c.GitLab.Configured() && c.SecretKey != ""
}
