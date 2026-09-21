// Package platform holds the two values compiled into the CLI: the GitHub
// org and the name of the Platform repository. Every other Platform setting
// (base domain, chart version, buckets) is read from platform.yaml in the
// Platform repository at run time, so changing it is a commit, not a release.
//
// This is the only place the org and the Platform repository name may
// appear in code.
package platform

import "strings"

// Registry is the GHCR namespace of Org, where Application images and the
// generic chart are pushed: ghcr.io/<org> in lowercase, as GHCR wants it.
var Registry = "ghcr.io/" + strings.ToLower(Org)

const (
	// Org is the GitHub org that owns the Platform repository and the
	// Application repositories the CLI creates.
	Org = "Itema-as"

	// RepositoryName is the name of the Platform repository within Org.
	RepositoryName = "iidp-platform"

	// Repository is the Platform repository as "owner/name".
	Repository = Org + "/" + RepositoryName

	// RepositoryURL is the git URL the CLI clones and pushes, and the URL
	// the ArgoCD Applications it writes name as their values source. It is
	// the same URL the bootstrap's root Application reconciles from.
	RepositoryURL = "https://github.com/" + Repository + ".git"

	// CLIRepository is this repository itself, as "owner/name": where the
	// generated deploy workflow downloads iidp release archives from
	// (docs/implementation-notes/12-deploy-workflow.md).
	CLIRepository = Org + "/iidp"
)
