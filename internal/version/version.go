// Package version reports the Herald build version.
package version

// Version is the human-readable build version. It defaults to "dev" for
// source builds and is injected at release time:
//
//	go build -ldflags "-X github.com/marcustut/herald/internal/version.Version=v0.1.0" ./cmd/herald
var Version = "dev"

// String returns the build version, falling back to "dev" when unset so an
// empty or misconfigured -ldflags injection still prints something sane.
func String() string {
	if Version == "" {
		return "dev"
	}
	return Version
}
