// Package hosted holds shared constants for the hosted gocov service, so
// the CLI's default server and the onboarding page's "server is implicit"
// check stay in sync from a single source of truth.
package hosted

// DefaultServer is the hosted gocov instance the CLI uploads to when no
// -server / $GOCOV_SERVER is given, and which the onboarding page treats
// as implicit (so hosted users only configure a token). Self-hosters run
// under a different base URL and set the server explicitly.
const DefaultServer = "https://app.gocov.dev"

// PinnedCLIVersion is the gocov CLI release the copy-paste CI recipes
// install. Runners that fetch a binary have to name a version, and "latest"
// in a pipeline is a build that changes under you, so the snippets pin one
// — in the CI recipe pages under docs/ and in the onboarding page the app
// renders. They are prose and HTML rather than code, so nothing but a test
// can keep them honest: bump this constant on release and
// TestPinnedCLIVersionIsInSync names every file still on the old one.
const PinnedCLIVersion = "v0.22.0" // x-release-please-version
