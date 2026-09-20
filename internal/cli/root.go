// Package cli builds the iidp command tree and runs it in-process.
package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/Itema-as/iidp/internal/platform"
	"github.com/Itema-as/iidp/internal/version"
)

// Run executes the CLI with the given arguments (excluding the program name)
// and streams, and returns the process exit code. main calls it with the real
// process streams; tests call it with buffers.
func Run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	// cobra treats SetArgs(nil) as "use os.Args[1:]", which would let a caller
	// passing nil escape the in-process seam. An empty slice means no arguments.
	if args == nil {
		args = []string{}
	}
	root := newRootCommand()
	root.SetArgs(args)
	root.SetIn(stdin)
	root.SetOut(stdout)
	root.SetErr(stderr)
	if err := root.Execute(); err != nil {
		return 1
	}
	return 0
}

func newRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:   "iidp",
		Short: "Create and change Applications on Itema's Platform",
		Long: "iidp creates and changes Applications on Itema's Platform. It writes the\n" +
			"desired state of each Application to the Platform repository\n" +
			"(" + platform.Repository + ") and lets the Platform reconcile from there.",
		SilenceUsage: true,
	}
	root.AddCommand(newVersionCommand())
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
