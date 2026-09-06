package server

import (
	"strconv"

	"github.com/gocov/gocov/internal/store"
)

// PostHog configures the opt-in browser analytics snippet. The layout
// renders it as one <meta> tag that static/app.js reads and turns into a
// posthog-js init; nothing here runs server-side, and nothing loads in the
// browser unless Key is set. The snippet is deliberately narrow: memory
// persistence (no cookie, no localStorage), no autocapture, Do Not Track
// honoured, and session replay only on the sign-in and setup pages with
// every input masked and the upload token blocked — never on a report or
// source page. Signed-in users are identified by their gocov user id, never
// by email, so what PostHog holds is a numeric pseudonym, page paths and
// the setup-flow recordings.
type PostHog struct {
	// Key is the PostHog project API key (phc_...). It is public by
	// design — every visitor's browser sees it — so it is not a secret.
	Key string
	// Host is the ingestion endpoint the browser loads posthog-js from
	// and sends events to, e.g. https://eu.i.posthog.com.
	Host string
}

// Configured reports whether the snippet renders.
func (p PostHog) Configured() bool { return p.Key != "" }

// posthogView is what the layout's <meta name="gocov-posthog"> carries:
// the init parameters plus the pseudonymous id for the signed-in user
// (empty when signed out, so anonymous pageviews stay anonymous).
type posthogView struct {
	Key, Host, UserID string
}

func (p PostHog) view(u *store.User) posthogView {
	v := posthogView{Key: p.Key, Host: p.Host}
	if u != nil {
		v.UserID = strconv.FormatInt(u.ID, 10)
	}
	return v
}
