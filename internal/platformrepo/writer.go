package platformrepo

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"

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
	// (chart/application/values.yaml).
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

// ReservedNames are the labels under baseDomain the Platform serves itself:
// the Deploy gate at deploy.<baseDomain>, the Itema login's oauth2-proxy at
// auth.<baseDomain> and the database tunnel at db.<baseDomain>. An
// Application of one of these names would have the same address. One
// serving deploy.<baseDomain> could receive the OIDC tokens other
// Applications' workflows mint for the gate, and one serving
// db.<baseDomain> the GitHub tokens developers send the tunnel.
var ReservedNames = []string{DeployGateHostLabel, "auth", DatabaseTunnelHostLabel}

// ValidateNewName is ValidateName for an Application about to be created:
// it also refuses the ReservedNames.
func ValidateNewName(name string) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	for _, reserved := range ReservedNames {
		if name == reserved {
			return fmt.Errorf("%w: %q is reserved: %s.<baseDomain> is the Platform's own address", ErrInvalidName, name, name)
		}
	}
	return nil
}

// EnvironmentDir is the directory of one Environment of an Application,
// relative to the Platform repository root.
func EnvironmentDir(application, environment string) string {
	return path.Join(ApplicationsDir, application, environment)
}

// Application is what the CLI knows about a new Application.
type Application struct {
	Name            string
	Kind            string
	Size            string
	ImageRepository string
	Port            int
	ProbePath       string
	// Postgres is the Postgres Capability, written into every Environment.
	// The migration command starts empty: the Deploy gate sets it from the
	// Application repository's iidp.yaml with each deploy.
	Postgres bool
	// Staging, when true, adds a second Environment next to prod: its own
	// address, its own database, the same Capabilities.
	Staging bool
	// Domains are custom domains for the prod Environment only; staging
	// keeps its Platform address.
	Domains []string
	// Login is the Itema login Capability, written into every Environment.
	// Refused together with a domain outside Config.LoginCookieDomain.
	Login bool
	// LoginGroups are the sign-in groups, Entra ID group object ids,
	// written into every Environment: with any, only their members get past
	// Itema login. Needs Login.
	LoginGroups []string
	// Repository, when set, binds the Application to its Application
	// repository by id, written as applications/<name>/repository.yaml in
	// the same commit.
	Repository *RepositoryBinding
	// Previews gives the Application a Preview Environment for every open
	// pull request labelled preview on its Application repository: the
	// ApplicationSet PreviewsPath. It needs Staging, whose values and
	// secrets previews use, and Repository, whose pull requests they follow.
	Previews bool
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
	// Login is whether the Itema login Capability is enabled.
	Login bool
	// LoginGroups are the Application's sign-in groups once the run is
	// done; none means any Itema user gets in.
	LoginGroups []string
	// PreviewAddress is the address of a Preview Environment, with
	// <number> for the pull request's, or "" when the Application has no
	// Preview Environments.
	PreviewAddress string
	// Databases are the database access levels of each Environment the run
	// turned Postgres on in, prod first.
	Databases []EnvironmentDatabase
}

// EnvironmentDatabase is one Environment's database access as a run left
// it.
type EnvironmentDatabase struct {
	Environment string
	Access      render.DatabaseAccess
	// Closed is true when a level is not none but its password was not
	// written, because platform.yaml has no agePublicKey: the Environment
	// stays closed until iidp app db access writes it.
	Closed bool
}

// openDatabase writes environment's default database access, with the
// passwords it needs when platform.yaml has an agePublicKey, and returns
// what it wrote and the Environment's database access.
func (w *Writer) openDatabase(ctx context.Context, dir string, cfg Config, application, environment string) ([]string, EnvironmentDatabase, error) {
	access := render.DefaultDatabaseAccess(environment)
	passwords := cfg.AgePublicKey != ""
	files, err := w.writeDatabaseAccess(ctx, dir, cfg, application, environment, access, passwords)
	if err != nil {
		return nil, EnvironmentDatabase{}, err
	}
	open := access.ReadWrite != render.AccessNone || access.ReadOnly != render.AccessNone
	return files, EnvironmentDatabase{Environment: environment, Access: access, Closed: open && !passwords}, nil
}

// appendPaths appends each of more to files that files does not hold yet.
func appendPaths(files []string, more ...string) []string {
	for _, f := range more {
		if !slices.Contains(files, f) {
			files = append(files, f)
		}
	}
	return files
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

func (w *Writer) encryptor() sops.Encryptor {
	if w.Encryptor != nil {
		return w.Encryptor
	}
	return sops.Binary{}
}

// CreateApplication writes app's Environments to the Platform repository,
// commits and pushes. A push refused because main moved is retried once
// from a fresh clone. out gets a line if a leftover directory from
// DeleteApplication is cleared.
func (w *Writer) CreateApplication(ctx context.Context, app Application, out io.Writer) (Result, error) {
	return runWithRetry(ctx, func(ctx context.Context, retry bool) (Result, error) {
		return w.attemptCreate(ctx, app, retry, false, out)
	})
}

// PreviewApplication reports what CreateApplication would write for app,
// without writing, committing or pushing anything.
func (w *Writer) PreviewApplication(ctx context.Context, app Application) (Result, error) {
	return w.attemptCreate(ctx, app, false, true, io.Discard)
}

// runWithRetry runs attempt, from clone to push, and runs it once more if
// the push is rejected because main moved. retry is true on the second
// call, so it can tell a genuine conflict. It is a function because Go
// methods cannot be generic.
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

// CheckAvailable reports an error if name already has a live Environment in
// the Platform repository, without writing anything; CreateApplication
// checks again. It returns platform.yaml from the same clone.
func (w *Writer) CheckAvailable(ctx context.Context, name string) (Config, error) {
	dir, err := os.MkdirTemp("", "iidp-platform-check-")
	if err != nil {
		return Config{}, err
	}
	defer os.RemoveAll(dir)
	if _, err := git.Clone(ctx, w.URL, Branch, dir, w.Auth); err != nil {
		return Config{}, fmt.Errorf("cloning %s: %w", platform.Repository, err)
	}
	cfg, err := LoadConfig(dir)
	if err != nil {
		return Config{}, err
	}
	return cfg, checkApplicationAbsent(dir, name, false)
}

// ReadConfig returns platform.yaml from a fresh clone of the Platform
// repository.
func (w *Writer) ReadConfig(ctx context.Context) (Config, error) {
	dir, err := os.MkdirTemp("", "iidp-platform-config-")
	if err != nil {
		return Config{}, err
	}
	defer os.RemoveAll(dir)
	if _, err := git.Clone(ctx, w.URL, Branch, dir, w.Auth); err != nil {
		return Config{}, fmt.Errorf("cloning %s: %w", platform.Repository, err)
	}
	return LoadConfig(dir)
}

// checkApplicationAbsent errors if name has an application.yaml under prod/
// or staging/ in the clone at dir. A directory without one is the leftover
// of DeleteApplication, which attemptCreate clears, so the name can be
// reused without a hand edit. retry words the error for a rerun after main
// moved.
func checkApplicationAbsent(dir, name string, retry bool) error {
	live, err := applicationHasLiveEnvironment(dir, name)
	if err != nil {
		return fmt.Errorf("checking for an existing Application: %w", err)
	}
	switch {
	case live && retry:
		return fmt.Errorf("%w: %q was added to %s while this command ran; nothing was written", ErrApplicationExists, name, platform.Repository)
	case live:
		return fmt.Errorf("%w: %q already has a live Environment under %s/ in %s", ErrApplicationExists, name, ApplicationsDir, platform.Repository)
	default:
		return nil
	}
}

func applicationHasLiveEnvironment(dir, name string) (bool, error) {
	for _, e := range Environments {
		switch _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(EnvironmentDir(name, e)), "application.yaml")); {
		case err == nil:
			return true, nil
		case errors.Is(err, fs.ErrNotExist):
			continue
		default:
			return false, err
		}
	}
	return false, nil
}

// attemptCreate validates app against a fresh clone of the Platform
// repository and, unless preview is true, writes its Environment files,
// commits and pushes them.
func (w *Writer) attemptCreate(ctx context.Context, app Application, retry, preview bool, out io.Writer) (Result, error) {
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
	if app.Postgres {
		if err := checkBackupsCredentialsPresent(dir); err != nil {
			return Result{}, err
		}
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
	if app.Login {
		if err := CheckLoginDomains(cfg, app.Domains); err != nil {
			return Result{}, err
		}
	}
	if len(app.LoginGroups) > 0 && !app.Login {
		return Result{}, fmt.Errorf("%w: give --login with --login-group", ErrLoginGroupsWithoutLogin)
	}
	if app.Previews {
		if !app.Staging {
			return Result{}, ErrPreviewsWithoutStaging
		}
		if app.Repository == nil {
			return Result{}, fmt.Errorf("%w: give --path create or --path adopt", ErrPreviewsWithoutBinding)
		}
		if _, _, err := previewsRepository(app.Name, *app.Repository, true); err != nil {
			return Result{}, err
		}
	}

	environments := []string{"prod"}
	if app.Staging {
		environments = append(environments, "staging")
	}

	var files []string
	var databases []EnvironmentDatabase
	if preview {
		for _, environment := range environments {
			envDir := EnvironmentDir(app.Name, environment)
			files = append(files, path.Join(envDir, "application.yaml"), path.Join(envDir, "values.yaml"))
			if app.Postgres {
				sopsDir := path.Join(envDir, "sops")
				files = append(files, path.Join(sopsDir, "backups-credentials.enc.yaml"), path.Join(sopsDir, "kustomization.yaml"), path.Join(sopsDir, "ksops.yaml"))
				for _, role := range render.AccessRoles {
					if role.Level(render.DefaultDatabaseAccess(environment)) != render.AccessNone && cfg.AgePublicKey != "" {
						files = append(files, path.Join(sopsDir, role.PasswordSlug()+".enc.yaml"))
					}
				}
			}
		}
		if app.Repository != nil {
			files = append(files, RepositoryBindingPath(app.Name))
		}
		if app.Previews {
			files = append(files, PreviewsPath(app.Name))
		}
	} else {
		appDir := path.Join(ApplicationsDir, app.Name)
		switch _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(appDir))); {
		case err == nil:
			// Not a live Environment (checkApplicationAbsent), so the leftover
			// of DeleteApplication: clear it in the same commit.
			fmt.Fprintf(out, "Found a leftover %s/ from a previous iidp app delete; removing it before creating %s.\n", appDir, app.Name)
			if err := repo.Remove(ctx, appDir); err != nil {
				return Result{}, fmt.Errorf("removing the leftover %s: %w", appDir, err)
			}
		case !errors.Is(err, fs.ErrNotExist):
			return Result{}, fmt.Errorf("checking for a leftover %s: %w", appDir, err)
		}
		for _, environment := range environments {
			envFiles, err := w.writeEnvironment(dir, cfg, app, environment)
			if err != nil {
				return Result{}, err
			}
			files = append(files, envFiles...)
			if app.Postgres {
				credFiles, err := copyBackupsCredentials(dir, app.Name, environment)
				if err != nil {
					return Result{}, err
				}
				files = append(files, credFiles...)
				dbFiles, db, err := w.openDatabase(ctx, dir, cfg, app.Name, environment)
				if err != nil {
					return Result{}, err
				}
				files = appendPaths(files, dbFiles...)
				databases = append(databases, db)
			}
		}
		if app.Repository != nil {
			bindingFile, err := writeRepositoryBinding(dir, app.Name, *app.Repository)
			if err != nil {
				return Result{}, err
			}
			files = append(files, bindingFile)
		}
		if app.Previews {
			// Last: the ApplicationSet is rendered from staging's
			// application.yaml as it is on disk, which must already carry
			// any sops/ source copyBackupsCredentials adds.
			previewFiles, err := writePreviews(dir, app.Name, cfg, *app.Repository, true)
			if err != nil {
				return Result{}, err
			}
			files = append(files, previewFiles...)
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
		Config:    cfg,
		Files:     files,
		Address:   "https://" + prodAddress,
		Domains:   domainPlans,
		Login:     app.Login,
		Databases: databases,
	}
	if app.Login {
		res.LoginGroups = app.LoginGroups
	}
	if app.Staging {
		res.StagingAddress = "https://" + stagingAddress
	}
	if app.Previews {
		res.PreviewAddress = PreviewAddress(app.Name, cfg.BaseDomain)
	}
	return res, nil
}

// writeEnvironment writes the files of one Environment into the clone at
// dir and returns their repository-relative paths.
func (w *Writer) writeEnvironment(dir string, cfg Config, app Application, environment string) ([]string, error) {
	chartRepoURL, chartName, err := cfg.Chart()
	if err != nil {
		return nil, err
	}
	env := render.Environment{
		Application:     app.Name,
		Environment:     environment,
		BaseDomain:      cfg.BaseDomain,
		Kind:            app.Kind,
		ImageRepository: app.ImageRepository,
		Size:            app.Size,
		Port:            app.Port,
		ProbePath:       app.ProbePath,
		PostgresEnabled: app.Postgres,
		Login:           app.Login,
	}
	if app.Postgres {
		env.BackupsBucket = cfg.BackupsBucket
		env.ObjectStorageEndpoint = cfg.ObjectStorageEndpoint
	}
	if app.Login {
		env.LoginCookieDomain = cfg.LoginCookieDomain()
		env.LoginGroups = app.LoginGroups
	}
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
