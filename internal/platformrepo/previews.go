package platformrepo

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/Itema-as/iidp/internal/render"
)

// PreviewsDir is the directory, beside an Application's Environment
// directories, that holds its Preview Environments' ApplicationSet.
const PreviewsDir = "previews"

// PreviewsPath is application's previews ApplicationSet, relative to the
// Platform repository root. bootstrap/applications.yaml applies it
// ('*/previews/applicationset.yaml'); the '*/*/application.yaml' pattern
// that finds Environments never matches it.
func PreviewsPath(application string) string {
	return path.Join(ApplicationsDir, application, PreviewsDir, "applicationset.yaml")
}

// ErrPreviewsWithoutStaging and ErrPreviewsWithoutBinding are wrapped when
// Preview Environments are asked for an Application that cannot have them:
// a preview renders staging's values with staging's secrets, and the
// generator follows the pull requests of the Application repository the
// Application is bound to.
var (
	ErrPreviewsWithoutStaging = errors.New("previews use staging's secrets; add --staging first")
	ErrPreviewsWithoutBinding = errors.New("previews follow the pull requests of the Application repository, and this Application is bound to none")
)

// previewsPresent reports whether the clone at dir has application's
// previews ApplicationSet.
func previewsPresent(dir, application string) (bool, error) {
	switch _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(PreviewsPath(application)))); {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	default:
		return false, fmt.Errorf("checking for %s: %w", PreviewsPath(application), err)
	}
}

// previewsRepository splits the binding's owner/name, the repository whose
// pull requests ArgoCD's Pull Request generator lists. GitHub still answers for the old name
// after a rename, redirecting to the repository's new one, so a stale name
// keeps previews working until the name is reused.
func previewsRepository(application string, b RepositoryBinding, bound bool) (owner, name string, err error) {
	bind := "iidp app bind " + application + " --repo <owner>/<repository>"
	if !bound || !b.Complete() {
		return "", "", fmt.Errorf("%w: bind it first with %s", ErrPreviewsWithoutBinding, bind)
	}
	owner, name, ok := strings.Cut(b.Repository, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return "", "", fmt.Errorf("%w: %s names no repository as owner/name (%q); bind it again with %s", ErrPreviewsWithoutBinding, RepositoryBindingPath(application), b.Repository, bind)
	}
	return owner, name, nil
}

// writePreviews renders application's previews ApplicationSet from its
// staging Environment's application.yaml as it is in the clone at dir, and
// writes it there. It returns the file's repository-relative path when the
// content changed, and nothing when the file already held exactly this: a
// run that did not change what the previews render from commits nothing
// for them.
func writePreviews(dir, application string, cfg Config, b RepositoryBinding, bound bool) ([]string, error) {
	owner, name, err := previewsRepository(application, b, bound)
	if err != nil {
		return nil, err
	}
	stagingRel := path.Join(EnvironmentDir(application, "staging"), "application.yaml")
	staging, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(stagingRel)))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: %q has no staging Environment", ErrPreviewsWithoutStaging, application)
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", stagingRel, err)
	}
	content, err := render.PreviewApplicationSet(render.Previews{
		Application: application,
		Owner:       owner,
		Repository:  name,
		GitHubAPI:   cfg.GitHubAPI,
		Staging:     staging,
	})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", stagingRel, err)
	}
	rel := PreviewsPath(application)
	abs := filepath.Join(dir, filepath.FromSlash(rel))
	if existing, err := os.ReadFile(abs); err == nil && bytes.Equal(existing, content) {
		return nil, nil
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(abs, content, 0o644); err != nil {
		return nil, err
	}
	return []string{rel}, nil
}

// refreshPreviews rewrites application's previews ApplicationSet, when it
// has one, after this run changed staging's application.yaml, so a preview
// keeps rendering from every source staging has: iidp secret set and
// --postgres add staging's sops/ source there once, and previews must get
// staging's secrets the same way. changed lists the paths the run wrote.
func refreshPreviews(dir, application string, cfg Config, changed []string) ([]string, error) {
	stagingRel := path.Join(EnvironmentDir(application, "staging"), "application.yaml")
	touched := false
	for _, f := range changed {
		if f == stagingRel {
			touched = true
		}
	}
	if !touched {
		return nil, nil
	}
	present, err := previewsPresent(dir, application)
	if err != nil || !present {
		return nil, err
	}
	b, bound, err := ReadRepositoryBinding(dir, application)
	if err != nil {
		return nil, err
	}
	return writePreviews(dir, application, cfg, b, bound)
}

// PreviewAddress is the address of the Preview Environment for pull
// request <number>, spelled with the placeholder the CLI prints.
func PreviewAddress(application, baseDomain string) string {
	return "https://" + application + "-" + render.PreviewEnvironment("<number>") + "." + baseDomain
}
