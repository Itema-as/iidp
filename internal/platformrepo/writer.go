package platformrepo

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"

	"github.com/Itema-as/iidp/internal/git"
	"github.com/Itema-as/iidp/internal/platform"
	"github.com/Itema-as/iidp/internal/render"
	"github.com/Itema-as/iidp/internal/sops"
)

const (
	// ApplicationsDir is the directory in the Platform repository holding
	// one directory per Application.
	ApplicationsDir = "applications"

	// Branch is the branch the CLI writes to. There is no review step; write
	// access to the Platform repository is the authorisation.
	Branch = "main"

	// MaxNameLength leaves room, inside the 63 characters a DNS label and a
	// Service name allow, for the -staging suffix and for the suffixes the
	// chart adds to an Environment's objects.
	MaxNameLength = 40

	// KindWebService and KindStaticSite are the two Kinds the chart accepts
	// (chart/application/values.yaml). This is the one place they are
	// spelled in Go; internal/templates derives a Framework's Kind from
	// these same constants.
	KindWebService = "web-service"
	KindStaticSite = "static-site"
)

// ValidKind reports whether kind is one the chart renders.
func ValidKind(kind string) bool {
	return kind == KindWebService || kind == KindStaticSite
}

// ErrApplicationExists is wrapped by CreateApplication when the Application
// already has a directory in the Platform repository.
var ErrApplicationExists = errors.New("Application already exists")

// ErrInvalidName is wrapped by ValidateName.
var ErrInvalidName = errors.New("invalid Application name")

var dns1035Label = regexp.MustCompile(`^[a-z]([-a-z0-9]*[a-z0-9])?$`)

// ValidateName checks that name can be an Application name: a lowercase
// DNS-1035 label (letters, digits and dashes, starting with a letter, not
// ending with a dash) of at most MaxNameLength characters.
func ValidateName(name string) error {
	switch {
	case name == "":
		return fmt.Errorf("%w: it is empty", ErrInvalidName)
	case !dns1035Label.MatchString(name):
		return fmt.Errorf("%w: %q must be lowercase letters, digits and dashes, start with a letter and not end with a dash", ErrInvalidName, name)
	case len(name) > MaxNameLength:
		// The label check above leaves only ASCII, so bytes are characters.
		return fmt.Errorf("%w: %q is %d characters, the most is %d", ErrInvalidName, name, len(name), MaxNameLength)
	}
	return nil
}

// EnvironmentDir is the directory of one Environment of an Application,
// relative to the Platform repository root.
func EnvironmentDir(application, environment string) string {
	return path.Join(ApplicationsDir, application, environment)
}

// Application is what the CLI knows about a new Application: the answers
// to the wizard's questions this ticket supports.
type Application struct {
	Name            string
	Kind            string
	Size            string
	ImageRepository string
	Port            int
	ProbePath       string
	// Postgres and MigrationCommand are the Postgres Capability, written
	// into every Environment. MigrationCommand is only meaningful with
	// Postgres.
	Postgres         bool
	MigrationCommand string
	// Staging, when true, adds a second Environment next to prod: its own
	// address, its own database, the same Capabilities.
	Staging bool
	// Domains are custom domains for the prod Environment only; staging
	// keeps its Platform address (docs/implementation-notes/13-cli-capabilities.md).
	Domains []string
}

// Result is what CreateApplication wrote and where it can be seen.
type Result struct {
	Config Config
	// Files are the paths written, relative to the Platform repository root.
	Files []string
	// Address is the prod Environment's URL.
	Address string
	// StagingAddress is the staging Environment's URL, or "" without one.
	StagingAddress string
	// Domains reports how each requested custom domain will be served, in
	// the order given.
	Domains []DomainPlan
	// Environment is set by SetImageTag to the Environment actually
	// written: the literal prod or staging it was given, or, given auto,
	// whichever of the two it resolved to.
	Environment string
}

// Writer commits Applications to the Platform repository.
type Writer struct {
	// URL is the git URL of the Platform repository.
	URL string
	// Auth is the credential for URL.
	Auth git.Auth
	// BeforePush, when set, runs after the commit and before each push
	// attempt. Tests use it to move main in the meantime.
	BeforePush func() error
	// Encryptor encrypts secret values for SetSecrets; nil means
	// sops.Binary{}, the sops binary on PATH.
	Encryptor sops.Encryptor
}

// encryptor returns w.Encryptor, defaulting to the sops binary on PATH.
func (w *Writer) encryptor() sops.Encryptor {
	if w.Encryptor != nil {
		return w.Encryptor
	}
	return sops.Binary{}
}

// CreateApplication adds the prod Environment of app to the Platform
// repository: it clones main, validates that the Application does not
// exist, writes the Environment's files, commits and pushes. A push refused
// because main moved is retried once from a fresh clone; if the Application
// appeared in the meantime, the retry fails with ErrApplicationExists.
func (w *Writer) CreateApplication(ctx context.Context, app Application) (Result, error) {
	return runWithRetry(ctx, func(ctx context.Context, retry bool) (Result, error) {
		return w.attemptCreate(ctx, app, retry, false)
	})
}

// PreviewApplication reports what CreateApplication would write for app —
// the Environment addresses, the domain plans, and the paths that would be
// written — without committing or pushing anything. The wizard's summary
// screen (docs/implementation-notes/14-cli-wizard.md) uses it so that
// declining the confirmation leaves no trace: platform.yaml is read from a
// clone that is removed before this method returns, and nothing is ever
// added, committed or pushed.
func (w *Writer) PreviewApplication(ctx context.Context, app Application) (Result, error) {
	return w.attemptCreate(ctx, app, false, true)
}

// runWithRetry runs attempt once, retrying it once after a fresh clone if
// the push is rejected because main moved in the meantime. attempt does
// everything from clone to push for one try; retry is true on the second
// call, so it can tell a genuine conflict from a first attempt. It is a
// free function, not a method, so every Writer operation (CreateApplication,
// SetSecrets, AddCapabilities, DeleteApplication) can share it whatever
// result type it produces: Go methods cannot themselves be generic.
func runWithRetry[T any](ctx context.Context, attempt func(ctx context.Context, retry bool) (T, error)) (T, error) {
	res, err := attempt(ctx, false)
	if errors.Is(err, git.ErrPushRejected) {
		res, err = attempt(ctx, true)
	}
	if errors.Is(err, git.ErrPushRejected) {
		var zero T
		return zero, fmt.Errorf("main of %s moved twice while this command ran; nothing was written, run it again", platform.Repository)
	}
	return res, err
}

// CheckAvailable reports an error if name already has a directory in the
// Platform repository, without writing anything. Create uses it to
// validate before the Application repository is created; CreateApplication
// checks again on its own clone, so a race is still caught.
func (w *Writer) CheckAvailable(ctx context.Context, name string) error {
	dir, err := os.MkdirTemp("", "iidp-platform-check-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	if _, err := git.Clone(ctx, w.URL, Branch, dir, w.Auth); err != nil {
		return fmt.Errorf("cloning %s: %w", platform.Repository, err)
	}
	return checkApplicationAbsent(dir, name, false)
}

// LoadRemoteConfig clones url anonymously (no credential at all) and reads
// platform.yaml. iidp ci set-image uses it to discover githubApp.id and
// githubApp.installationId before it has any credential to authenticate
// with: platform.yaml carries no secret (agePublicKey is a public key), so
// reading it needs none (docs/implementation-notes/12-deploy-workflow.md).
// This requires the Platform repository to allow anonymous read access
// (public, or a public deploy key/mirror): a private repository refuses
// the anonymous clone, and the error below says so.
func LoadRemoteConfig(ctx context.Context, url string) (Config, error) {
	dir, err := os.MkdirTemp("", "iidp-platform-config-")
	if err != nil {
		return Config{}, err
	}
	defer os.RemoveAll(dir)
	if _, err := git.Clone(ctx, url, Branch, dir, git.Auth{}); err != nil {
		return Config{}, fmt.Errorf("cloning %s anonymously to read %s: %w\niidp ci set-image reads githubApp.id and githubApp.installationId from %s before it has any credential to authenticate with, which requires %s to allow anonymous read access (see docs/implementation-notes/12-deploy-workflow.md); if it is private, this is expected to fail", platform.Repository, ConfigFile, err, ConfigFile, platform.Repository)
	}
	return LoadConfig(dir)
}

// checkApplicationAbsent errors if name already has a directory under
// ApplicationsDir in the clone at dir. retry names the error for the case
// where the check runs again after a push was rejected because main moved.
func checkApplicationAbsent(dir, name string, retry bool) error {
	switch _, err := os.Stat(filepath.Join(dir, ApplicationsDir, name)); {
	case err == nil && retry:
		return fmt.Errorf("%w: %q was added to %s while this command ran; nothing was written", ErrApplicationExists, name, platform.Repository)
	case err == nil:
		return fmt.Errorf("%w: %q already has a directory under %s/ in %s", ErrApplicationExists, name, ApplicationsDir, platform.Repository)
	case !errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("checking for an existing Application: %w", err)
	default:
		return nil
	}
}

// attemptCreate validates app against a fresh clone of the Platform
// repository and, unless preview is true, writes its Environment files,
// commits and pushes them. preview stops right after validation, before
// anything is written to the clone or the working tree, so PreviewApplication
// costs one clone and nothing else.
func (w *Writer) attemptCreate(ctx context.Context, app Application, retry, preview bool) (Result, error) {
	dir, err := os.MkdirTemp("", "iidp-platform-")
	if err != nil {
		return Result{}, err
	}
	defer os.RemoveAll(dir)

	repo, err := git.Clone(ctx, w.URL, Branch, dir, w.Auth)
	if err != nil {
		return Result{}, fmt.Errorf("cloning %s: %w", platform.Repository, err)
	}
	cfg, err := LoadConfig(dir)
	if err != nil {
		return Result{}, err
	}
	if err := checkApplicationAbsent(dir, app.Name, retry); err != nil {
		return Result{}, err
	}
	if app.Postgres && (cfg.BackupsBucket == "" || cfg.ObjectStorageEndpoint == "") {
		return Result{}, fmt.Errorf("%s in %s sets no backupsBucket or objectStorageEndpoint, needed for the Postgres Capability", ConfigFile, platform.Repository)
	}

	prodAddress := app.Name + "." + cfg.BaseDomain
	stagingAddress := app.Name + "-staging." + cfg.BaseDomain
	platformAddresses := []string{prodAddress}
	if app.Staging {
		platformAddresses = append(platformAddresses, stagingAddress)
	}
	domainPlans, err := ValidateDomains(app.Domains, cfg.BaseDomain, cfg.CloudflareZone, platformAddresses)
	if err != nil {
		return Result{}, err
	}

	environments := []string{"prod"}
	if app.Staging {
		environments = append(environments, "staging")
	}

	var files []string
	if preview {
		for _, environment := range environments {
			envDir := EnvironmentDir(app.Name, environment)
			files = append(files, path.Join(envDir, "application.yaml"), path.Join(envDir, "values.yaml"))
		}
	} else {
		for _, environment := range environments {
			envFiles, err := w.writeEnvironment(dir, cfg, app, environment)
			if err != nil {
				return Result{}, err
			}
			files = append(files, envFiles...)
		}
		if err := repo.Add(ctx, files...); err != nil {
			return Result{}, err
		}
		if err := repo.Commit(ctx, "iidp app create "+app.Name); err != nil {
			return Result{}, err
		}
		if w.BeforePush != nil {
			if err := w.BeforePush(); err != nil {
				return Result{}, err
			}
		}
		if err := repo.Push(ctx, Branch); err != nil {
			if errors.Is(err, git.ErrPushRejected) {
				return Result{}, err
			}
			return Result{}, fmt.Errorf("pushing to %s: %w\nIf this is a permission error, ask the Platform admin for write access to %s", platform.Repository, err, platform.Repository)
		}
	}

	res := Result{
		Config:  cfg,
		Files:   files,
		Address: "https://" + prodAddress,
		Domains: domainPlans,
	}
	if app.Staging {
		res.StagingAddress = "https://" + stagingAddress
	}
	return res, nil
}

// writeEnvironment renders and writes the files of one Environment into
// the clone at dir and returns their repository-relative paths.
func (w *Writer) writeEnvironment(dir string, cfg Config, app Application, environment string) ([]string, error) {
	chartRepoURL, chartName, err := cfg.Chart()
	if err != nil {
		return nil, err
	}
	env := render.Environment{
		Application:      app.Name,
		Environment:      environment,
		BaseDomain:       cfg.BaseDomain,
		Kind:             app.Kind,
		ImageRepository:  app.ImageRepository,
		Size:             app.Size,
		Port:             app.Port,
		ProbePath:        app.ProbePath,
		PostgresEnabled:  app.Postgres,
		MigrationCommand: app.MigrationCommand,
	}
	if app.Postgres {
		env.BackupsBucket = cfg.BackupsBucket
		env.ObjectStorageEndpoint = cfg.ObjectStorageEndpoint
	}
	// Custom domains apply to prod only; staging keeps its Platform address
	// (docs/implementation-notes/13-cli-capabilities.md).
	if environment == "prod" {
		env.Domains = app.Domains
	}
	envDir := EnvironmentDir(app.Name, environment)
	applicationPath := path.Join(envDir, "application.yaml")
	valuesPath := path.Join(envDir, "values.yaml")

	application, err := render.ArgoCDApplication(env, render.Chart{RepoURL: chartRepoURL, Name: chartName, Version: cfg.ChartVersion}, platform.RepositoryURL, valuesPath)
	if err != nil {
		return nil, err
	}
	values, err := render.Values(env)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(dir, filepath.FromSlash(envDir)), 0o755); err != nil {
		return nil, err
	}
	files := []struct {
		path    string
		content []byte
	}{{applicationPath, application}, {valuesPath, values}}
	paths := make([]string, 0, len(files))
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(f.path)), f.content, 0o644); err != nil {
			return nil, err
		}
		paths = append(paths, f.path)
	}
	return paths, nil
}
