// Package apprepo creates the Application repository the Create path
// writes to: a new GitHub repository generated from a built-in framework
// template (internal/templates) and pushed as the repository's first
// commit. docs/design.md ("The wizard") and
// docs/implementation-notes/11-cli-create-path.md describe the Create path.
package apprepo

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/Itema-as/iidp/internal/git"
	"github.com/Itema-as/iidp/internal/github"
	"github.com/Itema-as/iidp/internal/templates"
)

// Branch is the branch Create pushes the first commit to and sets as the
// repository's default.
const Branch = "main"

// ErrRepositoryExists is wrapped by Create when owner/name already exists.
// Create always generates a fresh repository; an existing one is what the
// Adopt path (not yet built, see issue #15) is for.
var ErrRepositoryExists = errors.New("repository already exists")

// Owner is where the Application repository is created.
type Owner struct {
	// Login is the GitHub organisation or user login.
	Login string
	// Org is true when Login is a GitHub organisation rather than a user.
	Org bool
}

// Application is what Create needs to make the Application repository.
type Application struct {
	Name      string
	Framework templates.Framework
	Owner     Owner
	Private   bool
}

// Result is what Create wrote and where. On an error after the repository
// was created, Create still returns the Result it has so far (at least
// URL), so the caller can tell the developer what already exists.
type Result struct {
	// URL is the Application repository's web address.
	URL string
	// CloneURL is the git URL the initial commit was pushed to.
	CloneURL string
	// Files are the rendered template's files, relative to the repository
	// root, in the order they were written.
	Files []string
}

// Creator creates Application repositories through the GitHub API.
type Creator struct {
	// Client talks to the GitHub API.
	Client *github.Client
	// Auth is the credential git presents when pushing the initial commit.
	Auth git.Auth
}

// Create makes app's repository on GitHub, renders its Framework's template
// into a temporary directory, and pushes it as the repository's first
// commit on Branch. It refuses if owner/name already exists.
func (c *Creator) Create(ctx context.Context, app Application) (Result, error) {
	exists, err := c.Client.RepositoryExists(ctx, app.Owner.Login, app.Name)
	if err != nil {
		return Result{}, err
	}
	if exists {
		return Result{}, fmt.Errorf("%w: %s/%s; Create always generates a fresh repository, use Adopt (not available yet, see issue #15) for one that already exists", ErrRepositoryExists, app.Owner.Login, app.Name)
	}

	dir, err := os.MkdirTemp("", "iidp-app-")
	if err != nil {
		return Result{}, err
	}
	defer os.RemoveAll(dir)
	files, err := templates.Render(app.Framework, templates.Data{Name: app.Name}, dir)
	if err != nil {
		return Result{}, err
	}

	url := "https://github.com/" + app.Owner.Login + "/" + app.Name
	cloneURL, defaultBranch, err := c.Client.CreateRepository(ctx, app.Owner.Login, app.Owner.Org, app.Name, app.Private)
	if err != nil {
		return Result{}, err
	}
	res := Result{URL: url, CloneURL: cloneURL, Files: files}

	repo, err := git.Init(ctx, dir, Branch, cloneURL, c.Auth)
	if err != nil {
		return res, fmt.Errorf("preparing the initial commit for %s/%s: %w", app.Owner.Login, app.Name, err)
	}
	if err := repo.Add(ctx, files...); err != nil {
		return res, err
	}
	if err := repo.Commit(ctx, "Initial commit from iidp"); err != nil {
		return res, err
	}
	if err := repo.Push(ctx, Branch); err != nil {
		return res, fmt.Errorf("pushing the initial commit to %s/%s: %w", app.Owner.Login, app.Name, err)
	}
	if defaultBranch != Branch {
		if err := c.Client.SetDefaultBranch(ctx, app.Owner.Login, app.Name, Branch); err != nil {
			return res, err
		}
	}
	return res, nil
}
