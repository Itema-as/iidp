package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Itema-as/iidp/internal/git"
	"github.com/Itema-as/iidp/internal/platform"
	"github.com/Itema-as/iidp/internal/platformrepo"
)

// deleteOptions are the flags of app delete.
type deleteOptions struct {
	force        bool
	interactive  bool
	platformRepo string
}

func newAppDeleteCommand(deps Dependencies) *cobra.Command {
	var opts deleteOptions
	cmd := &cobra.Command{
		Use:   "delete <name>",
		Short: "Delete an Application from the Platform",
		Long: "Delete an Application: for each Environment with Postgres enabled, commit a\n" +
			"final CloudNativePG Backup annotated to be kept 30 days, then, in a second\n" +
			"commit pushed together with the first, remove the Application's Environment\n" +
			"directories. The Environments' own ArgoCD Applications carry the resources\n" +
			"finalizer, so ArgoCD deletes their resources once it notices the directory\n" +
			"is gone; the final Backup's own directory is kept as the record. The\n" +
			"Application repository is never touched.\n\n" +
			"On a terminal, this asks for the Application name typed back and refuses on\n" +
			"a mismatch. --force skips the confirmation for non-interactive use; without\n" +
			"a terminal and without --force, the command refuses outright.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAppDelete(cmd, args[0], opts, deps)
		},
	}
	f := cmd.Flags()
	f.BoolVar(&opts.force, "force", false, "Skip the typed confirmation (for non-interactive use)")
	f.BoolVar(&opts.interactive, "interactive", false, "Ask for the typed confirmation even without a terminal (tests and scripts that pipe the typed name)")
	f.StringVar(&opts.platformRepo, "platform-repo", platform.RepositoryURL, "Git URL of the Platform repository")
	_ = f.MarkHidden("platform-repo")
	return cmd
}

func runAppDelete(cmd *cobra.Command, name string, opts deleteOptions, deps Dependencies) error {
	if err := platformrepo.ValidateName(name); err != nil {
		return err
	}

	out := cmd.OutOrStdout()
	if !opts.force {
		if !opts.interactive && !isTerminalReader(cmd.InOrStdin()) {
			return fmt.Errorf("refusing to delete %q without confirmation: run this on a terminal, or pass --force to skip confirmation non-interactively", name)
		}
		fmt.Fprintf(out, "This deletes Application %q: its Environments and, where Postgres is enabled, its databases (a final backup is kept %d days). Type the Application name to confirm: ", name, platformrepo.FinalBackupRetentionDays)
		typed, err := readLine(cmd.InOrStdin())
		if err != nil {
			return fmt.Errorf("reading the confirmation: %w", err)
		}
		if typed != name {
			return fmt.Errorf("typed %q, which does not match Application name %q; nothing was deleted", typed, name)
		}
	}

	token, err := deps.TokenSource.Token()
	if err != nil {
		return fmt.Errorf("not logged in to GitHub, so %s cannot be written: %w", platform.Repository, err)
	}

	writer := &platformrepo.Writer{URL: opts.platformRepo, Auth: git.Auth{Token: token}, BeforePush: deps.BeforePush}
	res, err := writer.DeleteApplication(cmd.Context(), name, out)
	if err != nil {
		return err
	}

	printDeleteResult(out, name, res)
	return nil
}

// isTerminalReader reports whether r is a character device, the same cheap
// check (no new dependency) used to decide whether app delete should
// prompt: a real terminal is *os.File backed by one; the buffers and pipes
// tests and scripts pass are not.
func isTerminalReader(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// readLine reads one line from r, trimmed of surrounding whitespace, for
// the typed-back confirmation.
func readLine(r io.Reader) (string, error) {
	scanner := bufio.NewScanner(r)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return "", err
		}
		return "", nil
	}
	return strings.TrimSpace(scanner.Text()), nil
}

func printDeleteResult(out io.Writer, name string, res platformrepo.DeleteResult) {
	fmt.Fprintf(out, "\nDeleted Application %s from %s:\n", name, platform.Repository)
	for _, d := range res.Deleted {
		fmt.Fprintf(out, "  %s\n", d)
	}
	if len(res.FinalBackups) == 0 {
		fmt.Fprintln(out, "\nNo Environment had Postgres enabled; there was nothing to back up.")
		return
	}
	fmt.Fprintln(out, "\nFinal Postgres backups recorded (ArgoCD applies each from its own directory):")
	for _, b := range res.FinalBackups {
		fmt.Fprintf(out, "  %s: Cluster %s in namespace %s, retain-until %s\n", b.Path, b.Cluster, b.Namespace, b.RetainUntil)
	}
	fmt.Fprintln(out, "\nThe continuous WAL archive and scheduled backups already in object storage stay for 30 days regardless of whether this final Backup completes before the Cluster is removed; they are not deleted with it.")
}
