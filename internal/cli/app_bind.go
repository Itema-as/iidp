package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/Itema-as/iidp/internal/apprepo"
	"github.com/Itema-as/iidp/internal/git"
	"github.com/Itema-as/iidp/internal/github"
	"github.com/Itema-as/iidp/internal/platform"
	"github.com/Itema-as/iidp/internal/platformrepo"
)

// bindOptions are the flags of app bind.
type bindOptions struct {
	repo         string
	rebind       bool
	platformRepo string
}

func newAppBindCommand(deps Dependencies) *cobra.Command {
	var opts bindOptions
	cmd := &cobra.Command{
		Use:   "bind <name> --repo " + platform.Org + "/<repository>",
		Short: "Bind an existing Application to its Application repository",
		Long: "Bind an existing Application to its Application repository by GitHub's\n" +
			"numeric ids, the ones a GitHub Actions OIDC token carries as repository_id\n" +
			"and repository_owner_id. The Deploy gate lets only that repository deploy\n" +
			"the Application; a rename keeps working, a repository deleted and\n" +
			"recreated under the same name does not.\n\n" +
			"iidp app create --path create and --path adopt bind the Application\n" +
			"themselves. This is for an Application made before they did, or made\n" +
			"without --path. It reads the repository's ids from GitHub and commits\n" +
			"applications/<name>/repository.yaml to the Platform repository\n" +
			"(" + platform.Repository + "). The repository must be in " + platform.Org + ".\n\n" +
			"Binding to the repository the Application is already bound to changes\n" +
			"nothing. Binding it to a different one is refused unless --rebind is given.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAppBind(cmd, args[0], opts, deps)
		},
	}
	f := cmd.Flags()
	f.StringVar(&opts.repo, "repo", "", "The Application repository, in "+platform.Org+": "+platform.Org+"/name or a URL (required)")
	f.BoolVar(&opts.rebind, "rebind", false, "Replace a binding to a different repository, which stops that repository deploying the Application")
	f.StringVar(&opts.platformRepo, "platform-repo", platform.RepositoryURL, "Git URL of the Platform repository")
	_ = f.MarkHidden("platform-repo")
	return cmd
}

func runAppBind(cmd *cobra.Command, name string, opts bindOptions, deps Dependencies) error {
	if err := platformrepo.ValidateName(name); err != nil {
		return err
	}
	if opts.repo == "" {
		return errors.New(`required flag(s) "repo" not set: the Application repository to bind, ` + platform.Org + `/name or a URL`)
	}
	owner, repoName, err := parseRepoFlag(opts.repo)
	if err != nil {
		return err
	}
	if err := apprepo.CheckInOrg(owner, repoName); err != nil {
		return err
	}

	token, err := deps.TokenSource.Token()
	if err != nil {
		return fmt.Errorf("not logged in to GitHub, so %s cannot be written: %w", platform.Repository, err)
	}
	ghClient := &github.Client{Token: token}
	if deps.GitHubAPI != "" {
		ghClient.BaseURL = deps.GitHubAPI
	}
	repo, err := ghClient.GetRepository(cmd.Context(), owner, repoName)
	if err != nil {
		if github.IsNotFound(err) {
			return fmt.Errorf("%s/%s does not exist, or your GitHub login cannot see it: %w", owner, repoName, err)
		}
		return err
	}
	binding, err := apprepo.BindingForRepository(owner+"/"+repoName, repo)
	if err != nil {
		return err
	}

	out := cmd.OutOrStdout()
	writer := &platformrepo.Writer{URL: opts.platformRepo, Auth: git.Auth{Token: token}, BeforePush: deps.BeforePush}
	res, err := writer.BindRepository(cmd.Context(), name, binding, opts.rebind)
	if err != nil {
		return err
	}
	if res.Unchanged {
		fmt.Fprintf(out, "Application %s is already bound to %s (repository id %d, owner id %d); nothing to change.\n", name, binding.Repository, binding.RepositoryID, binding.RepositoryOwnerID)
		return nil
	}
	fmt.Fprintf(out, "Bound Application %s to %s (repository id %d, owner id %d). Committed to %s:\n  %s\n", name, binding.Repository, binding.RepositoryID, binding.RepositoryOwnerID, platform.Repository, res.File)
	if prev := res.Previous; prev != nil && prev.Complete() && !prev.SameRepository(binding) {
		fmt.Fprintf(out, "It was bound to %s (repository id %d, owner id %d), which can no longer deploy it.\n", prev.Repository, prev.RepositoryID, prev.RepositoryOwnerID)
	}
	return nil
}
