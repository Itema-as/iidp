package cli

import (
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Itema-as/iidp/internal/apprepo"
	"github.com/Itema-as/iidp/internal/git"
	"github.com/Itema-as/iidp/internal/github"
	"github.com/Itema-as/iidp/internal/platform"
	"github.com/Itema-as/iidp/internal/platformrepo"
	"github.com/Itema-as/iidp/internal/templates"
)

// Kinds and sizes the chart accepts.
const (
	kindWebService = "web-service"
	kindStaticSite = "static-site"
)

// The two values --path accepts today. Adopt is refused with a clear
// message: issue #15 builds it. The zero value (no --path) is the legacy
// bare behaviour of app create before this ticket: it writes only the
// Platform repository and creates no Application repository.
const (
	pathCreate = "create"
	pathAdopt  = "adopt"
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
	framework    string
	owner        string
	private      bool
	public       bool
	size         string
	image        string
	port         int
	probePath    string
	path         string
	platformRepo string
	yes          bool
}

func newAppCreateCommand(deps Dependencies) *cobra.Command {
	var opts createOptions
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create an Application on the Platform",
		Long: "Create an Application on the Platform.\n\n" +
			"With --path create, it also creates the Application repository: under\n" +
			"the org by default or, with --owner user, under the developer's personal\n" +
			"account, generated from a built-in framework template (or a commented\n" +
			"Dockerfile stub for --framework other), and pushes the first commit.\n" +
			"Without --path, it writes only the prod Environment to the Platform\n" +
			"repository (" + platform.Repository + "): an ArgoCD Application pinned\n" +
			"to the chart version in platform.yaml and the values file that defines\n" +
			"the Environment. The Platform reconciles from there; nothing talks to\n" +
			"Kubernetes.\n\n" +
			"Every question has a flag, so the command runs in scripts. Authentication is\n" +
			"the gh CLI's login (gh auth login); write access to the Platform repository\n" +
			"(and, with --path create, to the Application's owner) is the authorisation.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runAppCreate(cmd, opts, deps)
		},
	}
	f := cmd.Flags()
	f.StringVar(&opts.name, "name", "", "Application name: lowercase letters, digits and dashes, starting with a letter, at most 40 characters, unique on the Platform")
	f.StringVar(&opts.kind, "kind", "", "Kind of Application: web-service or static-site (derived from --framework with --path create, unless --framework other)")
	f.StringVar(&opts.path, "path", "", "How the Application repository comes to be: create (generate one) or adopt (not available yet, see issue #15). Omit to write only the Platform repository, as before this flag existed")
	f.StringVar(&opts.framework, "framework", "", "Framework to generate the Application repository from (--path create only): nextjs, vite-react or other")
	f.StringVar(&opts.owner, "owner", "org", "Where to create the Application repository (--path create only): org (the compiled-in "+platform.Org+") or user (your personal GitHub account)")
	f.BoolVar(&opts.private, "private", true, "Create the Application repository as private (--path create only; default)")
	f.BoolVar(&opts.public, "public", false, "Create the Application repository as public instead of private (--path create only)")
	f.StringVar(&opts.size, "size", "small", "Size: small, medium or large")
	f.StringVar(&opts.image, "image", "", "Image repository (default ghcr.io/<owner lowercased>/<name>)")
	f.IntVar(&opts.port, "port", 3000, "Port the container listens on")
	f.StringVar(&opts.probePath, "probe-path", "/", "Path the readiness and liveness probes request")
	f.BoolVar(&opts.yes, "yes", false, "Skip the confirmation (nothing is asked yet; accepted so scripts keep working once the wizard asks)")
	f.StringVar(&opts.platformRepo, "platform-repo", platform.RepositoryURL, "Git URL of the Platform repository")
	_ = f.MarkHidden("platform-repo")
	return cmd
}

func runAppCreate(cmd *cobra.Command, opts createOptions, deps Dependencies) error {
	if err := opts.checkRequiredFlags(cmd); err != nil {
		return err
	}
	plan, err := opts.plan(cmd)
	if err != nil {
		return err
	}

	token, err := deps.TokenSource.Token()
	if err != nil {
		return fmt.Errorf("not logged in to GitHub, so %s cannot be written: %w", platform.Repository, err)
	}

	out := cmd.OutOrStdout()
	auth := git.Auth{Token: token}
	platformWriter := &platformrepo.Writer{URL: opts.platformRepo, Auth: auth, BeforePush: deps.BeforePush}

	ownerLogin := platform.Org
	ownerIsOrg := true
	var appRepo apprepo.Result

	if plan.path == pathCreate {
		ghClient := &github.Client{Token: token}
		if deps.GitHubAPI != "" {
			ghClient.BaseURL = deps.GitHubAPI
		}
		if plan.ownerMode == "user" {
			login, err := ghClient.CurrentUser(cmd.Context())
			if err != nil {
				return err
			}
			ownerLogin, ownerIsOrg = login, false
		}

		fmt.Fprintf(out, "Creating Application %s on the Platform (Create path):\n", plan.name)
		fmt.Fprintf(out, "  Framework: %s\n", plan.framework)
		fmt.Fprintf(out, "  Owner:     %s (%s)\n", ownerLogin, plan.ownerMode)
		fmt.Fprintf(out, "  Private:   %t\n", plan.private)

		if err := platformWriter.CheckAvailable(cmd.Context(), plan.name); err != nil {
			return err
		}

		creator := &apprepo.Creator{Client: ghClient, Auth: auth}
		appRepo, err = creator.Create(cmd.Context(), apprepo.Application{
			Name:      plan.name,
			Framework: plan.framework,
			Owner:     apprepo.Owner{Login: ownerLogin, Org: ownerIsOrg},
			Private:   plan.private,
		})
		if err != nil {
			if appRepo.URL != "" {
				fmt.Fprintf(out, "\nCreated the Application repository %s, but: %v\n", appRepo.URL, err)
				fmt.Fprintf(out, "Nothing was written to %s. The repository was left as it is; finish pushing the template by hand and re-run once it succeeds.\n", platform.Repository)
			}
			return err
		}
		fmt.Fprintf(out, "\nCreated and pushed the Application repository %s:\n", appRepo.URL)
		for _, f := range appRepo.Files {
			fmt.Fprintf(out, "  %s\n", f)
		}
	}

	image := plan.imageOverride
	if image == "" {
		image = "ghcr.io/" + strings.ToLower(ownerLogin) + "/" + plan.name
	}
	app := platformrepo.Application{
		Name:            plan.name,
		Kind:            plan.kind,
		Size:            plan.size,
		ImageRepository: image,
		Port:            plan.port,
		ProbePath:       plan.probePath,
	}

	fmt.Fprintf(out, "\nWriting the prod Environment to %s...\n", platform.Repository)
	fmt.Fprintf(out, "  Kind:   %s\n", app.Kind)
	fmt.Fprintf(out, "  Size:   %s\n", app.Size)
	fmt.Fprintf(out, "  Image:  %s\n", app.ImageRepository)
	fmt.Fprintf(out, "  Port:   %d\n", app.Port)
	fmt.Fprintf(out, "  Probe:  %s\n", app.ProbePath)

	res, err := platformWriter.CreateApplication(cmd.Context(), app)
	if err != nil {
		if plan.path == pathCreate {
			fmt.Fprintf(out, "\nThe Application repository %s was created and pushed.\n", appRepo.URL)
			fmt.Fprintf(out, "Writing %s failed: %v\n", platform.Repository, err)
			fmt.Fprintf(out, "Finish by hand: clone %s, add applications/%s/prod/{application.yaml,values.yaml} (see docs/platform-repository.md), commit and push to main. The Application repository is untouched; nothing is deleted.\n", platform.RepositoryURL, plan.name)
		}
		return err
	}
	if plan.path == pathCreate {
		fmt.Fprintf(out, "\nApplication repository: %s\n", appRepo.URL)
	}
	printCreated(out, plan.name, res)
	return nil
}

// checkRequiredFlags reports flags missing for the chosen --path, in the
// style of cobra's own required-flag message ("kind" is not a cobra
// required flag any more because whether it is required depends on
// --framework, which cobra cannot express).
func (o createOptions) checkRequiredFlags(cmd *cobra.Command) error {
	f := cmd.Flags()
	var missing []string
	if !f.Changed("name") {
		missing = append(missing, "name")
	}
	switch o.path {
	case pathCreate:
		if !f.Changed("framework") {
			missing = append(missing, "framework")
		}
	case pathAdopt:
		// Refused with its own message once flags are otherwise valid.
	default:
		if !f.Changed("kind") {
			missing = append(missing, "kind")
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	quoted := make([]string, len(missing))
	for i, m := range missing {
		quoted[i] = strconv.Quote(m)
	}
	return fmt.Errorf("required flag(s) %s not set", strings.Join(quoted, ", "))
}

// createPlan is what createOptions.plan validates the flags into: enough to
// build both the Application repository (when path is pathCreate) and the
// Platform repository's Environment.
type createPlan struct {
	name          string
	kind          string
	framework     templates.Framework
	size          string
	imageOverride string
	port          int
	probePath     string
	path          string
	ownerMode     string
	private       bool
}

// plan validates every flag before anything is cloned or written, and
// turns them into the plan for the chosen --path.
func (o createOptions) plan(cmd *cobra.Command) (createPlan, error) {
	if err := platformrepo.ValidateName(o.name); err != nil {
		return createPlan{}, err
	}

	switch o.path {
	case "", pathCreate:
	case pathAdopt:
		return createPlan{}, errors.New("the Adopt path (--path adopt) is not implemented yet; see issue #15")
	default:
		return createPlan{}, fmt.Errorf("unknown --path %q: must be create or adopt", o.path)
	}

	if o.path != pathCreate {
		for _, name := range []string{"framework", "owner", "private", "public"} {
			if cmd.Flags().Changed(name) {
				return createPlan{}, fmt.Errorf("--%s requires --path create", name)
			}
		}
	}

	kind := o.kind
	var framework templates.Framework
	if o.path == pathCreate {
		framework = templates.Framework(o.framework)
		if !framework.Valid() {
			return createPlan{}, fmt.Errorf("unknown --framework %q: must be nextjs, vite-react or other", o.framework)
		}
		if framework == templates.Other {
			if !cmd.Flags().Changed("kind") {
				return createPlan{}, errors.New(`required flag(s) "kind" not set: --framework other does not choose a Kind`)
			}
		} else {
			if cmd.Flags().Changed("kind") {
				return createPlan{}, fmt.Errorf("--kind must not be set with --framework %s: the Kind is derived from the framework", o.framework)
			}
			kind = framework.Kind()
		}
	}

	switch kind {
	case kindWebService, kindStaticSite:
	default:
		return createPlan{}, fmt.Errorf("unknown Kind %q: --kind must be %s or %s", kind, kindWebService, kindStaticSite)
	}
	if !slices.Contains(sizes, o.size) {
		return createPlan{}, fmt.Errorf("unknown size %q: --size must be %s", o.size, strings.Join(sizes, ", "))
	}
	if o.port < 1 || o.port > 65535 {
		return createPlan{}, fmt.Errorf("--port %d is not a port between 1 and 65535", o.port)
	}
	if !strings.HasPrefix(o.probePath, "/") {
		return createPlan{}, fmt.Errorf("--probe-path %q must start with /", o.probePath)
	}

	ownerMode := "org"
	private := true
	if o.path == pathCreate {
		switch o.owner {
		case "org", "user":
			ownerMode = o.owner
		default:
			return createPlan{}, fmt.Errorf("unknown --owner %q: must be org or user", o.owner)
		}
		if cmd.Flags().Changed("private") && cmd.Flags().Changed("public") {
			return createPlan{}, errors.New("--private and --public are mutually exclusive")
		}
		if cmd.Flags().Changed("public") {
			private = !o.public
		}
		if cmd.Flags().Changed("private") {
			private = o.private
		}
	}

	return createPlan{
		name:          o.name,
		kind:          kind,
		framework:     framework,
		size:          o.size,
		imageOverride: o.image,
		port:          o.port,
		probePath:     o.probePath,
		path:          o.path,
		ownerMode:     ownerMode,
		private:       private,
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
