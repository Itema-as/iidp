// Package version holds the CLI version injected at build time.
package version

// Version is the version of the iidp binary. GoReleaser sets it at build time
// with -ldflags "-X github.com/Itema-as/iidp/internal/version.Version=<version>",
// where <version> is the release tag without its leading "v" (tag v0.3.1
// gives "0.3.1"). A binary built with plain go build reports "dev".
var Version = "dev"
