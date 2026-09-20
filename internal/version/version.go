// Package version holds the CLI version injected at build time.
package version

// Version is the version of the iidp binary. GoReleaser overrides it at
// build time with -ldflags "-X github.com/Itema-as/iidp/internal/version.Version=<tag>".
// A binary built with plain go build reports "dev".
var Version = "dev"
