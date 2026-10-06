package cli

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/Itema-as/iidp/internal/appconfig"
	"github.com/Itema-as/iidp/internal/apprepo"
	"github.com/Itema-as/iidp/internal/git"
	"github.com/Itema-as/iidp/internal/github"
	"github.com/Itema-as/iidp/internal/migrate"
	"github.com/Itema-as/iidp/internal/platform"
	"github.com/Itema-as/iidp/internal/platformrepo"
	"github.com/Itema-as/iidp/internal/prompt"
	"github.com/Itema-as/iidp/internal/render"
	"github.com/Itema-as/iidp/internal/templates"
)

// The values --path accepts: create generates a new Application repository,
// adopt opens a pull request on an existing one. Without --path, app create
// writes only the Platform repository.
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
	app.AddCommand(newAppAddCapabilityCommand(deps))
	app.AddCommand(newAppDeleteCommand(deps))
	app.AddCommand(newAppBindCommand(deps))
	app.AddCommand(newAppStatusCommand(deps))
	return app
}

// createOptions are the answers to the wizard's questions, each with a
// flag so the command runs non-interactively.
type createOptions struct {
	name             string
	kind             string
	framework        string
	private          bool
	public           bool
	size             string
	image            string
	port             int
	probePath        string
	path             string
	repo             string
	platformRepo     string
	yes              bool
	postgres         bool
	migrationCommand string
	appDir           string
	staging          bool
	domains          []string
	login            bool
	loginGroups      []string
	previews         bool
	// interactive forces the wizard without a terminal on stdin, so tests
	// can drive it with an injected reader and writer.
	interactive bool
}

func newAppCreateCommand(deps Dependencies) *cobra.Command {
	var opts createOptions
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create an Application on the Platform",
		Long: "Create an Application on the Platform.\n\n" +
			"With --path create, it also creates the Application repository in\n" +
			platform.Org + ", generated from a built-in framework template (or a\n" +
			"commented Dockerfile stub for --framework other), and pushes the first\n" +
			"commit. With --path adopt --repo " + platform.Org + "/<name>, it instead opens a\n" +
			"pull request on that existing repository, adding only a Dockerfile (when\n" +
			"it has none, generated from its detected framework), the deploy workflow\n" +
			"and iidp.yaml, which holds the migration command each deploy sends the\n" +
			"Platform. Only repositories in " + platform.Org + " can be Applications: transfer\n" +
			"one there before adopting it. Both paths bind the Application to its\n" +
			"repository by GitHub's numeric ids, so a rename keeps it deployable.\n" +
			"Without --path, it writes only the prod Environment to the Platform\n" +
			"repository (" + platform.Repository + "): an ArgoCD Application pinned\n" +
			"to the chart version in platform.yaml and the values file that defines\n" +
			"the Environment. The Platform reconciles from there; nothing talks to\n" +
			"Kubernetes.\n\n" +
			"Every question has a flag, so the command runs in scripts. Authentication is\n" +
			"the gh CLI's login (gh auth login); write access to the Platform repository\n" +
			"(and, with --path create or adopt, to the Application's repository) is the\n" +
			"authorisation. With --path create or adopt, the login also needs the\n" +
			"workflow scope, because both push .github/workflows/deploy.yaml; add it\n" +
			"with gh auth refresh -s workflow. The command checks this before it\n" +
			"creates anything.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runAppCreate(cmd, &opts, deps)
		},
	}
	f := cmd.Flags()
	f.StringVar(&opts.name, "name", "", "Application name: lowercase letters, digits and dashes, starting with a letter, at most 40 characters, unique on the Platform (defaults to the repository name with --path adopt)")
	f.StringVar(&opts.kind, "kind", "", "Kind of Application: web-service or static-site (derived from --framework with --path create, unless --framework other; required with --path adopt when the repository already has a Dockerfile or no known framework is detected)")
	f.StringVar(&opts.path, "path", "", "How the Application repository comes to be: create (generate one) or adopt (open a pull request on an existing one). Omit to write only the Platform repository")
	f.StringVar(&opts.repo, "repo", "", "Existing Application repository to adopt, in "+platform.Org+": "+platform.Org+"/name or a URL (--path adopt only)")
	f.StringVar(&opts.framework, "framework", "", "Framework to generate the Application repository from (--path create only): nextjs, vite-react or other")
	f.BoolVar(&opts.private, "private", true, "Create the Application repository as private (--path create only; default)")
	f.BoolVar(&opts.public, "public", false, "Create the Application repository as public instead of private (--path create only)")
	f.StringVar(&opts.size, "size", "small", "Size: small, medium or large")
	f.StringVar(&opts.image, "image", "", "Image repository (default ghcr.io/<owner lowercased>/<name>)")
	f.IntVar(&opts.port, "port", 3000, "Port the container listens on")
	f.StringVar(&opts.probePath, "probe-path", "/", "Path the readiness and liveness probes request")
	f.BoolVar(&opts.yes, "yes", false, "Skip the wizard's confirmation before anything is written")
	f.BoolVar(&opts.postgres, "postgres", false, "Add a Postgres database Capability: DATABASE_URL injected into every Environment, continuous backups")
	f.StringVar(&opts.migrationCommand, "migration-command", "", "Shell command run before every rollout with DATABASE_URL set, written to the Application repository's iidp.yaml (requires --postgres); detected from Prisma, Drizzle or an npm migrate script when omitted")
	f.StringVar(&opts.appDir, "app-dir", "", "Directory to detect the migration command in (default: the generated template with --path create, or the current directory when it has a package.json)")
	_ = f.MarkHidden("app-dir")
	f.BoolVar(&opts.staging, "staging", false, "Add a staging Environment next to prod: its own address, its own database, the same Capabilities")
	f.StringArrayVar(&opts.domains, "domain", nil, "Custom domain to serve besides the Platform address, for prod only (repeatable)")
	f.BoolVar(&opts.login, "login", false, "Require Itema (Entra ID) sign-in on every address of every Environment, custom domains included; every --domain must then be inside platform.yaml's cloudflareZone, the sign-in cookie's domain, which the browser sends to every host in it")
	f.StringArrayVar(&opts.loginGroups, "login-group", nil, "Entra group object id (a GUID) whose members may sign in; repeatable, a member of any one gets in. Needs --login. Without it, every Itema user gets in")
	f.BoolVar(&opts.previews, "previews", false, "Give every open pull request labelled "+render.PreviewLabel+" on the Application repository a Preview Environment at <name>-pr-<number>.<baseDomain>: staging's values and secrets, the smallest size, Itema login, an empty database without backups, removed when the pull request closes. Needs --staging, and --path create or adopt")
	f.StringVar(&opts.platformRepo, "platform-repo", platform.RepositoryURL, "Git URL of the Platform repository")
	_ = f.MarkHidden("platform-repo")
	f.BoolVar(&opts.interactive, "interactive", false, "Run the wizard even without a terminal on stdin")
	_ = f.MarkHidden("interactive")
	return cmd
}

// isInteractive reports whether app create should run the wizard.
//
// It uses term.IsTerminal rather than os.ModeCharDevice: /dev/null is also a
// character device, and scripts and CI runners redirect stdin from it.
func isInteractive(opts createOptions) bool {
	if opts.interactive {
		return true
	}
	return term.IsTerminal(int(os.Stdin.Fd()))
}

func runAppCreate(cmd *cobra.Command, opts *createOptions, deps Dependencies) error {
	out := cmd.OutOrStdout()
	interactive := isInteractive(*opts)
	// One Prompter for the whole command: prompt.New wraps its reader in a
	// bufio.Reader, which reads ahead of what it returns, so a second
	// Prompter over the same underlying reader (the confirmation, below)
	// would lose whatever the first one had already buffered.
	p := prompt.New(cmd.InOrStdin(), out)

	if interactive {
		if err := runWizard(cmd, opts, p); err != nil {
			return err
		}
	} else if err := opts.checkRequiredFlags(cmd); err != nil {
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

	auth := git.Auth{Token: token}
	platformWriter := &platformrepo.Writer{URL: opts.platformRepo, Auth: auth, BeforePush: deps.BeforePush}

	ghClient := &github.Client{Token: token}
	if deps.GitHubAPI != "" {
		ghClient.BaseURL = deps.GitHubAPI
	}

	// Create and Adopt push .github/workflows/deploy.yaml, which GitHub
	// refuses without the workflow scope. Create always adds it, so it is
	// checked here; Adopt adds it only when the repository has none, which
	// is known once it is cloned, so Adopt checks after detection.
	var scopes github.TokenScopes
	if plan.path == pathCreate || plan.path == pathAdopt {
		scopes, err = ghClient.TokenScopes(cmd.Context())
		if err != nil {
			return err
		}
	}
	if plan.path == pathCreate {
		if err := apprepo.CheckWorkflowScope(scopes); err != nil {
			return err
		}
	}

	// Every Application repository is in the org, so its image is in the
	// org's GHCR namespace.
	image := plan.imageOverride
	if image == "" {
		image = platform.Registry + "/" + plan.name
	}
	// For Adopt, kind and migrationCommand depend on the cloned repository:
	// they are resolved for the interactive summary by Adopter.Preview, and
	// finally by Adopter.Adopt.
	kind := plan.kind
	migrationCommand := plan.migrationCommand
	if plan.path != pathAdopt {
		migrationCommand, err = detectMigrationCommand(plan, out)
		if err != nil {
			return err
		}
	}

	adopter := &apprepo.Adopter{Client: ghClient, Auth: auth}

	if interactive && !opts.yes {
		summaryKind := kind
		summaryMigration := migrationCommand
		var adoptFiles []string
		if plan.path == pathAdopt {
			preview, err := adopter.Preview(cmd.Context(), plan.repoOwner, plan.repoName)
			if err != nil {
				return err
			}
			if err := checkAdoptPreview(preview, scopes, plan.repoOwner, plan.repoName); err != nil {
				return err
			}
			summaryKind, err = apprepo.ResolveKind(plan.kind, preview.Detection)
			if err != nil {
				return fmt.Errorf("%s/%s: %w", plan.repoOwner, plan.repoName, err)
			}
			summaryMigration = preview.Detection.MigrationCommand(plan.postgres, plan.migrationCommand, plan.migrationCommandSet)
			adoptFiles = preview.Detection.Files(summaryKind)
		}
		if err := checkPostgresKind(plan.postgres, summaryKind); err != nil {
			return err
		}
		app := platformrepo.Application{
			Name:            plan.name,
			Kind:            summaryKind,
			Size:            plan.size,
			ImageRepository: image,
			Port:            plan.port,
			ProbePath:       plan.probePath,
			Postgres:        plan.postgres,
			Staging:         plan.staging,
			Domains:         plan.domains,
			Login:           plan.login,
			LoginGroups:     plan.loginGroups,
			Previews:        plan.previews,
		}
		// --previews needs a bound repository, but Create and Adopt bind it
		// only once it exists. For the summary, PreviewApplication gets a
		// stand-in binding with placeholder ids, so everything else is still
		// checked.
		if plan.previews {
			app.Repository = &platformrepo.RepositoryBinding{Repository: plan.repoOwner + "/" + plan.repoName, RepositoryID: 1, RepositoryOwnerID: 1}
			if plan.path == pathCreate {
				app.Repository.Repository = platform.Org + "/" + plan.name
			}
		}
		preview, err := platformWriter.PreviewApplication(cmd.Context(), app)
		if err != nil {
			return err
		}
		printSummary(out, plan, app, summaryMigration, preview, adoptFiles)
		proceed, err := p.YesNo("Proceed?", true)
		if err != nil {
			return err
		}
		if !proceed {
			fmt.Fprintln(out, "Nothing created: declined at the summary.")
			return nil
		}
	}

	var appRepo apprepo.Result
	if plan.path == pathCreate {
		fmt.Fprintf(out, "Creating Application %s on the Platform (Create path):\n", plan.name)
		fmt.Fprintf(out, "  Framework: %s\n", plan.framework)
		fmt.Fprintf(out, "  Owner:     %s\n", platform.Org)
		fmt.Fprintf(out, "  Private:   %t\n", plan.private)

		cfg, err := platformWriter.CheckAvailable(cmd.Context(), plan.name)
		if err != nil {
			return err
		}

		creator := &apprepo.Creator{Client: ghClient, Auth: auth}
		appRepo, err = creator.Create(cmd.Context(), apprepo.Application{
			Name:             plan.name,
			Framework:        plan.framework,
			Private:          plan.private,
			DeployGateURL:    cfg.DeployGateURL(),
			MigrationCommand: migrationCommand,
		})
		if err != nil {
			if appRepo.URL != "" {
				fmt.Fprintf(out, "\nCreated the Application repository %s, but: %v\n", appRepo.URL, err)
				fmt.Fprintf(out, "Nothing was written to %s. Running this command again with the same --name will refuse: %s already exists. Either push the rendered template to %s by hand and finish with the Platform-repository step yourself (see docs/platform-repository.md, then %s), or delete the repository on GitHub and run this command again.\n", platform.Repository, appRepo.URL, appRepo.CloneURL, bindCommand(plan.name, platform.Org+"/"+plan.name))
			}
			return err
		}
		fmt.Fprintf(out, "\nCreated and pushed the Application repository %s:\n", appRepo.URL)
		for _, f := range appRepo.Files {
			fmt.Fprintf(out, "  %s\n", f)
		}
		fmt.Fprintf(out, "\nThe deploy workflow (.github/workflows/deploy.yaml) calls %s, so every iidp release in that major version reaches it with no edit.\n", appRepo.DeployWorkflowRef)
	}

	var adoptResult apprepo.AdoptResult
	if plan.path == pathAdopt {
		fmt.Fprintf(out, "Adopting %s/%s onto the Platform:\n", plan.repoOwner, plan.repoName)

		// Check the name before the pull request is opened; the same clone
		// gives the base domain for the Deploy gate's URL.
		cfg, err := platformWriter.CheckAvailable(cmd.Context(), plan.name)
		if err != nil {
			return err
		}

		// Adopter.Adopt checks --postgres against the Kind once it has
		// resolved it, before anything is pushed: a refusal must never come
		// after the pull request exists.
		adoptResult, err = adopter.Adopt(cmd.Context(), apprepo.AdoptRequest{
			Owner:               plan.repoOwner,
			Name:                plan.repoName,
			AppName:             plan.name,
			Kind:                kind,
			DeployGateURL:       cfg.DeployGateURL(),
			Postgres:            plan.postgres,
			Scopes:              scopes,
			MigrationCommand:    plan.migrationCommand,
			MigrationCommandSet: plan.migrationCommandSet,
		})
		if err != nil {
			return err
		}
		kind = adoptResult.Kind
		migrationCommand = adoptResult.MigrationCommand
		if !plan.migrationCommandSet {
			switch {
			case plan.postgres && adoptResult.MigrationOK:
				fmt.Fprintf(out, "Detected %s in %s/%s; migration command: %s\n", adoptResult.Migration.Tool, plan.repoOwner, plan.repoName, migrationCommand)
			case plan.postgres:
				fmt.Fprintf(out, "No migration tooling detected; %s sets no migration command. Add one there if the Application has migrations.\n", appconfig.FileName)
			}
		}
		if adoptResult.HasAppConfig && migrationCommand != "" {
			fmt.Fprintf(out, "%s/%s already has %s, which Adopt leaves as it is. Make sure it has this line:\n  %s\n", plan.repoOwner, plan.repoName, appconfig.FileName, appconfig.MigrationCommandLine(migrationCommand))
		}

		fmt.Fprintf(out, "\nOpened a pull request on %s, branch %s: %s\n", adoptResult.RepoURL, adoptResult.Branch, adoptResult.PullRequestURL)
		fmt.Fprintln(out, "It adds:")
		for _, f := range adoptResult.Files {
			if f == "Dockerfile" && adoptResult.HasDockerfile {
				fmt.Fprintf(out, "  %s (changed, to run as non-root: %s)\n", f, adoptResult.NonRoot.Change)
				continue
			}
			fmt.Fprintf(out, "  %s\n", f)
		}
		fmt.Fprintln(out, "The first merged run of its deploy workflow deploys the Application.")
		if adoptResult.NonRoot.Advice != "" {
			fmt.Fprintf(out, "Its Dockerfile may run as root, and the Platform refuses to start it then. Change it before merging: %s\n", adoptResult.NonRoot.Advice)
		}
	}

	// Without --path there is no Application repository to bind.
	var binding *platformrepo.RepositoryBinding
	switch plan.path {
	case pathCreate:
		binding = &appRepo.Binding
	case pathAdopt:
		binding = &adoptResult.Binding
	}

	app := platformrepo.Application{
		Name:            plan.name,
		Kind:            kind,
		Size:            plan.size,
		ImageRepository: image,
		Port:            plan.port,
		ProbePath:       plan.probePath,
		Postgres:        plan.postgres,
		Staging:         plan.staging,
		Domains:         plan.domains,
		Login:           plan.login,
		LoginGroups:     plan.loginGroups,
		Repository:      binding,
		Previews:        plan.previews,
	}

	fmt.Fprintf(out, "\nWriting the prod Environment to %s...\n", platform.Repository)
	fmt.Fprintf(out, "  Kind:   %s\n", app.Kind)
	fmt.Fprintf(out, "  Size:   %s\n", app.Size)
	fmt.Fprintf(out, "  Image:  %s\n", app.ImageRepository)
	fmt.Fprintf(out, "  Port:   %d\n", app.Port)
	fmt.Fprintf(out, "  Probe:  %s\n", app.ProbePath)
	if app.Postgres {
		fmt.Fprintln(out, "  Postgres: enabled")
	}
	if app.Staging {
		fmt.Fprintln(out, "  Staging:  a second Environment, its own address and database")
	}
	if app.Login {
		fmt.Fprintln(out, "  Login:    Itema (Entra ID) sign-in required on every address")
		fmt.Fprintf(out, "  Sign-in groups: %s\n", signInGroupsText(app.LoginGroups))
	}
	if app.Previews {
		fmt.Fprintf(out, "  Previews: one for each open pull request labelled %s\n", render.PreviewLabel)
	}
	if binding != nil {
		fmt.Fprintf(out, "  Repository: %s (repository id %d, owner id %d)\n", binding.Repository, binding.RepositoryID, binding.RepositoryOwnerID)
	}

	res, err := platformWriter.CreateApplication(cmd.Context(), app, out)
	if err != nil {
		switch plan.path {
		case pathCreate:
			fmt.Fprintf(out, "\nThe Application repository %s was created and pushed.\n", appRepo.URL)
			fmt.Fprintf(out, "Writing %s failed: %v\n", platform.Repository, err)
			fmt.Fprintf(out, "Finish by hand: clone %s, add applications/%s/prod/{application.yaml,values.yaml} (see docs/platform-repository.md), commit and push to main, then run %s. The Application repository is untouched; nothing is deleted.\n", platform.RepositoryURL, plan.name, bindCommand(plan.name, binding.Repository))
		case pathAdopt:
			fmt.Fprintf(out, "\nThe pull request %s was opened.\n", adoptResult.PullRequestURL)
			fmt.Fprintf(out, "Writing %s failed: %v\n", platform.Repository, err)
			fmt.Fprintf(out, "Finish by hand: clone %s, add applications/%s/prod/{application.yaml,values.yaml} (see docs/platform-repository.md), commit and push to main, then run %s. The pull request is untouched; nothing is deleted.\n", platform.RepositoryURL, plan.name, bindCommand(plan.name, binding.Repository))
		}
		return err
	}
	switch plan.path {
	case pathCreate:
		fmt.Fprintf(out, "\nApplication repository: %s\n", appRepo.URL)
	case pathAdopt:
		fmt.Fprintf(out, "\nPull request: %s\n", adoptResult.PullRequestURL)
	}
	printCreated(out, plan.name, res)
	if plan.postgres {
		switch {
		case plan.path == pathCreate && migrationCommand != "":
			fmt.Fprintf(out, "\nMigration command, in the Application repository's %s: %s\nEach deploy sends it to the Platform with the image it belongs to.\n", appconfig.FileName, migrationCommand)
		case plan.path == pathAdopt && migrationCommand != "" && !adoptResult.HasAppConfig:
			fmt.Fprintf(out, "\nMigration command, in the pull request's %s: %s\nEach deploy sends it to the Platform with the image it belongs to.\n", appconfig.FileName, migrationCommand)
		case plan.path != pathCreate && plan.path != pathAdopt:
			printMigrationCommandToAdd(out, plan.name, migrationCommand)
		}
	}
	if binding == nil {
		fmt.Fprintf(out, "\nThis Application is not bound to an Application repository, so no repository can deploy it. Once its repository is in %s, bind it: %s\n", platform.Org, bindCommand(plan.name, platform.Org+"/<repository>"))
	}
	return nil
}

// printMigrationCommandToAdd tells the developer what to put in iidp.yaml
// when the CLI cannot write it there itself.
func printMigrationCommandToAdd(out io.Writer, name, command string) {
	fmt.Fprintf(out, "\nThe migration command lives in %s at the root of %s's Application repository: the deploy workflow sends it to the Platform with every deploy, so it always matches the code it migrates.\n", appconfig.FileName, name)
	if command != "" {
		fmt.Fprintf(out, "Add this line to %s and push:\n  %s\n", appconfig.FileName, appconfig.MigrationCommandLine(command))
		return
	}
	fmt.Fprintf(out, "If %s has migrations, add a line like this to %s and push:\n  %s\n", name, appconfig.FileName, appconfig.MigrationCommandLine("npx prisma migrate deploy"))
}

// bindCommand is the iidp app bind invocation that binds name to repo.
func bindCommand(name, repo string) string {
	return "iidp app bind " + name + " --repo " + repo
}

// checkPostgresKind refuses --postgres for a Static site. It is a no-op when
// kind is "" (Adopt, before detection); Adopter.Adopt checks again later.
func checkPostgresKind(postgres bool, kind string) error {
	if postgres && kind == platformrepo.KindStaticSite {
		return errors.New("--postgres needs --kind web-service (or a framework/detected framework that derives it); a Static site has no server to use a database")
	}
	return nil
}

// checkAdoptPreview returns the errors Adopter.Adopt would, so the wizard
// refuses before the summary rather than after the developer confirms.
func checkAdoptPreview(preview apprepo.AdoptPreview, scopes github.TokenScopes, owner, name string) error {
	if !preview.CanPush {
		return fmt.Errorf("%w: you do not have write access to %s/%s; Adopt needs it to open a pull request", apprepo.ErrNoPushAccess, owner, name)
	}
	if preview.BranchExists {
		return fmt.Errorf("%w: %s already exists on %s/%s; merge or delete it before adopting again", apprepo.ErrBranchExists, apprepo.AdoptBranch, owner, name)
	}
	if !preview.HasDeployWorkflow {
		return apprepo.CheckWorkflowScope(scopes)
	}
	return nil
}

// checkRequiredFlags reports flags missing for the chosen --path, in the
// style of cobra's required-flag message. Cobra cannot express that --kind
// is required only for some --path and --framework values.
func (o createOptions) checkRequiredFlags(cmd *cobra.Command) error {
	f := cmd.Flags()
	var missing []string
	switch o.path {
	case pathCreate:
		if !f.Changed("name") {
			missing = append(missing, "name")
		}
		if !f.Changed("framework") {
			missing = append(missing, "framework")
		}
	case pathAdopt:
		// --name defaults to the repository name. Whether --kind is needed
		// depends on detection, so Adopter.Adopt checks it.
		if !f.Changed("repo") {
			missing = append(missing, "repo")
		}
	default:
		if !f.Changed("name") {
			missing = append(missing, "name")
		}
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

// createPlan is the validated form of createOptions.
type createPlan struct {
	name          string
	kind          string
	framework     templates.Framework
	size          string
	imageOverride string
	port          int
	probePath     string
	path          string
	// repoOwner and repoName are --repo, parsed.
	repoOwner        string
	repoName         string
	private          bool
	postgres         bool
	migrationCommand string
	// migrationCommandSet is true when --migration-command was given, even
	// empty (the wizard sets it on an empty answer); detection is then
	// skipped.
	migrationCommandSet bool
	appDir              string
	staging             bool
	domains             []string
	login               bool
	loginGroups         []string
	previews            bool
}

// plan validates every flag before anything is cloned or written.
func (o createOptions) plan(cmd *cobra.Command) (createPlan, error) {
	switch o.path {
	case "", pathCreate, pathAdopt:
	default:
		return createPlan{}, fmt.Errorf("unknown --path %q: must be create or adopt", o.path)
	}

	if o.path != pathCreate {
		for _, name := range []string{"framework", "private", "public"} {
			if cmd.Flags().Changed(name) {
				return createPlan{}, fmt.Errorf("--%s requires --path create", name)
			}
		}
	}
	if o.path != pathAdopt && cmd.Flags().Changed("repo") {
		return createPlan{}, errors.New("--repo requires --path adopt")
	}

	name := o.name
	var repoOwner, repoName string
	if o.path == pathAdopt {
		if o.repo == "" {
			return createPlan{}, errors.New("--repo is required with --path adopt: the existing Application repository to open a pull request on, owner/name or a URL")
		}
		var err error
		repoOwner, repoName, err = parseRepoFlag(o.repo)
		if err != nil {
			return createPlan{}, err
		}
		// Adopter checks again against the owner GitHub reports.
		if err := apprepo.CheckInOrg(repoOwner, repoName); err != nil {
			return createPlan{}, err
		}
		if name == "" {
			name = repoName
		}
	}
	if err := platformrepo.ValidateNewName(name); err != nil {
		return createPlan{}, err
	}

	kind := o.kind
	var framework templates.Framework
	switch o.path {
	case pathCreate:
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
		if !platformrepo.ValidKind(kind) {
			return createPlan{}, fmt.Errorf("unknown Kind %q: --kind must be %s or %s", kind, platformrepo.KindWebService, platformrepo.KindStaticSite)
		}
	case pathAdopt:
		// Whether Kind is required depends on what the clone contains, so
		// apprepo.ResolveKind refuses later. An explicit --kind is validated
		// here.
		if cmd.Flags().Changed("kind") {
			if !platformrepo.ValidKind(kind) {
				return createPlan{}, fmt.Errorf("unknown Kind %q: --kind must be %s or %s", kind, platformrepo.KindWebService, platformrepo.KindStaticSite)
			}
		} else {
			kind = ""
		}
	default:
		if !platformrepo.ValidKind(kind) {
			return createPlan{}, fmt.Errorf("unknown Kind %q: --kind must be %s or %s", kind, platformrepo.KindWebService, platformrepo.KindStaticSite)
		}
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
	if o.migrationCommand != "" && !o.postgres {
		return createPlan{}, errors.New("--migration-command requires --postgres: there is no database to migrate")
	}
	if err := appconfig.ValidateMigrationCommand(o.migrationCommand); err != nil {
		return createPlan{}, fmt.Errorf("--migration-command: %w", err)
	}
	if err := checkPostgresKind(o.postgres, kind); err != nil {
		return createPlan{}, err
	}
	loginGroups, err := parseLoginGroups(o.loginGroups)
	if err != nil {
		return createPlan{}, err
	}
	if cmd.Flags().Changed("login-group") && !o.login {
		return createPlan{}, fmt.Errorf("%w: give --login with --login-group", platformrepo.ErrLoginGroupsWithoutLogin)
	}
	// A Preview Environment uses staging's values and secrets, and follows
	// the pull requests of the bound repository.
	if o.previews && !o.staging {
		return createPlan{}, platformrepo.ErrPreviewsWithoutStaging
	}
	if o.previews && o.path != pathCreate && o.path != pathAdopt {
		return createPlan{}, fmt.Errorf("%w: give --path create or --path adopt, which bind it", platformrepo.ErrPreviewsWithoutBinding)
	}

	private := true
	if o.path == pathCreate {
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
		name:                name,
		kind:                kind,
		framework:           framework,
		size:                o.size,
		imageOverride:       o.image,
		port:                o.port,
		probePath:           o.probePath,
		path:                o.path,
		repoOwner:           repoOwner,
		repoName:            repoName,
		private:             private,
		postgres:            o.postgres,
		migrationCommand:    o.migrationCommand,
		migrationCommandSet: cmd.Flags().Changed("migration-command"),
		appDir:              o.appDir,
		staging:             o.staging,
		domains:             o.domains,
		login:               o.login,
		loginGroups:         loginGroups,
		previews:            o.previews,
	}, nil
}

// parseLoginGroups normalises --login-group values. A single empty value
// means no groups, which is how add-capability removes them; an empty value
// next to ids is refused rather than silently dropped.
func parseLoginGroups(values []string) ([]string, error) {
	if len(values) == 1 && strings.TrimSpace(values[0]) == "" {
		return []string{}, nil
	}
	for _, v := range values {
		if strings.TrimSpace(v) == "" {
			return nil, errors.New("--login-group '' removes every sign-in group and cannot be given together with group ids")
		}
	}
	return platformrepo.NormalizeLoginGroups(values)
}

func signInGroupsText(groups []string) string {
	if len(groups) == 0 {
		return "none, every Itema user gets in"
	}
	return strings.Join(groups, ", ") + " (a member of any one gets in)"
}

// parseRepoFlag splits --repo, owner/name or a GitHub URL, into owner and
// name.
func parseRepoFlag(s string) (owner, name string, err error) {
	trimmed := strings.TrimSpace(s)
	p := trimmed
	if strings.Contains(trimmed, "://") {
		u, err := url.Parse(trimmed)
		if err != nil {
			return "", "", fmt.Errorf("--repo %q is not a valid URL: %w", s, err)
		}
		p = strings.Trim(u.Path, "/")
	}
	p = strings.TrimSuffix(p, ".git")
	parts := strings.Split(p, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("--repo %q must be owner/name or a GitHub URL such as https://github.com/owner/name", s)
	}
	return parts[0], parts[1], nil
}

// resolveMigrationDir picks the directory to detect the migration command
// in: --app-dir, then the Create path's rendered template, then the current
// directory when it has a package.json. dir is "" when there is nowhere to
// look. cleanup must always be called.
func resolveMigrationDir(plan createPlan) (dir, description string, cleanup func(), err error) {
	noop := func() {}
	switch {
	case plan.appDir != "":
		return plan.appDir, plan.appDir, noop, nil
	case plan.path == pathCreate:
		tmp, err := os.MkdirTemp("", "iidp-detect-")
		if err != nil {
			return "", "", noop, err
		}
		if _, err := templates.Render(plan.framework, templates.Data{Name: plan.name}, tmp); err != nil {
			os.RemoveAll(tmp)
			return "", "", noop, err
		}
		return tmp, "the generated " + string(plan.framework) + " template", func() { os.RemoveAll(tmp) }, nil
	default:
		if cwd, err := os.Getwd(); err == nil {
			if info, err := os.Stat(filepath.Join(cwd, "package.json")); err == nil && !info.IsDir() {
				return cwd, cwd + " (the current directory)", noop, nil
			}
		}
		return "", "", noop, nil
	}
}

// detectMigrationCommand returns the explicit --migration-command, or with
// --postgres detects one, printing what it found to out.
func detectMigrationCommand(plan createPlan, out io.Writer) (string, error) {
	if plan.migrationCommandSet {
		return plan.migrationCommand, nil
	}
	if !plan.postgres {
		return "", nil
	}

	dir, description, cleanup, err := resolveMigrationDir(plan)
	if err != nil {
		return "", err
	}
	defer cleanup()
	if dir == "" {
		return "", nil
	}
	fmt.Fprintf(out, "Looking for a migration command in %s...\n", description)

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

// suggestMigrationCommand runs detectMigrationCommand's detection without
// printing, for the wizard's suggestion.
func suggestMigrationCommand(plan createPlan) (migrate.Detection, bool, error) {
	dir, _, cleanup, err := resolveMigrationDir(plan)
	if err != nil {
		return migrate.Detection{}, false, err
	}
	defer cleanup()
	if dir == "" {
		return migrate.Detection{}, false, nil
	}
	return migrate.Detect(dir)
}

func printCreated(out io.Writer, name string, res platformrepo.Result) {
	fmt.Fprintf(out, "\nCreated Application %s. Committed to %s:\n", name, platform.Repository)
	for _, f := range res.Files {
		fmt.Fprintf(out, "  %s\n", f)
	}
	fmt.Fprintf(out, "\n  prod:     %s\n", res.Address)
	if res.StagingAddress != "" {
		fmt.Fprintf(out, "  staging:  %s\n", res.StagingAddress)
	}
	printPreviews(out, res)
	if res.Login {
		fmt.Fprintf(out, "  Login:    Itema (Entra ID) sign-in required; one sign-in covers every protected address inside %s\n", res.Config.LoginCookieDomain())
		fmt.Fprintf(out, "  Sign-in groups: %s\n", signInGroupsText(res.LoginGroups))
	}
	if res.Config.ArgoCDURL != "" {
		fmt.Fprintf(out, "  ArgoCD:   %s\n", res.Config.ArgoCDURL)
	}
	if res.Config.GrafanaURL != "" {
		fmt.Fprintf(out, "  Grafana:  %s\n", res.Config.GrafanaURL)
	}
	printDomains(out, res)
	printLoginCallbacks(out, res)
	fmt.Fprintf(out, "\nThe Environment deploys once the deploy workflow writes the first image tag.\n")
}

func printPreviews(out io.Writer, res platformrepo.Result) {
	if res.PreviewAddress == "" {
		return
	}
	fmt.Fprintf(out, "  previews: %s, for each open pull request labelled %s\n", res.PreviewAddress, render.PreviewLabel)
	fmt.Fprintf(out, "            within 3 minutes of the label, once the deploy workflow has pushed the pull request's image; removed when it closes or loses the label\n")
}

// printDomains reports how each custom domain gets its certificate and DNS,
// and lists the CNAME records left to create by hand.
func printDomains(out io.Writer, res platformrepo.Result) {
	if len(res.Domains) == 0 {
		return
	}
	fmt.Fprintln(out, "\nCustom domains:")
	var cnames []platformrepo.DomainPlan
	for _, d := range res.Domains {
		switch {
		case d.Wildcard:
			fmt.Fprintf(out, "  %s: covered by the Platform's wildcard certificate, DNS automatic\n", d.Host)
		case d.Automated:
			fmt.Fprintf(out, "  %s: certificate from platform.httpIssuer, DNS automatic (inside the Cloudflare zone)\n", d.Host)
		default:
			fmt.Fprintf(out, "  %s: certificate from platform.httpIssuer, DNS not automatic\n", d.Host)
			cnames = append(cnames, d)
		}
	}
	if len(cnames) == 0 {
		return
	}
	target := strings.TrimPrefix(res.Address, "https://")
	fmt.Fprintln(out, "\nAdd these DNS records:")
	for _, d := range cnames {
		fmt.Fprintf(out, "  CNAME %s -> %s\n", d.Host, target)
	}
}

// entraLoginApp is the display name the bootstrap wizard gives oauth2-proxy's
// Entra app registration.
const entraLoginApp = "iidp-oauth2-proxy"

// printLoginCallbacks lists the redirect URIs the Entra app registration
// needs for custom domains outside the login cookie domain, each of which
// signs in on its own host. It prints the az command and the portal steps
// for the Platform admin, and runs neither.
func printLoginCallbacks(out io.Writer, res platformrepo.Result) {
	if len(res.LoginCallbackHosts) == 0 {
		return
	}
	uris := make([]string, 0, len(res.LoginCallbackHosts))
	for _, host := range res.LoginCallbackHosts {
		uris = append(uris, platformrepo.LoginCallbackURL(host))
	}
	fmt.Fprintf(out, "\nItema login on custom domains outside %s signs in on each host. Ask the Platform admin to add these redirect URIs to the %s app registration in Entra ID:\n", res.Config.LoginCookieDomain(), entraLoginApp)
	for _, uri := range uris {
		fmt.Fprintf(out, "  %s\n", uri)
	}
	// --web-redirect-uris replaces the whole list, so the command reads
	// the URIs already there and writes them back with the new ones.
	fmt.Fprintln(out, "With az, keeping the redirect URIs already there:")
	fmt.Fprintf(out, "  app=$(az ad app list --display-name %s --query '[0].appId' -o tsv)\n", entraLoginApp)
	fmt.Fprintf(out, "  az ad app update --id \"$app\" --web-redirect-uris $( (az ad app show --id \"$app\" --query 'web.redirectUris[]' -o tsv; printf '%%s\\n' %s) | sort -u)\n", strings.Join(uris, " "))
	fmt.Fprintf(out, "Or in the Microsoft Entra admin center: App registrations > %s > Authentication > Web > Redirect URIs > Add URI, once per URI, then Save.\n", entraLoginApp)
	fmt.Fprintln(out, "Until then, signing in on these hosts ends at Entra ID's error AADSTS50011 (redirect URI mismatch).")
}
