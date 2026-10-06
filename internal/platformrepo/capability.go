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
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/Itema-as/iidp/internal/git"
	"github.com/Itema-as/iidp/internal/platform"
	"github.com/Itema-as/iidp/internal/render"
)

// ErrApplicationMissing is wrapped when the named Application has no
// directory in the Platform repository yet.
var ErrApplicationMissing = errors.New("Application does not exist")

// ErrCapabilityExists is wrapped by AddCapabilities when a requested
// Capability is already present, naming it.
var ErrCapabilityExists = errors.New("Capability already present")

// Capabilities is what iidp app add-capability asks AddCapabilities to add
// to an existing Application. AddCapabilities refuses each one that is
// already present.
type Capabilities struct {
	// Postgres enables the Postgres Capability in every Environment the
	// Application already has. It never sets a migration command: that
	// comes from the Application repository's iidp.yaml, through the Deploy
	// gate, with the image it belongs to.
	Postgres bool
	// Staging adds a second Environment next to prod, copying prod's
	// values.
	Staging bool
	// Domains are custom domains to add to prod; custom domains apply to
	// prod only.
	Domains []string
	// Size, when not "", is the new size for every Environment.
	Size string
	// Login is the Itema login Capability, added to every Environment the
	// Application already has.
	Login bool
	// SetLoginGroups replaces the sign-in groups of every Environment with
	// LoginGroups; an empty LoginGroups removes them, letting any Itema user
	// in again. Needs Itema login, already on or turned on by Login.
	SetLoginGroups bool
	LoginGroups    []string
	// Previews adds the Preview Environments' ApplicationSet (PreviewsPath).
	// It needs a staging Environment, already there or added by Staging, and
	// a repository binding.
	Previews bool
}

// environmentState is the handful of values AddCapabilities reads back out
// of an existing Environment's values.yaml to decide whether a Capability
// is already present.
type environmentState struct {
	Postgres struct {
		Enabled bool `yaml:"enabled"`
	} `yaml:"postgres"`
	Domains []string `yaml:"domains"`
	Size    string   `yaml:"size"`
	Login   struct {
		Enabled bool     `yaml:"enabled"`
		Groups  []string `yaml:"groups"`
	} `yaml:"login"`
}

// AddCapabilities adds caps to application's existing Environments, in one
// commit. values.yaml is edited in place, so unrelated keys, secrets and
// comments survive. A push refused because main moved is retried once
// from a fresh clone.
func (w *Writer) AddCapabilities(ctx context.Context, application string, caps Capabilities, out io.Writer) (Result, error) {
	return runWithRetry(ctx, func(ctx context.Context, retry bool) (Result, error) {
		return w.attemptAddCapabilities(ctx, application, caps, out, retry)
	})
}

func (w *Writer) attemptAddCapabilities(ctx context.Context, application string, caps Capabilities, out io.Writer, _ bool) (Result, error) {
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

	appDir := filepath.Join(dir, filepath.FromSlash(ApplicationsDir), application)
	if _, err := os.Stat(appDir); errors.Is(err, fs.ErrNotExist) {
		return Result{}, fmt.Errorf("%w: %q has no directory under %s/ in %s; create it first with iidp app create", ErrApplicationMissing, application, ApplicationsDir, platform.Repository)
	} else if err != nil {
		return Result{}, fmt.Errorf("checking for the Application: %w", err)
	}

	stagingExists := dirExists(filepath.Join(appDir, "staging"))
	prodValuesPath := filepath.Join(appDir, "prod", "values.yaml")
	prodValuesData, err := os.ReadFile(prodValuesPath)
	if err != nil {
		return Result{}, fmt.Errorf("reading %s: %w", prodValuesPath, err)
	}
	var prod environmentState
	if err := yaml.Unmarshal(prodValuesData, &prod); err != nil {
		return Result{}, fmt.Errorf("parsing %s: %w", prodValuesPath, err)
	}

	if err := checkCapabilitiesAbsent(application, caps, prod, stagingExists); err != nil {
		return Result{}, err
	}
	previewsExist, err := previewsPresent(dir, application)
	if err != nil {
		return Result{}, err
	}
	// A binding that is not YAML binds nothing, the way every other reader
	// takes it.
	binding, bound, bindingErr := ReadRepositoryBinding(dir, application)
	bound = bound && bindingErr == nil
	if caps.Previews {
		switch {
		case previewsExist:
			return Result{}, fmt.Errorf("%w: %q already has Preview Environments", ErrCapabilityExists, application)
		case !stagingExists && !caps.Staging:
			return Result{}, fmt.Errorf("%w: %q has no staging Environment", ErrPreviewsWithoutStaging, application)
		}
		if _, _, err := previewsRepository(application, binding, bound); err != nil {
			return Result{}, err
		}
	}
	if caps.Postgres && (cfg.BackupsBucket == "" || cfg.ObjectStorageEndpoint == "") {
		return Result{}, fmt.Errorf("%s in %s sets no backupsBucket or objectStorageEndpoint, needed for the Postgres Capability", ConfigFile, platform.Repository)
	}
	// postgresEnabledAfter is Postgres's state once this run's edits land.
	// Postgres is the same in every Environment of an Application, so
	// prod's answer holds for a new staging too.
	postgresEnabledAfter := caps.Postgres || prod.Postgres.Enabled
	if caps.Postgres || (caps.Staging && prod.Postgres.Enabled) {
		if err := checkBackupsCredentialsPresent(dir); err != nil {
			return Result{}, err
		}
	}
	platformAddresses := []string{application + "." + cfg.BaseDomain}
	if stagingExists || caps.Staging {
		platformAddresses = append(platformAddresses, application+"-staging."+cfg.BaseDomain)
	}
	domainPlans, err := ValidateDomains(caps.Domains, cfg.BaseDomain, cfg.CloudflareZone, platformAddresses)
	if err != nil {
		return Result{}, err
	}
	// The custom domains outside the login cookie domain that sign in on
	// their own host from now on: all of prod's when login is turned on,
	// otherwise only the ones added to an Application that has it.
	var loginCallbackHosts []string
	switch {
	case caps.Login:
		loginCallbackHosts = HostsOutsideLoginCookieDomain(append(slices.Clone(prod.Domains), caps.Domains...), cfg.LoginCookieDomain())
	case prod.Login.Enabled:
		loginCallbackHosts = HostsOutsideLoginCookieDomain(caps.Domains, cfg.LoginCookieDomain())
	}

	envs := []string{"prod"}
	if stagingExists {
		envs = append(envs, "staging")
	}
	var files []string
	var databases []EnvironmentDatabase
	var prodValuesAfterEdits []byte
	for _, env := range envs {
		envFiles, newValues, err := applyCapabilitiesToEnvironment(dir, application, env, caps, cfg, prod.Login.Enabled)
		if err != nil {
			return Result{}, err
		}
		files = append(files, envFiles...)
		if caps.Postgres {
			// Only when Postgres is turned on now: an Environment that
			// already had it has the file, and access of its own.
			credFiles, err := copyBackupsCredentials(dir, application, env)
			if err != nil {
				return Result{}, err
			}
			files = append(files, credFiles...)
			dbFiles, db, err := w.openDatabase(ctx, dir, cfg, application, env)
			if err != nil {
				return Result{}, err
			}
			files = appendPaths(files, dbFiles...)
			databases = append(databases, db)
		}
		if env == "prod" {
			prodValuesAfterEdits = newValues
		}
	}

	if caps.Staging {
		fmt.Fprintf(out, "Adding a staging Environment for %s, copying prod's values (environment: staging)...\n", application)
		if prodHasSecrets(prodValuesAfterEdits) {
			fmt.Fprintln(out, "prod has secrets set; they are not copied to the new staging Environment. Run iidp secret set for staging too once it exists.")
		}
		stagingFiles, err := writeStagingFromProd(dir, application, cfg, prodValuesAfterEdits)
		if err != nil {
			return Result{}, err
		}
		files = append(files, stagingFiles...)
		if postgresEnabledAfter {
			// A new staging Environment never had the file copied, nor
			// access of its own.
			credFiles, err := copyBackupsCredentials(dir, application, "staging")
			if err != nil {
				return Result{}, err
			}
			files = append(files, credFiles...)
			dbFiles, db, err := w.openDatabase(ctx, dir, cfg, application, "staging")
			if err != nil {
				return Result{}, err
			}
			files = appendPaths(files, dbFiles...)
			databases = append(databases, db)
		}
	}

	// Last, once staging's application.yaml is final: the ApplicationSet
	// copies its sources.
	var previewFiles []string
	if caps.Previews {
		previewFiles, err = writePreviews(dir, application, cfg, binding, bound)
	} else {
		previewFiles, err = refreshPreviews(dir, application, cfg, files)
	}
	if err != nil {
		return Result{}, err
	}
	files = appendPaths(files, previewFiles...)

	if err := repo.Add(ctx, files...); err != nil {
		return Result{}, err
	}
	if err := repo.Commit(ctx, "iidp app add-capability "+application+" "+strings.Join(capabilityNames(caps), " ")); err != nil {
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

	res := Result{
		Config:             cfg,
		Files:              files,
		Address:            "https://" + application + "." + cfg.BaseDomain,
		Domains:            domainPlans,
		Login:              caps.Login || prod.Login.Enabled,
		LoginCallbackHosts: loginCallbackHosts,
		Databases:          databases,
	}
	switch {
	case caps.SetLoginGroups:
		res.LoginGroups = caps.LoginGroups
	case prod.Login.Enabled:
		res.LoginGroups = prod.Login.Groups
	}
	if stagingExists || caps.Staging {
		res.StagingAddress = "https://" + application + "-staging." + cfg.BaseDomain
	}
	if previewsExist || caps.Previews {
		res.PreviewAddress = PreviewAddress(application, cfg.BaseDomain)
	}
	return res, nil
}

// checkCapabilitiesAbsent refuses each Capability in caps that prod (or, for
// staging, the filesystem) already carries, naming it.
func checkCapabilitiesAbsent(application string, caps Capabilities, prod environmentState, stagingExists bool) error {
	if caps.Postgres && prod.Postgres.Enabled {
		return fmt.Errorf("%w: Postgres is already enabled for %q", ErrCapabilityExists, application)
	}
	if caps.Staging && stagingExists {
		return fmt.Errorf("%w: %q already has a staging Environment", ErrCapabilityExists, application)
	}
	if caps.Size != "" && caps.Size == prod.Size {
		return fmt.Errorf("%w: %q is already size %q", ErrCapabilityExists, application, caps.Size)
	}
	if caps.Login && prod.Login.Enabled {
		if caps.SetLoginGroups {
			return fmt.Errorf("%w: Itema login is already enabled for %q; to change its sign-in groups, give --login-group without --login", ErrCapabilityExists, application)
		}
		return fmt.Errorf("%w: Itema login is already enabled for %q", ErrCapabilityExists, application)
	}
	if caps.SetLoginGroups && !caps.Login && !prod.Login.Enabled {
		return fmt.Errorf("%w: %q does not have Itema login; give --login with --login-group to turn it on for these groups only", ErrLoginGroupsWithoutLogin, application)
	}
	if caps.SetLoginGroups && prod.Login.Enabled {
		current := make([]string, len(prod.Login.Groups))
		for i, g := range prod.Login.Groups {
			current[i] = strings.ToLower(g)
		}
		if slices.Equal(current, caps.LoginGroups) {
			if len(current) == 0 {
				return fmt.Errorf("%w: %q has no sign-in groups; every Itema user already gets in", ErrCapabilityExists, application)
			}
			return fmt.Errorf("%w: the sign-in groups of %q are already %s", ErrCapabilityExists, application, strings.Join(current, ", "))
		}
	}
	for _, host := range caps.Domains {
		for _, existing := range prod.Domains {
			if existing == host {
				return fmt.Errorf("%w: domain %q is already listed for %q", ErrCapabilityExists, host, application)
			}
		}
	}
	return nil
}

// applyCapabilitiesToEnvironment edits one existing Environment's
// values.yaml for Postgres, size, login and, for prod only, domains,
// returning the paths it wrote and the edited values.yaml. loginOn is
// whether the Application already has Itema login: custom domains added
// to its prod then also write platform.loginCookieDomain, which older
// values.yaml files lack. Without it the chart would judge the domains
// against baseDomain and send one inside the zone to the host-only login.
func applyCapabilitiesToEnvironment(dir, application, environment string, caps Capabilities, cfg Config, loginOn bool) (files []string, newValues []byte, err error) {
	valuesRelPath := path.Join(EnvironmentDir(application, environment), "values.yaml")
	valuesAbsPath := filepath.Join(dir, filepath.FromSlash(valuesRelPath))
	data, err := os.ReadFile(valuesAbsPath)
	if err != nil {
		return nil, nil, fmt.Errorf("reading %s: %w", valuesRelPath, err)
	}

	changed := false
	if caps.Postgres {
		data, err = render.EnablePostgres(data, cfg.BackupsBucket, cfg.ObjectStorageEndpoint)
		if err != nil {
			return nil, nil, err
		}
		changed = true
	}
	if caps.Login || (loginOn && environment == "prod" && len(caps.Domains) > 0) {
		data, err = render.EnableLogin(data, cfg.LoginCookieDomain())
		if err != nil {
			return nil, nil, err
		}
		changed = true
	}
	if caps.SetLoginGroups {
		var groupsChanged bool
		data, groupsChanged, err = render.SetLoginGroups(data, caps.LoginGroups)
		if err != nil {
			return nil, nil, err
		}
		changed = changed || groupsChanged
	}
	if caps.Size != "" {
		var sizeChanged bool
		data, sizeChanged, err = render.SetSize(data, caps.Size)
		if err != nil {
			return nil, nil, err
		}
		changed = changed || sizeChanged
	}
	if environment == "prod" {
		for _, host := range caps.Domains {
			var domainChanged bool
			data, domainChanged, err = render.AddDomain(data, host)
			if err != nil {
				return nil, nil, err
			}
			changed = changed || domainChanged
		}
	}

	if changed {
		if err := os.WriteFile(valuesAbsPath, data, 0o644); err != nil {
			return nil, nil, err
		}
		files = append(files, valuesRelPath)
	}
	return files, data, nil
}

// writeStagingFromProd writes the staging Environment's application.yaml
// and values.yaml, the latter derived from prod's own (already edited)
// values.yaml by render.CopyValuesForStaging.
func writeStagingFromProd(dir, application string, cfg Config, prodValues []byte) ([]string, error) {
	chartRepoURL, chartName, err := cfg.Chart()
	if err != nil {
		return nil, err
	}
	envDir := EnvironmentDir(application, "staging")
	applicationRelPath := path.Join(envDir, "application.yaml")
	valuesRelPath := path.Join(envDir, "values.yaml")

	env := render.Environment{Application: application, Environment: "staging"}
	applicationYAML, err := render.ArgoCDApplication(env, render.Chart{RepoURL: chartRepoURL, Name: chartName, Version: cfg.ChartVersion}, platform.RepositoryURL, valuesRelPath)
	if err != nil {
		return nil, err
	}
	stagingValues, _, err := render.CopyValuesForStaging(prodValues)
	if err != nil {
		return nil, err
	}

	if err := os.MkdirAll(filepath.Join(dir, filepath.FromSlash(envDir)), 0o755); err != nil {
		return nil, err
	}
	files := []struct {
		path    string
		content []byte
	}{{applicationRelPath, applicationYAML}, {valuesRelPath, stagingValues}}
	paths := make([]string, 0, len(files))
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(f.path)), f.content, 0o644); err != nil {
			return nil, err
		}
		paths = append(paths, f.path)
	}
	return paths, nil
}

// capabilityNames lists the Capabilities caps carries, for the commit
// message.
func capabilityNames(caps Capabilities) []string {
	var names []string
	if caps.Postgres {
		names = append(names, "postgres")
	}
	if caps.Staging {
		names = append(names, "staging")
	}
	if len(caps.Domains) > 0 {
		names = append(names, "domain")
	}
	if caps.Size != "" {
		names = append(names, "size")
	}
	if caps.Login {
		names = append(names, "login")
	}
	if caps.SetLoginGroups {
		names = append(names, "login-group")
	}
	if caps.Previews {
		names = append(names, "previews")
	}
	return names
}

// prodHasSecrets reports whether prod's values.yaml lists any secrets, so
// the caller can say they were not copied to staging.
func prodHasSecrets(valuesYAML []byte) bool {
	var v struct {
		Secrets []string `yaml:"secrets"`
	}
	if err := yaml.Unmarshal(valuesYAML, &v); err != nil {
		return false
	}
	return len(v.Secrets) > 0
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
