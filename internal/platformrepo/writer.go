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
)

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
}

// Result is what CreateApplication wrote and where it can be seen.
type Result struct {
	Config Config
	// Files are the paths written, relative to the Platform repository root.
	Files []string
	// Address is the prod Environment's URL.
	Address string
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
}

// CreateApplication adds the prod Environment of app to the Platform
// repository: it clones main, validates that the Application does not
// exist, writes the Environment's files, commits and pushes. A push refused
// because main moved is retried once from a fresh clone; if the Application
// appeared in the meantime, the retry fails with ErrApplicationExists.
func (w *Writer) CreateApplication(ctx context.Context, app Application) (Result, error) {
	res, err := w.attempt(ctx, app, false)
	if errors.Is(err, git.ErrPushRejected) {
		res, err = w.attempt(ctx, app, true)
	}
	if errors.Is(err, git.ErrPushRejected) {
		return Result{}, fmt.Errorf("main of %s moved twice while this command ran; nothing was written, run it again", platform.Repository)
	}
	return res, err
}

func (w *Writer) attempt(ctx context.Context, app Application, retry bool) (Result, error) {
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
	switch _, err := os.Stat(filepath.Join(dir, ApplicationsDir, app.Name)); {
	case err == nil && retry:
		return Result{}, fmt.Errorf("%w: %q was added to %s while this command ran; nothing was written", ErrApplicationExists, app.Name, platform.Repository)
	case err == nil:
		return Result{}, fmt.Errorf("%w: %q already has a directory under %s/ in %s", ErrApplicationExists, app.Name, ApplicationsDir, platform.Repository)
	case !errors.Is(err, fs.ErrNotExist):
		return Result{}, fmt.Errorf("checking for an existing Application: %w", err)
	}

	files, err := w.writeEnvironment(dir, cfg, app, "prod")
	if err != nil {
		return Result{}, err
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
	return Result{
		Config:  cfg,
		Files:   files,
		Address: "https://" + app.Name + "." + cfg.BaseDomain,
	}, nil
}

// writeEnvironment renders and writes the files of one Environment into
// the clone at dir and returns their repository-relative paths.
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
