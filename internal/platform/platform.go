// Package platform holds the two values compiled into the CLI: the GitHub
// org and the name of the Platform repository. Every other Platform setting
// (base domain, chart version, buckets) is read from platform.yaml in the
// Platform repository at run time, so changing it is a commit, not a release.
//
// This is the only place the org and the Platform repository name may
// appear in code.
package platform

const (
	// Org is the GitHub org that owns the Platform repository and the
	// Application repositories the CLI creates.
	Org = "Itema-as"

	// RepositoryName is the name of the Platform repository within Org.
	RepositoryName = "iidp-platform"

	// Repository is the Platform repository as "owner/name".
	Repository = Org + "/" + RepositoryName
)
