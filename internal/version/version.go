package version

// Version is overwritten at link time by the build with
//   -ldflags="-X github.com/luuuc/beacon/internal/version.Version=…"
// Left as a var (not const) so ldflags can rewrite it; the "-dev" suffix
// makes unstamped local builds easy to spot in the wild.
var Version = "0.0.0-dev"
