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

	"github.com/Itema-as/iidp/internal/apprepo"
	"github.com/Itema-as/iidp/internal/git"
	"github.com/Itema-as/iidp/internal/github"
	"github.com/Itema-as/iidp/internal/migrate"
	"github.com/Itema-as/iidp/internal/platform"
	"github.com/Itema-as/iidp/internal/platformrepo"
	"github.com/Itema-as/iidp/internal/prompt"
	"github.com/Itema-as/iidp/internal/templates"
)

// The two values --path accepts: create generates a new Application
// repository (issue #11), adopt opens a pull request on an existing one
// (issue #15). The zero value (no --path) is the legacy bare behaviour of
// app create from before either existed: it writes only the Platform
// repository and creates no Application repository.
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
	return app
}

// createOptions are the answers to the wizard's questions, each with a
// flag so the command runs non-interactively.
type createOptions struct {
	name             string
	kind             string
	framework        string
	owner            string
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
	// interactive forces the wizard even without a terminal on stdin: the
	// hidden --interactive flag, so tests can drive it with an injected
	// reader and writer (docs/implementation-notes/14-cli-wizard.md).
	interactive bool
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
			"With --path adopt --repo <owner>/<name>, it instead opens a pull request\n" +
			"on that existing repository, adding only a Dockerfile (when it has none,\n" +
			"generated from its detected framework) and the deploy workflow.\n" +
			"Without --path, it writes only the prod Environment to the Platform\n" +
			"repository (" + platform.Repository + "): an ArgoCD Application pinned\n" +
			"to the chart version in platform.yaml and the values file that defines\n" +
			"the Environment. The Platform reconciles from there; nothing talks to\n" +
			"Kubernetes.\n\n" +
			"Every question has a flag, so the command runs in scripts. Authentication is\n" +
			"the gh CLI's login (gh auth login); write access to the Platform repository\n" +
			"(and, with --path create or adopt, to the Application's repository) is the\n" +
			"authorisation.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runAppCreate(cmd, &opts, deps)
		},
	}
	f := cmd.Flags()
	f.StringVar(&opts.name, "name", "", "Application name: lowercase letters, digits and dashes, starting with a letter, at most 40 characters, unique on the Platform (defaults to the repository name with --path adopt)")
	f.StringVar(&opts.kind, "kind", "", "Kind of Application: web-service or static-site (derived from --framework with --path create, unless --framework other; required with --path adopt when the repository already has a Dockerfile or no known framework is detected)")
	f.StringVar(&opts.path, "path", "", "How the Application repository comes to be: create (generate one) or adopt (open a pull request on an existing one). Omit to write only the Platform repository, as before this flag existed")
	f.StringVar(&opts.repo, "repo", "", "Existing Application repository to adopt: owner/name or a URL (--path adopt only)")
	f.StringVar(&opts.framework, "framework", "", "Framework to generate the Application repository from (--path create only): nextjs, vite-react or other")
	f.StringVar(&opts.owner, "owner", "org", "Where to create the Application repository (--path create only): org (the compiled-in "+platform.Org+") or user (your personal GitHub account)")
	f.BoolVar(&opts.private, "private", true, "Create the Application repository as private (--path create only; default)")
	f.BoolVar(&opts.public, "public", false, "Create the Application repository as public instead of private (--path create only)")
	f.StringVar(&opts.size, "size", "small", "Size: small, medium or large")
	f.StringVar(&opts.image, "image", "", "Image repository (default ghcr.io/<owner lowercased>/<name>)")
	f.IntVar(&opts.port, "port", 3000, "Port the container listens on")
	f.StringVar(&opts.probePath, "probe-path", "/", "Path the readiness and liveness probes request")
	f.BoolVar(&opts.yes, "yes", false, "Skip the confirmation (nothing is asked yet; accepted so scripts keep working once the wizard asks)")
	f.BoolVar(&opts.postgres, "postgres", false, "Add a Postgres database Capability: DATABASE_URL injected into every Environment, continuous backups")
	f.StringVar(&opts.migrationCommand, "migration-command", "", "Shell command run before every rollout with DATABASE_URL set (requires --postgres); detected from Prisma, Drizzle or an npm migrate script when omitted")
	f.StringVar(&opts.appDir, "app-dir", "", "Directory to detect the migration command in (default: the generated template with --path create, or the current directory when it has a package.json)")
	_ = f.MarkHidden("app-dir")
	f.BoolVar(&opts.staging, "staging", false, "Add a staging Environment next to prod: its own address, its own database, the same Capabilities")
	f.StringArrayVar(&opts.domains, "domain", nil, "Custom domain to serve besides the Platform address, for prod only (repeatable)")
	f.BoolVar(&opts.login, "login", false, "Require Itema (Entra ID) sign-in on the Platform addresses, in every Environment; refused together with --domain")
	f.StringVar(&opts.platformRepo, "platform-repo", platform.RepositoryURL, "Git URL of the Platform repository")
	_ = f.MarkHidden("platform-repo")
	f.BoolVar(&opts.interactive, "interactive", false, "Run the wizard even without a terminal on stdin")
	_ = f.MarkHidden("interactive")
	return cmd
}

// isInteractive reports whether app create should run the wizard: forced by
// the hidden --interactive flag (how tests drive it without a real
// terminal), or stdin being a terminal. Without either, behaviour is
// unchanged: a missing required flag is an error.
//
// This checks golang.org/x/term.IsTerminal rather than only
// os.Stdin.Stat's os.ModeCharDevice bit: /dev/null is itself a character
// device, so that bit alone cannot tell a real terminal from stdin
// redirected from /dev/null, which every non-interactive script, cron job
// and CI runner does
// (docs/implementation-notes/14-cli-wizard.md).
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

	ownerLogin := platform.Org
	ownerIsOrg := true
	switch plan.path {
	case pathCreate:
		if plan.ownerMode == "user" {
			login, err := ghClient.CurrentUser(cmd.Context())
			if err != nil {
				return err
			}
			ownerLogin, ownerIsOrg = login, false
		}
	case pathAdopt:
		// The Application repository's own owner, not a choice: Adopt
		// reads an existing repository rather than creating one under the
		// org or the developer's account
		// (docs/implementation-notes/15-cli-adopt-path.md).
		ownerLogin = plan.repoOwner
		ownerIsOrg = strings.EqualFold(plan.repoOwner, platform.Org)
	}

	image := plan.imageOverride
	if image == "" {
		image = "ghcr.io/" + strings.ToLower(ownerLogin) + "/" + plan.name
	}
	// kind and migrationCommand are already final for Create and the
	// legacy bare path (plan.kind is resolved, and detectMigrationCommand
	// runs against a freshly rendered template or the current directory).
	// For Adopt, both depend on cloning the target repository, which the
	// Create/legacy paths never do: they are resolved below, either during
	// the interactive preview (to show real values in the summary) or,
	// definitively, once Adopter.Adopt itself has cloned the repository.
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
			if err := checkAdoptPreview(preview, plan.repoOwner, plan.repoName); err != nil {
				return err
			}
			summaryKind, err = apprepo.ResolveKind(plan.kind, preview.Detection)
			if err != nil {
				return fmt.Errorf("%s/%s: %w", plan.repoOwner, plan.repoName, err)
			}
			if plan.postgres && !plan.migrationCommandSet && preview.MigrationOK {
				summaryMigration = preview.Migration.Command
			}
			adoptFiles = preview.Detection.Files()
		}
		if err := checkPostgresKind(plan.postgres, summaryKind); err != nil {
			return err
		}
		app := platformrepo.Application{
			Name:             plan.name,
			Kind:             summaryKind,
			Size:             plan.size,
			ImageRepository:  image,
			Port:             plan.port,
			ProbePath:        plan.probePath,
			Postgres:         plan.postgres,
			MigrationCommand: summaryMigration,
			Staging:          plan.staging,
			Domains:          plan.domains,
			Login:            plan.login,
		}
		preview, err := platformWriter.PreviewApplication(cmd.Context(), app)
		if err != nil {
			return err
		}
		printSummary(out, plan, ownerLogin, app, preview, adoptFiles)
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
				fmt.Fprintf(out, "Nothing was written to %s. Running this command again with the same --name will refuse: %s already exists. Either push the rendered template to %s by hand and finish with the Platform-repository step yourself (see docs/platform-repository.md), or delete the repository on GitHub and run this command again.\n", platform.Repository, appRepo.URL, appRepo.CloneURL)
			}
			return err
		}
		fmt.Fprintf(out, "\nCreated and pushed the Application repository %s:\n", appRepo.URL)
		for _, f := range appRepo.Files {
			fmt.Fprintf(out, "  %s\n", f)
		}
		fmt.Fprintf(out, "\nThe deploy workflow (.github/workflows/deploy.yaml) is pinned to iidp %s.\n", appRepo.IidpVersion)
		if !ownerIsOrg {
			printPersonalOwnerNote(out, appRepo.URL)
		}
	}

	var adoptResult apprepo.AdoptResult
	if plan.path == pathAdopt {
		fmt.Fprintf(out, "Adopting %s/%s onto the Platform:\n", plan.repoOwner, plan.repoName)

		// Postgres-vs-Kind is validated inside Adopter.Adopt itself, right
		// after Kind is resolved and before anything is written, committed
		// or pushed: unlike the interactive summary's preview (which can
		// check this before the developer even confirms), a flag-driven
		// run does not know the final Kind until the repository has been
		// cloned, and a refusal must never come after the pull request
		// already exists (docs/implementation-notes/15-cli-adopt-path.md).
		adoptResult, err = adopter.Adopt(cmd.Context(), apprepo.AdoptRequest{
			Owner:    plan.repoOwner,
			Name:     plan.repoName,
			AppName:  plan.name,
			Kind:     kind,
			Postgres: plan.postgres,
		})
		if err != nil {
			return err
		}
		kind = adoptResult.Kind
		if !plan.migrationCommandSet {
			switch {
			case plan.postgres && adoptResult.MigrationOK:
				migrationCommand = adoptResult.Migration.Command
				fmt.Fprintf(out, "Detected %s in %s/%s; migration command: %s\n", adoptResult.Migration.Tool, plan.repoOwner, plan.repoName, migrationCommand)
			case plan.postgres:
				fmt.Fprintln(out, "No migration tooling detected; postgres.migrationCommand is left empty. Set --migration-command if the Application has migrations.")
			}
		}

		fmt.Fprintf(out, "\nOpened a pull request on %s, branch %s: %s\n", adoptResult.RepoURL, adoptResult.Branch, adoptResult.PullRequestURL)
		fmt.Fprintln(out, "It adds:")
		for _, f := range adoptResult.Files {
			fmt.Fprintf(out, "  %s\n", f)
		}
		fmt.Fprintln(out, "The first merged run of its deploy workflow deploys the Application.")
		if !ownerIsOrg {
			printPersonalOwnerNote(out, adoptResult.RepoURL)
		}
	}

	app := platformrepo.Application{
		Name:             plan.name,
		Kind:             kind,
		Size:             plan.size,
		ImageRepository:  image,
		Port:             plan.port,
		ProbePath:        plan.probePath,
		Postgres:         plan.postgres,
		MigrationCommand: migrationCommand,
		Staging:          plan.staging,
		Domains:          plan.domains,
		Login:            plan.login,
	}

	fmt.Fprintf(out, "\nWriting the prod Environment to %s...\n", platform.Repository)
	fmt.Fprintf(out, "  Kind:   %s\n", app.Kind)
	fmt.Fprintf(out, "  Size:   %s\n", app.Size)
	fmt.Fprintf(out, "  Image:  %s\n", app.ImageRepository)
	fmt.Fprintf(out, "  Port:   %d\n", app.Port)
	fmt.Fprintf(out, "  Probe:  %s\n", app.ProbePath)
	if app.Postgres {
		fmt.Fprintf(out, "  Postgres: enabled (migration command: %q)\n", app.MigrationCommand)
	}
	if app.Staging {
		fmt.Fprintln(out, "  Staging:  a second Environment, its own address and database")
	}
	if app.Login {
		fmt.Fprintln(out, "  Login:    Itema (Entra ID) sign-in required on the Platform addresses")
	}

	res, err := platformWriter.CreateApplication(cmd.Context(), app, out)
	if err != nil {
		switch plan.path {
		case pathCreate:
			fmt.Fprintf(out, "\nThe Application repository %s was created and pushed.\n", appRepo.URL)
			fmt.Fprintf(out, "Writing %s failed: %v\n", platform.Repository, err)
			fmt.Fprintf(out, "Finish by hand: clone %s, add applications/%s/prod/{application.yaml,values.yaml} (see docs/platform-repository.md), commit and push to main. The Application repository is untouched; nothing is deleted.\n", platform.RepositoryURL, plan.name)
		case pathAdopt:
			fmt.Fprintf(out, "\nThe pull request %s was opened.\n", adoptResult.PullRequestURL)
			fmt.Fprintf(out, "Writing %s failed: %v\n", platform.Repository, err)
			fmt.Fprintf(out, "Finish by hand: clone %s, add applications/%s/prod/{application.yaml,values.yaml} (see docs/platform-repository.md), commit and push to main. The pull request is untouched; nothing is deleted.\n", platform.RepositoryURL, plan.name)
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
	return nil
}

// checkAdoptPreview turns an AdoptPreview's two refusal conditions into the
// same errors Adopter.Adopt itself would return, so the interactive wizard
// refuses before the summary rather than after a developer confirms.
// checkPostgresKind refuses --postgres against a Static site Kind: a
// Static site has no server to run a database against. A no-op when kind
// is "" (Adopt, before it is resolved): the same check runs again once
// Kind is known, inside Adopter.Adopt and the interactive summary's
// preview.
func checkPostgresKind(postgres bool, kind string) error {
	if postgres && kind == platformrepo.KindStaticSite {
		return errors.New("--postgres needs --kind web-service (or a framework/detected framework that derives it); a Static site has no server to use a database")
	}
	return nil
}

func checkAdoptPreview(preview apprepo.AdoptPreview, owner, name string) error {
	if !preview.CanPush {
		return fmt.Errorf("%w: you do not have write access to %s/%s; Adopt needs it to open a pull request", apprepo.ErrNoPushAccess, owner, name)
	}
	if preview.BranchExists {
		return fmt.Errorf("%w: %s already exists on %s/%s; merge or delete it before adopting again", apprepo.ErrBranchExists, apprepo.AdoptBranch, owner, name)
	}
	return nil
}

// printPersonalOwnerNote is the by-hand secret-and-variable note for an
// Application repository under a personal account rather than the org
// (docs/implementation-notes/12-deploy-workflow.md): neither the deploy
// App's private key nor its id reaches a personal-account repository
// automatically.
func printPersonalOwnerNote(out io.Writer, repoURL string) {
	fmt.Fprintf(out, "IIDP_DEPLOY_APP_PRIVATE_KEY (a secret) and IIDP_DEPLOY_APP_ID (a variable) are org-level; a personal-account repository does not receive either automatically. Add both by hand: %s/settings/secrets/actions/new (secret IIDP_DEPLOY_APP_PRIVATE_KEY, the org GitHub App's private key PEM) and %s/settings/variables/actions/new (variable IIDP_DEPLOY_APP_ID, the org GitHub App's id).\n", repoURL, repoURL)
}

// checkRequiredFlags reports flags missing for the chosen --path, in the
// style of cobra's own required-flag message ("kind" is not a cobra
// required flag any more because whether it is required depends on
// --framework, which cobra cannot express).
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
		// --name is not required for Adopt: it defaults to the repository
		// name (createOptions.plan). --kind's requirement depends on
		// detection, which needs the repository cloned, so it cannot be
		// checked here; Adopter.Adopt refuses clearly once it knows.
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
	// repoOwner and repoName are --repo, parsed (--path adopt only).
	repoOwner        string
	repoName         string
	ownerMode        string
	private          bool
	postgres         bool
	migrationCommand string
	// migrationCommandSet is true when --migration-command was given
	// explicitly (including by the wizard, which sets the flag even on an
	// empty answer): detectMigrationCommand then uses migrationCommand
	// as-is instead of running detection again.
	migrationCommandSet bool
	appDir              string
	staging             bool
	domains             []string
	login               bool
}

// plan validates every flag before anything is cloned or written, and
// turns them into the plan for the chosen --path.
func (o createOptions) plan(cmd *cobra.Command) (createPlan, error) {
	switch o.path {
	case "", pathCreate, pathAdopt:
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
		if name == "" {
			name = repoName
		}
	}
	if err := platformrepo.ValidateName(name); err != nil {
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
		// Whether Kind is required at all depends on what Adopt finds when
		// it clones the repository (an existing Dockerfile, or no known
		// framework), which needs network access plan() never does; the
		// eventual refusal is apprepo.ResolveKind's job, run once the
		// repository has actually been read
		// (docs/implementation-notes/15-cli-adopt-path.md). An explicit
		// --kind is validated here and used unconditionally later.
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
	// For Adopt, kind can still be "" here (it must be derived from
	// detection, which needs the repository cloned); checkPostgresKind is a
	// no-op against an empty Kind, and this check runs again, with the
	// real Kind, inside Adopter.Adopt and the interactive summary's
	// preview, both before anything is written.
	if err := checkPostgresKind(o.postgres, kind); err != nil {
		return createPlan{}, err
	}
	if o.login && len(o.domains) > 0 {
		return createPlan{}, errors.New(platformrepo.LoginDomainConflictMessage)
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
		ownerMode:           ownerMode,
		private:             private,
		postgres:            o.postgres,
		migrationCommand:    o.migrationCommand,
		migrationCommandSet: cmd.Flags().Changed("migration-command"),
		appDir:              o.appDir,
		staging:             o.staging,
		domains:             o.domains,
		login:               o.login,
	}, nil
}

// parseRepoFlag splits --repo into an owner and a repository name: either
// the short form owner/name, or a GitHub URL such as
// https://github.com/owner/name, with or without a trailing .git or slash.
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

// resolveMigrationDir picks the directory detection should look in for
// plan, in the order docs/design.md's wizard describes: --app-dir, then the
// Create path's rendered template, then the current directory when it has a
// package.json. dir is "" when there is nowhere to look; description names
// the directory for the "Looking for a migration command in ..." message;
// cleanup removes any temporary directory this created and must always be
// called.
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

// detectMigrationCommand resolves plan's migration command: migrationCommand
// as-is when migrationCommandSet (an explicit --migration-command, including
// one the wizard set from the developer's answer), otherwise, with
// --postgres, a detection with resolveMigrationDir. It prints what it looked
// in and what it found (or didn't) to out, the way docs/design.md's wizard
// help text does.
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
		fmt.Fprintln(out, "No migration tooling detected; postgres.migrationCommand is left empty. Set --migration-command if the Application has migrations.")
		return "", nil
	}
	fmt.Fprintf(out, "Detected %s; migration command: %s\n", det.Tool, det.Command)
	return det.Command, nil
}

// suggestMigrationCommand runs the same detection detectMigrationCommand
// would, without printing anything: the wizard's migration question uses it
// to show docs/design.md's help text (including the detected suggestion)
// before asking, given the answers gathered so far (name, path, framework,
// app-dir).
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
	if res.Login {
		fmt.Fprintln(out, "  Login:    Itema (Entra ID) sign-in required; sign in once to reach every protected address")
	}
	if res.Config.ArgoCDURL != "" {
		fmt.Fprintf(out, "  ArgoCD:   %s\n", res.Config.ArgoCDURL)
	}
	if res.Config.GrafanaURL != "" {
		fmt.Fprintf(out, "  Grafana:  %s\n", res.Config.GrafanaURL)
	}
	printDomains(out, res)
	fmt.Fprintf(out, "\nThe Environment deploys once the deploy workflow writes the first image tag.\n")
}

// printDomains reports, for each custom domain, which branch the chart
// takes (the wildcard certificate or a per-host one from
// platform.httpIssuer) and whether DNS is automatic, then lists the CNAME
// records left to create by hand for the domains that are not.
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
