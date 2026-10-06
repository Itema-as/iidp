package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"sync"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/Itema-as/iidp/internal/dbtunnel/api"
	"github.com/Itema-as/iidp/internal/git"
	"github.com/Itema-as/iidp/internal/platform"
	"github.com/Itema-as/iidp/internal/platformrepo"
	"github.com/Itema-as/iidp/internal/render"
)

// DatabaseTunnel is what app db connect needs of the Database tunnel: its
// check, and one connection per local connection.
type DatabaseTunnel interface {
	Check(ctx context.Context, application, environment string, readOnly bool) (api.Grant, error)
	Connect(ctx context.Context, application, environment string, readOnly bool) (net.Conn, error)
}

type dbConnectOptions struct {
	environment  string
	pr           int
	readOnly     bool
	psql         bool
	platformRepo string
}

func newAppDBConnectCommand(deps Dependencies) *cobra.Command {
	var opts dbConnectOptions
	cmd := &cobra.Command{
		Use:   "connect <name> [--env staging|prod] [--pr <number>] [--read-only] [--psql]",
		Short: "Reach an Environment's database from this machine through the Database tunnel",
		Long: "Reach the database of one of an Application's Environments from this machine,\n" +
			"through the Database tunnel at https://db.<baseDomain> (baseDomain from the\n" +
			"Platform repository's platform.yaml). The Environment is staging when the\n" +
			"Application has one, else prod; --env picks one, and --pr a Preview\n" +
			"Environment by its pull request's number.\n\n" +
			"The tunnel lets you in by your permission on the Application repository,\n" +
			"which it asks GitHub with your gh auth token, and the Environment's access\n" +
			"levels (see iidp app db access): read-write as <name>_write when you\n" +
			"qualify for it, else read-only as <name>_read. --read-only steps read-write\n" +
			"down to read-only. The tunnel logs in to the database itself, so you need no\n" +
			"password, and none reaches this machine.\n\n" +
			"The command checks once, then listens on 127.0.0.1 and prints a connection\n" +
			"string for any Postgres client, until Ctrl-C. Every connection is checked\n" +
			"again and recorded. A session ends after 30 minutes without traffic, and\n" +
			"after 8 hours in any case. --psql runs psql with the connection string and\n" +
			"exits with its status. Nothing talks to Kubernetes from your machine.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAppDBConnect(cmd, args[0], opts, deps)
		},
	}
	f := cmd.Flags()
	f.StringVar(&opts.environment, "env", "", "The Environment: prod or staging (default staging when there is one, else prod)")
	f.IntVar(&opts.pr, "pr", 0, "The Preview Environment of this pull request")
	f.BoolVar(&opts.readOnly, "read-only", false, "Connect read-only, even when you may write")
	f.BoolVar(&opts.psql, "psql", false, "Run psql with the connection string, and exit with its status")
	f.StringVar(&opts.platformRepo, "platform-repo", platform.RepositoryURL, "Git URL of the Platform repository")
	_ = f.MarkHidden("platform-repo")
	return cmd
}

func runAppDBConnect(cmd *cobra.Command, name string, opts dbConnectOptions, deps Dependencies) error {
	if err := platformrepo.ValidateName(name); err != nil {
		return err
	}
	environment := api.EnvironmentAuto
	switch {
	case opts.environment != "" && opts.pr != 0:
		return errors.New("--env and --pr cannot be combined: --pr picks a Preview Environment, whose access is staging's")
	case opts.pr < 0:
		return fmt.Errorf("--pr %d is not a pull request number", opts.pr)
	case opts.pr > 0:
		environment = render.PreviewEnvironment(strconv.Itoa(opts.pr))
	case opts.environment != "":
		if err := platformrepo.ValidateEnvironmentName(opts.environment); err != nil {
			return err
		}
		environment = opts.environment
	}
	psql := ""
	if opts.psql {
		var err error
		if psql, err = exec.LookPath("psql"); err != nil {
			return errors.New("psql is not on your PATH: install it, or leave out --psql and connect with any Postgres client to the connection string iidp app db connect prints")
		}
	}
	token, err := deps.TokenSource.Token()
	if err != nil {
		return fmt.Errorf("not logged in to GitHub, and the Database tunnel needs your GitHub token to let you in: %w", err)
	}
	// The tunnel's address follows platform.yaml's baseDomain. If the
	// Platform repository cannot be read, Itema's Platform is assumed: the
	// tunnel then says for itself whether the token may connect.
	url := platform.DefaultDatabaseTunnelURL
	writer := &platformrepo.Writer{URL: opts.platformRepo, Auth: git.Auth{Token: token}}
	if cfg, err := writer.ReadConfig(cmd.Context()); err == nil {
		url = cfg.DatabaseTunnelURL()
	} else {
		fmt.Fprintf(cmd.ErrOrStderr(), "Could not read platform.yaml, so asking the Database tunnel at %s: %v\n", url, err)
	}
	var tunnel DatabaseTunnel = &api.Client{URL: url, Token: token}
	if deps.DatabaseTunnel != nil {
		tunnel = deps.DatabaseTunnel(url, token)
	}

	grant, err := tunnel.Check(cmd.Context(), name, environment, opts.readOnly)
	if err != nil {
		var refused *api.Refused
		if errors.As(err, &refused) {
			return errors.New(refused.Message)
		}
		return err
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer listener.Close()
	conninfo := fmt.Sprintf("postgresql://%s@%s/%s?sslmode=disable", grant.Role, listener.Addr(), grant.Database)

	// psql reads Ctrl-C itself, to cancel a query; it must not stop the
	// tunnel under it. Without psql, Ctrl-C is how the command ends.
	stopSignals := []os.Signal{os.Interrupt, syscall.SIGTERM}
	if psql != "" {
		stopSignals = []os.Signal{syscall.SIGTERM}
		interrupts := make(chan os.Signal, 1)
		signal.Notify(interrupts, os.Interrupt)
		defer signal.Stop(interrupts)
	}
	ctx, stop := signal.NotifyContext(cmd.Context(), stopSignals...)
	defer stop()

	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "Connected to %s %s's database through the Database tunnel at %s:\n\n  %s\n\n", grant.Application, grant.Environment, url, conninfo)
	fmt.Fprintf(out, "%s %s, %s as %s. Each connection is checked again and recorded; a session ends after 30 minutes without traffic and after 8 hours.\n",
		grant.Application, grant.Environment, grant.Access, grant.Role)

	var connections sync.WaitGroup
	defer connections.Wait()
	go func() {
		<-ctx.Done()
		listener.Close()
	}()
	go func() {
		for {
			local, err := listener.Accept()
			if err != nil {
				return
			}
			connections.Add(1)
			go func() {
				defer connections.Done()
				forward(ctx, cmd.ErrOrStderr(), tunnel, local, grant, opts.readOnly)
			}()
		}
	}()

	if psql == "" {
		fmt.Fprintln(out, "Press Ctrl-C to stop.")
		<-ctx.Done()
		return nil
	}
	run := exec.Command(psql, conninfo)
	run.Stdin, run.Stdout, run.Stderr = cmd.InOrStdin(), out, cmd.ErrOrStderr()
	err = run.Run()
	stop()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		cmd.SilenceErrors = true
		return &exitStatus{code: exit.ExitCode()}
	}
	return err
}

// forward carries one local connection through its own connection to the
// tunnel, until either side closes or ctx ends.
func forward(ctx context.Context, stderr io.Writer, tunnel DatabaseTunnel, local net.Conn, grant api.Grant, readOnly bool) {
	defer local.Close()
	remote, err := tunnel.Connect(ctx, grant.Application, grant.Environment, readOnly)
	if err != nil {
		fmt.Fprintf(stderr, "A connection could not be opened: %v\n", err)
		return
	}
	defer remote.Close()
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(remote, local)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(local, remote)
		done <- struct{}{}
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
}
