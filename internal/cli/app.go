package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Itema-as/iidp/internal/git"
	"github.com/Itema-as/iidp/internal/platform"
	"github.com/Itema-as/iidp/internal/platformrepo"
)

// Kinds and sizes the chart accepts. The Static site Kind is a valid answer
// that is refused until its chart support lands.
const (
	kindWebService = "web-service"
	kindStaticSite = "static-site"
)

var sizes = []string{"small", "medium", "large"}

func newAppCommand(deps Dependencies) *cobra.Command {
	app := &cobra.Command{
		Use:   "app",
		Short: "Create and change Applications",
	}
	app.AddCommand(newAppCreateCommand(deps))
	return app
}

// createOptions are the answers to the wizard's questions, each with a
// flag so the command runs non-interactively.
type createOptions struct {
	name         string
	kind         string
	size         string
	image        string
	port         int
	probePath    string
	platformRepo string
	yes          bool
}

func newAppCreateCommand(deps Dependencies) *cobra.Command {
	var opts createOptions
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create an Application on the Platform",
		Long: "Create an Application on the Platform by writing its prod Environment to the\n" +
			"Platform repository (" + platform.Repository + "): an ArgoCD Application pinned\n" +
			"to the chart version in platform.yaml and the values file that defines the\n" +
			"Environment. The Platform reconciles from there; nothing talks to Kubernetes.\n\n" +
			"Every question has a flag, so the command runs in scripts. Authentication is\n" +
			"the gh CLI's login (gh auth login); write access to the Platform repository is\n" +
			"the authorisation.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runAppCreate(cmd, opts, deps)
		},
	}
	f := cmd.Flags()
	f.StringVar(&opts.name, "name", "", "Application name: lowercase letters, digits and dashes, at most 40 characters, unique on the Platform")
	f.StringVar(&opts.kind, "kind", "", "Kind of Application: web-service (static-site is not available yet)")
	f.StringVar(&opts.size, "size", "small", "Size: small, medium or large")
	f.StringVar(&opts.image, "image", "", "Image repository (default ghcr.io/"+strings.ToLower(platform.Org)+"/<name>)")
	f.IntVar(&opts.port, "port", 3000, "Port the container listens on")
	f.StringVar(&opts.probePath, "probe-path", "/", "Path the readiness and liveness probes request")
	f.BoolVar(&opts.yes, "yes", false, "Skip the confirmation (nothing is asked yet; accepted so scripts keep working once the wizard asks)")
	f.StringVar(&opts.platformRepo, "platform-repo", platform.RepositoryURL, "Git URL of the Platform repository")
	_ = f.MarkHidden("platform-repo")
	_ = cmd.MarkFlagRequired("name")
	_ = cmd.MarkFlagRequired("kind")
	return cmd
}

func runAppCreate(cmd *cobra.Command, opts createOptions, deps Dependencies) error {
	app, err := opts.application()
	if err != nil {
		return err
	}

	token, err := deps.TokenSource.Token()
	if err != nil {
		return fmt.Errorf("not logged in to GitHub, so %s cannot be written: %w", platform.Repository, err)
	}

	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "Creating Application %s on the Platform:\n", app.Name)
	fmt.Fprintf(out, "  Kind:   %s\n", app.Kind)
	fmt.Fprintf(out, "  Size:   %s\n", app.Size)
	fmt.Fprintf(out, "  Image:  %s\n", app.ImageRepository)
	fmt.Fprintf(out, "  Port:   %d\n", app.Port)
	fmt.Fprintf(out, "  Probe:  %s\n", app.ProbePath)
	fmt.Fprintf(out, "Writing the prod Environment to %s...\n", platform.Repository)

	writer := &platformrepo.Writer{
		URL:        opts.platformRepo,
		Auth:       git.Auth{Token: token},
		BeforePush: deps.BeforePush,
	}
	res, err := writer.CreateApplication(cmd.Context(), app)
	if err != nil {
		return err
	}
	printCreated(out, app.Name, res)
	return nil
}

// application validates the flags before anything is cloned or written
// and turns them into the Application to create.
func (o createOptions) application() (platformrepo.Application, error) {
	if err := platformrepo.ValidateName(o.name); err != nil {
		return platformrepo.Application{}, err
	}
	switch o.kind {
	case kindWebService:
	case kindStaticSite:
		return platformrepo.Application{}, fmt.Errorf("the Static site Kind (--kind %s) is not available yet; use --kind %s", kindStaticSite, kindWebService)
	default:
		return platformrepo.Application{}, fmt.Errorf("unknown Kind %q: --kind must be %s (%s is not available yet)", o.kind, kindWebService, kindStaticSite)
	}
	if !contains(sizes, o.size) {
		return platformrepo.Application{}, fmt.Errorf("unknown size %q: --size must be %s", o.size, strings.Join(sizes, ", "))
	}
	if o.port < 1 || o.port > 65535 {
		return platformrepo.Application{}, fmt.Errorf("--port %d is not a port between 1 and 65535", o.port)
	}
	if !strings.HasPrefix(o.probePath, "/") {
		return platformrepo.Application{}, fmt.Errorf("--probe-path %q must start with /", o.probePath)
	}
	image := o.image
	if image == "" {
		image = "ghcr.io/" + strings.ToLower(platform.Org) + "/" + o.name
	}
	return platformrepo.Application{
		Name:            o.name,
		Kind:            o.kind,
		Size:            o.size,
		ImageRepository: image,
		Port:            o.port,
		ProbePath:       o.probePath,
	}, nil
}

func printCreated(out io.Writer, name string, res platformrepo.Result) {
	fmt.Fprintf(out, "\nCreated Application %s. Committed to %s:\n", name, platform.Repository)
	for _, f := range res.Files {
		fmt.Fprintf(out, "  %s\n", f)
	}
	fmt.Fprintf(out, "\n  prod:     %s\n", res.Address)
	if res.Config.ArgoCDURL != "" {
		fmt.Fprintf(out, "  ArgoCD:   %s\n", res.Config.ArgoCDURL)
	}
	if res.Config.GrafanaURL != "" {
		fmt.Fprintf(out, "  Grafana:  %s\n", res.Config.GrafanaURL)
	}
	fmt.Fprintf(out, "\nThe Environment deploys once the deploy workflow writes the first image tag.\n")
}

func contains(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}
