// Package version holds the CLI version injected at build time.
package version

// Version is the version of the iidp binary: the release tag without its
// leading "v", set by GoReleaser through -ldflags, or "dev" for a plain go build.
var Version = "dev"
