package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/Itema-as/iidp/internal/git"
	"github.com/Itema-as/iidp/internal/platform"
	"github.com/Itema-as/iidp/internal/platformrepo"
	"github.com/Itema-as/iidp/internal/render"
)

func newAppDBCommand(deps Dependencies) *cobra.Command {
	db := &cobra.Command{
		Use:   "db",
		Short: "Set who among an Application's developers may reach its databases, and reach them",
	}
	db.AddCommand(newAppDBAccessCommand(deps))
	db.AddCommand(newAppDBConnectCommand(deps))
	return db
}

type dbAccessOptions struct {
	environment  string
	readWrite    string
	readOnly     string
	platformRepo string
}

func newAppDBAccessCommand(deps Dependencies) *cobra.Command {
	var opts dbAccessOptions
	cmd := &cobra.Command{
		Use:   "access <name> --env <environment>",
		Short: "Set who among an Application's developers may reach an Environment's database",
		Long: "Set who among an Application's developers may reach the database of one\n" +
			"Environment, by their permission on the Application repository. Each level\n" +
			"is none, pull, push, maintain or admin, and names the lowest permission\n" +
			"that qualifies: admin includes maintain, which includes push, which\n" +
			"includes pull. A developer gets the higher of the two levels they qualify\n" +
			"for; GitHub's triage counts as pull.\n\n" +
			"--read-write opens the role <name>_write, which reads and writes every\n" +
			"table but changes no schema; --read-only opens <name>_read, which reads\n" +
			"every table. --read-only must not need more permission than --read-write\n" +
			"unless one of them is none. A level left out keeps its current value; an\n" +
			"Environment that never set one has its default: read-write push and\n" +
			"read-only none on staging, both none on prod. A Preview Environment always\n" +
			"has staging's levels.\n\n" +
			"The command writes the levels to the Environment's values file, generates\n" +
			"the password of each role whose level is not none, SOPS-encrypted with the\n" +
			"Platform's age key like iidp secret set, and removes the password of a role\n" +
			"whose level is none. Without --read-write or --read-only it writes the\n" +
			"passwords the current levels need. Write access to the Platform repository\n" +
			"is the authorisation, as for every other setting.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAppDBAccess(cmd, args[0], opts, deps)
		},
	}
	f := cmd.Flags()
	f.StringVar(&opts.environment, "env", "", "The Environment: prod or staging")
	_ = cmd.MarkFlagRequired("env")
	f.StringVar(&opts.readWrite, "read-write", "", "The lowest Application repository permission that may read and write: none, pull, push, maintain or admin")
	f.StringVar(&opts.readOnly, "read-only", "", "The lowest Application repository permission that may read: none, pull, push, maintain or admin")
	f.StringVar(&opts.platformRepo, "platform-repo", platform.RepositoryURL, "Git URL of the Platform repository")
	_ = f.MarkHidden("platform-repo")
	return cmd
}

func runAppDBAccess(cmd *cobra.Command, name string, opts dbAccessOptions, deps Dependencies) error {
	if err := platformrepo.ValidateName(name); err != nil {
		return err
	}
	if err := platformrepo.ValidateEnvironmentName(opts.environment); err != nil {
		return err
	}
	token, err := deps.TokenSource.Token()
	if err != nil {
		return fmt.Errorf("not logged in to GitHub, so %s cannot be written: %w", platform.Repository, err)
	}

	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "Setting database access for %s %s...\n", name, opts.environment)
	writer := &platformrepo.Writer{URL: opts.platformRepo, Auth: git.Auth{Token: token}, BeforePush: deps.BeforePush, Encryptor: deps.Encryptor}
	res, err := writer.SetDatabaseAccess(cmd.Context(), name, opts.environment, render.DatabaseAccess{ReadWrite: opts.readWrite, ReadOnly: opts.readOnly})
	if err != nil {
		return err
	}
	if len(res.Files) == 0 {
		fmt.Fprintf(out, "\nNothing to change in %s.\n", platform.Repository)
	} else {
		fmt.Fprintf(out, "\nCommitted to %s:\n", platform.Repository)
		for _, f := range res.Files {
			fmt.Fprintf(out, "  %s\n", f)
		}
	}
	fmt.Fprintf(out, "\n  %s database: %s\n", opts.environment, databaseAccessText(res.Access))
	return nil
}

// databaseAccessText is an Environment's database access levels in words.
func databaseAccessText(access render.DatabaseAccess) string {
	return "read-write " + access.ReadWrite + " · read-only " + access.ReadOnly
}
