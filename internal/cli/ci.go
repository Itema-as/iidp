package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Itema-as/iidp/internal/git"
	"github.com/Itema-as/iidp/internal/github"
	"github.com/Itema-as/iidp/internal/githubapp"
	"github.com/Itema-as/iidp/internal/platform"
	"github.com/Itema-as/iidp/internal/platformrepo"
)

// ciSetImageOptions are the positional arguments of ci set-image.
type ciSetImageOptions struct {
	application  string
	environment  string
	tag          string
	platformRepo string
}

func newCICommand(deps Dependencies) *cobra.Command {
	ci := &cobra.Command{
		Use:   "ci",
		Short: "Commands the deploy workflow runs; not for developers",
	}
	ci.AddCommand(newCISetImageCommand(deps))
	return ci
}

func newCISetImageCommand(deps Dependencies) *cobra.Command {
	var opts ciSetImageOptions
	cmd := &cobra.Command{
		Use:   "set-image <app> <prod|staging|auto> <tag>",
		Short: "Write an image tag into an Environment's values.yaml (run by the deploy workflow)",
		Long: "Writes tag into image.tag of an Application's Environment in the Platform\n" +
			"repository (" + platform.Repository + "), the way the deploy workflow's\n" +
			"write-back step does: a commit SHA on every push to main, a version on a\n" +
			"v* tag. auto targets staging when the Application has one and prod\n" +
			"otherwise; that decision is made from the Platform repository, never by\n" +
			"the workflow.\n\n" +
			"Authenticates as the org's GitHub App, not the developer: the app id from\n" +
			"IIDP_DEPLOY_APP_ID (an org Actions variable), the private key from\n" +
			"IIDP_DEPLOY_APP_PRIVATE_KEY (a PEM) or IIDP_DEPLOY_APP_PRIVATE_KEY_FILE (a\n" +
			"path to one), and the installation id discovered from GitHub itself. gh\n" +
			"auth login is not consulted; this command is not meant to be run by hand.",
		Args: cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.application = args[0]
			opts.environment = args[1]
			opts.tag = args[2]
			return runCISetImage(cmd, opts, deps)
		},
	}
	f := cmd.Flags()
	f.StringVar(&opts.platformRepo, "platform-repo", platform.RepositoryURL, "Git URL of the Platform repository")
	_ = f.MarkHidden("platform-repo")
	return cmd
}

func runCISetImage(cmd *cobra.Command, opts ciSetImageOptions, deps Dependencies) error {
	if err := platformrepo.ValidateName(opts.application); err != nil {
		return err
	}
	if err := validateCIEnvironment(opts.environment); err != nil {
		return err
	}
	if strings.TrimSpace(opts.tag) == "" {
		return errors.New("the tag must not be empty")
	}

	auth, err := ciInstallationAuth(cmd.Context(), deps)
	if err != nil {
		return err
	}

	writer := &platformrepo.Writer{URL: opts.platformRepo, Auth: auth, BeforePush: deps.BeforePush}
	res, err := writer.SetImageTag(cmd.Context(), opts.application, opts.environment, opts.tag)
	if err != nil {
		return err
	}

	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "Deploy %s %s %s\n", opts.application, res.Environment, opts.tag)
	fmt.Fprintf(out, "\nCommitted to %s:\n", platform.Repository)
	for _, f := range res.Files {
		fmt.Fprintf(out, "  %s\n", f)
	}
	if res.DocumentedGitHubAppInstallationID != 0 {
		fmt.Fprintf(out, "\n(%s documents githubApp.installationId %d; informational only.)\n", platformrepo.ConfigFile, res.DocumentedGitHubAppInstallationID)
	}
	return nil
}

// validateCIEnvironment refuses anything but the three values ci set-image
// accepts, clearly, before any network call.
func validateCIEnvironment(environment string) error {
	switch environment {
	case "prod", "staging", platformrepo.EnvironmentAuto:
		return nil
	default:
		return fmt.Errorf("unknown Environment %q: must be prod, staging or auto", environment)
	}
}

// ciInstallationAuth mints the GitHub App installation token ci set-image
// authenticates the Platform repository with, entirely without reading the
// Platform repository first: the app id comes from IIDP_DEPLOY_APP_ID (the
// org Actions variable the deploy workflow passes through, see
// docs/implementation-notes/05-bootstrap-wizard.md), the private key from
// the environment, and the installation id is discovered from GitHub
// itself (GET /app/installations) rather than from platform.yaml, so
// minting a credential never depends on already having one to read the
// Platform repository with (docs/implementation-notes/12-deploy-workflow.md).
func ciInstallationAuth(ctx context.Context, deps Dependencies) (git.Auth, error) {
	appID, err := githubapp.AppIDFromEnv()
	if err != nil {
		return git.Auth{}, err
	}
	key, err := githubapp.PrivateKeyFromEnv()
	if err != nil {
		return git.Auth{}, err
	}
	jwt, err := githubapp.SignJWT(appID, key, time.Now())
	if err != nil {
		return git.Auth{}, err
	}
	ghClient := &github.Client{Token: jwt}
	if deps.GitHubAPI != "" {
		ghClient.BaseURL = deps.GitHubAPI
	}
	installationID, err := resolveInstallationID(ctx, ghClient, 0)
	if err != nil {
		return git.Auth{}, err
	}
	token, err := ghClient.CreateInstallationToken(ctx, installationID)
	if err != nil {
		return git.Auth{}, err
	}
	if deps.CIAuthObserved != nil {
		deps.CIAuthObserved(token)
	}
	return git.Auth{Token: token, Identity: ciIdentity()}, nil
}

// ciIdentity is who a deploy write-back commits as: the GitHub user whose
// push or tag started the workflow, from the variables every Actions run
// sets, with the noreply address GitHub links to that account, so the
// Platform repository's log records who deployed what. A hosted runner has
// no git identity of its own, and without this the commit fails. Outside
// Actions it is empty, leaving the identity to git's own configuration.
func ciIdentity() git.Identity {
	actor, id := os.Getenv("GITHUB_ACTOR"), os.Getenv("GITHUB_ACTOR_ID")
	if actor == "" || id == "" {
		return git.Identity{}
	}
	return git.Identity{Name: actor, Email: id + "+" + actor + "@users.noreply.github.com"}
}

// resolveInstallationID is the org's installation id of the GitHub App
// ghClient is authenticated as (a JWT). When knownID is non-zero, it is
// returned directly and GitHub is not consulted at all: a shortcut for a
// caller that already learned the installation id some other way (for
// example, platform.yaml's githubApp.installationId, once it has been read
// from an authenticated clone — never before one exists). iidp ci set-image
// itself always calls this with knownID 0, since it has no clone yet at
// this point; the shortcut exists for callers that do.
//
// Without a known id, it lists the App's installations
// (GET /app/installations, confirmed against the current GitHub REST API
// documentation: "List installations for the authenticated app") and picks
// the one whose account matches the compiled-in org (internal/platform)
// case-insensitively.
func resolveInstallationID(ctx context.Context, ghClient *github.Client, knownID int64) (int64, error) {
	if knownID != 0 {
		return knownID, nil
	}
	installations, err := ghClient.ListInstallations(ctx)
	if err != nil {
		return 0, err
	}
	for _, inst := range installations {
		if strings.EqualFold(inst.Account.Login, platform.Org) {
			return inst.ID, nil
		}
	}
	return 0, fmt.Errorf("no GitHub App installation found for %s; install the org's deploy App on %s first", platform.Org, platform.Org)
}
