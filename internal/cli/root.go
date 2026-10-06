// Package cli builds the iidp command tree and runs it in-process.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/spf13/cobra"

	"github.com/Itema-as/iidp/internal/github"
	"github.com/Itema-as/iidp/internal/platform"
	"github.com/Itema-as/iidp/internal/sops"
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
	// GitHubAPI overrides the GitHub API base URL; empty means the real API.
	GitHubAPI string
	// CIRetryDelay is how long ci set-image waits before calling an
	// unavailable Deploy gate again; zero means ten seconds.
	CIRetryDelay time.Duration
	// HTTPClient is what app status calls the Deploy gate with; nil means a
	// client with a one-minute timeout.
	HTTPClient *http.Client
	// Encryptor encrypts what the CLI writes to the Platform repository's
	// sops/ directories; nil means the sops binary on PATH.
	Encryptor sops.Encryptor
	// Context is what commands run in; nil means context.Background().
	// Tests cancel it where a developer would press Ctrl-C.
	Context context.Context
	// DatabaseTunnel is how app db connect reaches the Database tunnel at
	// url with the developer's token; nil means its WebSocket client.
	DatabaseTunnel func(url, token string) DatabaseTunnel
}

// exitStatus is an error that ends the CLI with code and says nothing
// more: the program it ran, such as psql, has said why.
type exitStatus struct{ code int }

func (e *exitStatus) Error() string { return fmt.Sprintf("exit status %d", e.code) }

// Run executes the CLI with args (excluding the program name) and returns
// the process exit code.
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
	ctx := deps.Context
	if ctx == nil {
		ctx = context.Background()
	}
	if err := root.ExecuteContext(ctx); err != nil {
		var exit *exitStatus
		if errors.As(err, &exit) {
			return exit.code
		}
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
	root.AddCommand(newSecretCommand(deps))
	root.AddCommand(newCICommand(deps))
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
