package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Itema-as/iidp/internal/appconfig"
	"github.com/Itema-as/iidp/internal/git"
	"github.com/Itema-as/iidp/internal/migrate"
	"github.com/Itema-as/iidp/internal/platform"
	"github.com/Itema-as/iidp/internal/platformrepo"
)

// addCapabilityOptions are the flags of app add-capability.
type addCapabilityOptions struct {
	postgres         bool
	migrationCommand string
	appDir           string
	staging          bool
	domains          []string
	size             string
	login            bool
	platformRepo     string
}

func newAppAddCapabilityCommand(deps Dependencies) *cobra.Command {
	var opts addCapabilityOptions
	cmd := &cobra.Command{
		Use:   "add-capability <name>",
		Short: "Add a Capability to an existing Application",
		Long: "Add Postgres, a staging Environment, a custom domain or a new size to an\n" +
			"Application that already exists on the Platform, reusing the same writers\n" +
			"as iidp app create. At least one Capability flag is required; a Capability\n" +
			"already present (Postgres already enabled, a staging Environment that\n" +
			"already exists, a domain already listed, the same size) is refused,\n" +
			"naming it, and nothing is written.\n\n" +
			"--postgres enables Postgres in every Environment the Application already\n" +
			"has, and prints the migrationCommand line to add to iidp.yaml in the\n" +
			"Application repository (--migration-command, or detected): each deploy\n" +
			"sends it to the Platform with its image. --staging adds a second Environment next to prod, copying prod's\n" +
			"values (its secrets are not copied). --domain (repeatable) adds a custom\n" +
			"domain to prod. --size changes the size of every Environment. --login\n" +
			"requires Itema (Entra ID) sign-in on the Platform addresses, in every\n" +
			"Environment; refused together with --domain, or when a custom domain is\n" +
			"already present.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAppAddCapability(cmd, args[0], opts, deps)
		},
	}
	f := cmd.Flags()
	f.BoolVar(&opts.postgres, "postgres", false, "Add the Postgres Capability: DATABASE_URL injected into every Environment, continuous backups")
	f.StringVar(&opts.migrationCommand, "migration-command", "", "Shell command run before every rollout with DATABASE_URL set, printed as the line to add to iidp.yaml (requires --postgres); detected from Prisma, Drizzle or an npm migrate script when omitted")
	f.StringVar(&opts.appDir, "app-dir", "", "Directory to detect the migration command in (default: the current directory when it has a package.json)")
	_ = f.MarkHidden("app-dir")
	f.BoolVar(&opts.staging, "staging", false, "Add a staging Environment next to prod, copying prod's values (its secrets are not copied)")
	f.StringArrayVar(&opts.domains, "domain", nil, "Custom domain to add to prod (repeatable)")
	f.StringVar(&opts.size, "size", "", "New size for every Environment: small, medium or large")
	f.BoolVar(&opts.login, "login", false, "Require Itema (Entra ID) sign-in on the Platform addresses, in every Environment; refused together with --domain, or when a custom domain is already present")
	f.StringVar(&opts.platformRepo, "platform-repo", platform.RepositoryURL, "Git URL of the Platform repository")
	_ = f.MarkHidden("platform-repo")
	return cmd
}

func runAppAddCapability(cmd *cobra.Command, name string, opts addCapabilityOptions, deps Dependencies) error {
	if err := platformrepo.ValidateName(name); err != nil {
		return err
	}
	// Checked on the effective values, not cmd.Flags().Changed: --postgres=false
	// or --staging=false is "changed" but asks for nothing, and would
	// otherwise reach AddCapabilities with an all-zero Capabilities and no
	// file to commit.
	if !opts.postgres && !opts.staging && len(opts.domains) == 0 && opts.size == "" && !opts.login {
		return fmt.Errorf("at least one Capability flag is required: --postgres, --staging, --domain, --size or --login")
	}
	if opts.migrationCommand != "" && !opts.postgres {
		return fmt.Errorf("--migration-command requires --postgres: there is no database to migrate")
	}
	if err := appconfig.ValidateMigrationCommand(opts.migrationCommand); err != nil {
		return fmt.Errorf("--migration-command: %w", err)
	}
	if opts.size != "" && !slices.Contains(sizes, opts.size) {
		return fmt.Errorf("unknown size %q: --size must be %s", opts.size, strings.Join(sizes, ", "))
	}
	if opts.login && len(opts.domains) > 0 {
		return errors.New(platformrepo.LoginDomainConflictMessage)
	}

	out := cmd.OutOrStdout()
	migrationCommand, err := detectAddCapabilityMigrationCommand(opts.postgres, opts.migrationCommand, opts.appDir, out)
	if err != nil {
		return err
	}

	token, err := deps.TokenSource.Token()
	if err != nil {
		return fmt.Errorf("not logged in to GitHub, so %s cannot be written: %w", platform.Repository, err)
	}

	fmt.Fprintf(out, "Adding Capabilities to %s: %s\n", name, strings.Join(capabilitySummary(opts), ", "))

	writer := &platformrepo.Writer{URL: opts.platformRepo, Auth: git.Auth{Token: token}, BeforePush: deps.BeforePush}
	res, err := writer.AddCapabilities(cmd.Context(), name, platformrepo.Capabilities{
		Postgres: opts.postgres,
		Staging:  opts.staging,
		Domains:  opts.domains,
		Size:     opts.size,
		Login:    opts.login,
	}, out)
	if err != nil {
		return err
	}

	printAddCapabilityResult(out, name, res)
	// add-capability cannot write to the developer's repository, and the
	// Platform must not take a command before the image that can run it,
	// so it prints the line instead.
	if opts.postgres {
		printMigrationCommandToAdd(out, name, migrationCommand)
	}
	return nil
}

// capabilitySummary describes, for the progress line, which Capabilities
// were asked for.
func capabilitySummary(opts addCapabilityOptions) []string {
	var s []string
	if opts.postgres {
		s = append(s, "Postgres")
	}
	if opts.staging {
		s = append(s, "staging")
	}
	for _, d := range opts.domains {
		s = append(s, "domain "+d)
	}
	if opts.size != "" {
		s = append(s, "size "+opts.size)
	}
	if opts.login {
		s = append(s, "login")
	}
	return s
}

// detectAddCapabilityMigrationCommand resolves the migration command for
// add-capability the same way runAppCreate's detectMigrationCommand does
// for anything but the Create path: the explicit --migration-command flag
// if given, otherwise, with --postgres, a detection in --app-dir or the
// current directory when it has a package.json
// (docs/implementation-notes/13-cli-capabilities.md). There is no generated
// template to fall back to here: add-capability never creates an
// Application repository.
func detectAddCapabilityMigrationCommand(postgres bool, migrationCommand, appDir string, out io.Writer) (string, error) {
	if migrationCommand != "" {
		return migrationCommand, nil
	}
	if !postgres {
		return "", nil
	}

	dir := appDir
	switch {
	case dir != "":
		fmt.Fprintf(out, "Looking for a migration command in %s...\n", dir)
	default:
		if cwd, err := os.Getwd(); err == nil {
			if info, err := os.Stat(filepath.Join(cwd, "package.json")); err == nil && !info.IsDir() {
				dir = cwd
				fmt.Fprintf(out, "Looking for a migration command in %s (the current directory)...\n", dir)
			}
		}
	}
	if dir == "" {
		return "", nil
	}

	det, ok, err := migrate.Detect(dir)
	if err != nil {
		return "", err
	}
	if !ok {
		fmt.Fprintln(out, "No migration tooling detected, so no migration command. Set --migration-command if the Application has migrations.")
		return "", nil
	}
	fmt.Fprintf(out, "Detected %s; migration command: %s\n", det.Tool, det.Command)
	return det.Command, nil
}

func printAddCapabilityResult(out io.Writer, name string, res platformrepo.Result) {
	fmt.Fprintf(out, "\nAdded Capabilities to %s. Committed to %s:\n", name, platform.Repository)
	for _, f := range res.Files {
		fmt.Fprintf(out, "  %s\n", f)
	}
	fmt.Fprintf(out, "\n  prod:     %s\n", res.Address)
	if res.StagingAddress != "" {
		fmt.Fprintf(out, "  staging:  %s\n", res.StagingAddress)
	}
	if res.Login {
		fmt.Fprintln(out, "  Login:    Itema (Entra ID) sign-in required; sign in once to reach every protected address")
	}
	printDomains(out, res)
}
