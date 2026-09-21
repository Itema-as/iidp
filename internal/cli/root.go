// Package cli builds the iidp command tree and runs it in-process.
package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/Itema-as/iidp/internal/github"
	"github.com/Itema-as/iidp/internal/platform"
	"github.com/Itema-as/iidp/internal/version"
)

// Dependencies are the CLI's boundaries with the outside world that tests
// replace. A zero value means the real thing.
type Dependencies struct {
	// TokenSource yields the GitHub token; nil means the gh CLI.
	TokenSource github.TokenSource
	// BeforePush, when set, runs between committing to the Platform
	// repository and each push attempt. Tests use it to move main.
	BeforePush func() error
}

// Run executes the CLI with the given arguments (excluding the program name)
// and streams, and returns the process exit code. main calls it with the real
// process streams; tests call it with buffers.
func Run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	return RunWith(args, stdin, stdout, stderr, Dependencies{})
}

// RunWith is Run with the CLI's external dependencies replaced.
func RunWith(args []string, stdin io.Reader, stdout, stderr io.Writer, deps Dependencies) int {
	// cobra treats SetArgs(nil) as "use os.Args[1:]", which would let a caller
	// passing nil escape the in-process seam. An empty slice means no arguments.
	if args == nil {
		args = []string{}
	}
	if deps.TokenSource == nil {
		deps.TokenSource = github.GhCLI{}
	}
	root := newRootCommand(deps)
	root.SetArgs(args)
	root.SetIn(stdin)
	root.SetOut(stdout)
	root.SetErr(stderr)
	if err := root.Execute(); err != nil {
		return 1
	}
	return 0
}

func newRootCommand(deps Dependencies) *cobra.Command {
	root := &cobra.Command{
		Use:   "iidp",
		Short: "Create and change Applications on Itema's Platform",
		Long: "iidp creates and changes Applications on Itema's Platform. It writes the\n" +
			"desired state of each Application to the Platform repository\n" +
			"(" + platform.Repository + ") and lets the Platform reconcile from there.",
		SilenceUsage: true,
	}
	root.AddCommand(newVersionCommand())
	root.AddCommand(newAppCommand(deps))
	return root
}

func newVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the iidp version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := fmt.Fprintln(cmd.OutOrStdout(), version.Version)
			return err
		},
	}
}
