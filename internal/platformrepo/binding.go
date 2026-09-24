package platformrepo

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"github.com/Itema-as/iidp/internal/git"
	"github.com/Itema-as/iidp/internal/platform"
)

// RepositoryFile is the name of the file, directly under
// applications/<name>/, that binds an Application to its Application
// repository by GitHub's numeric ids. It sits beside the Environment
// directories rather than inside them because the binding is the
// Application's, not an Environment's, and outside them so the
// '*/*/application.yaml' glob that makes ArgoCD apply Environments never
// matches it (docs/platform-repository.md).
const RepositoryFile = "repository.yaml"

// RepositoryBindingPath is the binding file of application, relative to
// the Platform repository root.
func RepositoryBindingPath(application string) string {
	return path.Join(ApplicationsDir, application, RepositoryFile)
}

// ErrAlreadyBound is wrapped by BindRepository when the Application is
// already bound to a different repository and rebinding was not asked for.
var ErrAlreadyBound = errors.New("Application is bound to another repository")

// RepositoryBinding is the Application repository an Application is bound
// to, as GitHub identifies it: by numeric ids that survive a rename and a
// transfer, and that a repository deleted and recreated under the same name
// does not get back. The Deploy gate (#60) authorises a deploy only when the
// caller's GitHub Actions OIDC token carries these ids as repository_id and
// repository_owner_id (docs/adr/0005-private-application-repositories-on-github-free.md).
type RepositoryBinding struct {
	// Repository is owner/name when the binding was written, for people
	// reading the file. It goes stale on a rename and is never used to
	// authorise anything.
	Repository string `yaml:"repository"`
	// RepositoryID is the Application repository's numeric id.
	RepositoryID int64 `yaml:"repositoryId"`
	// RepositoryOwnerID is the numeric id of the org that owns it.
	RepositoryOwnerID int64 `yaml:"repositoryOwnerId"`
}

// Complete reports whether b carries both ids. A binding without them
// binds nothing: the Deploy gate treats it as absent.
func (b RepositoryBinding) Complete() bool {
	return b.RepositoryID > 0 && b.RepositoryOwnerID > 0
}

// SameRepository reports whether b and other name the same repository in
// the same org, by id. Repository, the human-readable name, is not
// compared.
func (b RepositoryBinding) SameRepository(other RepositoryBinding) bool {
	return b.RepositoryID == other.RepositoryID && b.RepositoryOwnerID == other.RepositoryOwnerID
}

// renderRepositoryBinding is the content of application's binding file.
func renderRepositoryBinding(application string, b RepositoryBinding) ([]byte, error) {
	body, err := yaml.Marshal(b)
	if err != nil {
		return nil, err
	}
	header := fmt.Sprintf("# The Application repository %s is bound to. The Deploy gate lets only the\n"+
		"# repository with these GitHub ids (an Actions OIDC token's repository_id and\n"+
		"# repository_owner_id) deploy it; repository is the name when it was bound,\n"+
		"# for people to read, and goes stale on a rename.\n"+
		"# Written by iidp (iidp app create, iidp app bind); do not edit by hand.\n", application)
	return append([]byte(header), body...), nil
}

// ReadRepositoryBinding reads application's binding file from a clone of
// the Platform repository at dir, ignoring keys it does not know. ok is
// false when there is no file. A file that is present but not valid YAML
// is an error; one that parses but lacks an id comes back with Complete()
// false. The Deploy gate reads bindings with this; every one of those
// cases except a Complete binding means the Application is unbound
// (docs/platform-repository.md).
func ReadRepositoryBinding(dir, application string) (b RepositoryBinding, ok bool, err error) {
	data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(RepositoryBindingPath(application))))
	if errors.Is(err, fs.ErrNotExist) {
		return RepositoryBinding{}, false, nil
	}
	if err != nil {
		return RepositoryBinding{}, false, err
	}
	if err := yaml.Unmarshal(data, &b); err != nil {
		return RepositoryBinding{}, true, fmt.Errorf("%s is not valid YAML: %w", RepositoryBindingPath(application), err)
	}
	return b, true, nil
}

// writeRepositoryBinding writes application's binding file into the clone
// at dir and returns its repository-relative path.
func writeRepositoryBinding(dir, application string, b RepositoryBinding) (string, error) {
	content, err := renderRepositoryBinding(application, b)
	if err != nil {
		return "", err
	}
	rel := RepositoryBindingPath(application)
	full := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(full, content, 0o644); err != nil {
		return "", err
	}
	return rel, nil
}

// BindResult is what BindRepository did.
type BindResult struct {
	// File is the binding file's path, relative to the Platform repository
	// root.
	File string
	// Previous is the binding the file held before, when there was one.
	Previous *RepositoryBinding
	// Unchanged is true when the Application was already bound to exactly
	// this repository under this name: nothing was committed.
	Unchanged bool
}

// BindRepository binds application, which must have a live Environment, to
// the repository b names, in one commit: the backfill for an Application
// written before Create and Adopt recorded the ids, or written without an
// Application repository at all (app create without --path). A binding to
// the same ids is left alone, or only has its human-readable name
// refreshed; a binding to a different repository is refused unless rebind
// is true. A binding file that is not valid YAML or lacks an id binds
// nothing, so it is replaced without needing rebind. A push refused
// because main moved is retried once from a fresh clone, exactly as
// CreateApplication does.
func (w *Writer) BindRepository(ctx context.Context, application string, b RepositoryBinding, rebind bool) (BindResult, error) {
	if !b.Complete() {
		return BindResult{}, fmt.Errorf("binding %s: the repository id and the owner id are both required", application)
	}
	return runWithRetry(ctx, func(ctx context.Context, _ bool) (BindResult, error) {
		return w.attemptBind(ctx, application, b, rebind)
	})
}

func (w *Writer) attemptBind(ctx context.Context, application string, b RepositoryBinding, rebind bool) (BindResult, error) {
	dir, err := os.MkdirTemp("", "iidp-platform-")
	if err != nil {
		return BindResult{}, err
	}
	defer os.RemoveAll(dir)

	repo, err := git.Clone(ctx, w.URL, Branch, dir, w.Auth)
	if err != nil {
		return BindResult{}, fmt.Errorf("cloning %s: %w", platform.Repository, err)
	}
	live, err := applicationHasLiveEnvironment(dir, application)
	if err != nil {
		return BindResult{}, fmt.Errorf("checking for the Application: %w", err)
	}
	if !live {
		return BindResult{}, fmt.Errorf("%w: %q has no Environment with a live application.yaml under %s/ in %s", ErrApplicationMissing, application, ApplicationsDir, platform.Repository)
	}

	res := BindResult{File: RepositoryBindingPath(application)}
	existing, ok, err := ReadRepositoryBinding(dir, application)
	switch {
	case err != nil:
		// A hand edit left something that is not YAML: it binds nothing,
		// so replacing it cannot take deploy rights away from anyone.
	case ok && existing.Complete():
		res.Previous = &existing
		if existing.SameRepository(b) {
			if existing.Repository == b.Repository {
				res.Unchanged = true
				return res, nil
			}
		} else if !rebind {
			return BindResult{}, fmt.Errorf("%w: %s is bound to %s (repository id %d, owner id %d), not %s (repository id %d); pass --rebind to bind it to %s instead, which stops %s deploying it", ErrAlreadyBound, application, existing.Repository, existing.RepositoryID, existing.RepositoryOwnerID, b.Repository, b.RepositoryID, b.Repository, existing.Repository)
		}
	case ok:
		res.Previous = &existing
	}

	if _, err := writeRepositoryBinding(dir, application, b); err != nil {
		return BindResult{}, err
	}
	if err := repo.Add(ctx, res.File); err != nil {
		return BindResult{}, err
	}
	if err := repo.Commit(ctx, "iidp app bind "+application+" "+b.Repository); err != nil {
		return BindResult{}, err
	}
	if w.BeforePush != nil {
		if err := w.BeforePush(); err != nil {
			return BindResult{}, err
		}
	}
	if err := repo.Push(ctx, Branch); err != nil {
		if errors.Is(err, git.ErrPushRejected) {
			return BindResult{}, err
		}
		return BindResult{}, fmt.Errorf("pushing to %s: %w\nIf this is a permission error, ask the Platform admin for write access to %s", platform.Repository, err, platform.Repository)
	}
	return res, nil
}
