// Package platform holds the values compiled into the CLI: the GitHub org and
// the Platform repository name. Every other Platform setting is read from
// platform.yaml at run time, so changing it is a commit, not a release.
//
// This is the only place the org and the Platform repository name may
// appear in code.
package platform

import "strings"

// Registry is the GHCR namespace of Org, lowercased as GHCR requires.
var Registry = "ghcr.io/" + strings.ToLower(Org)

const (
	// Org is the GitHub org that owns the Platform repository and the
	// Application repositories the CLI creates.
	Org = "Itema-as"

	// RepositoryName is the name of the Platform repository within Org.
	RepositoryName = "iidp-platform"

	// Repository is the Platform repository as "owner/name".
	Repository = Org + "/" + RepositoryName

	// RepositoryURL is the git URL of the Platform repository. ArgoCD
	// Applications name it as their values source, so it must match the URL
	// the bootstrap's root Application reconciles from.
	RepositoryURL = "https://github.com/" + Repository + ".git"

	// CLIRepository is this repository, as "owner/name": where the deploy
	// workflow downloads iidp release archives from.
	CLIRepository = Org + "/iidp"

	// DefaultDeployGateURL is the default of the reusable deploy workflow's
	// deploy-gate-url input. A test keeps the two equal.
	DefaultDeployGateURL = "https://deploy.app.itma.no"

	// DeployWorkflow is the reusable workflow every Application
	// repository's .github/workflows/deploy.yaml calls, without a ref.
	DeployWorkflow = CLIRepository + "/.github/workflows/application-deploy.yaml"
)
