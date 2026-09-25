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
	"strings"

	"github.com/Itema-as/iidp/internal/git"
	"github.com/Itema-as/iidp/internal/github"
	"github.com/Itema-as/iidp/internal/platform"
	"github.com/Itema-as/iidp/internal/platformrepo"
	"github.com/Itema-as/iidp/internal/templates"
	"github.com/Itema-as/iidp/internal/version"
)

// Branch is the branch Create pushes the first commit to and sets as the
// repository's default.
const Branch = "main"

// ErrRepositoryExists is wrapped by Create when owner/name already exists.
// Create always generates a fresh repository; an existing one is what
// Adopt (--path adopt --repo owner/name) is for.
var ErrRepositoryExists = errors.New("repository already exists")

// Application is what Create needs to make the Application repository. It
// is always created in platform.Org: only repositories in the org can be
// Applications (docs/adr/0005-private-application-repositories-on-github-free.md).
type Application struct {
	Name      string
	Framework templates.Framework
	Private   bool
	// DeployGateURL is the Platform's Deploy gate, rendered into the
	// deploy workflow (templates.Data.DeployGateURL).
	DeployGateURL string
	// MigrationCommand is written into the repository's iidp.yaml; "" for
	// none.
	MigrationCommand string
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
	// DeployWorkflowRef is the reusable workflow, at its major tag, that
	// .github/workflows/deploy.yaml calls.
	DeployWorkflowRef string
	// Binding is the new repository's ids, as GitHub reported them on
	// creation, for the Platform repository to bind the Application to.
	Binding platformrepo.RepositoryBinding
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
	owner := platform.Org
	exists, err := c.Client.RepositoryExists(ctx, owner, app.Name)
	if err != nil {
		return Result{}, err
	}
	if exists {
		return Result{}, fmt.Errorf("%w: %s/%s; Create always generates a fresh repository, use --path adopt --repo %s/%s for one that already exists", ErrRepositoryExists, owner, app.Name, owner, app.Name)
	}

	dir, err := os.MkdirTemp("", "iidp-app-")
	if err != nil {
		return Result{}, err
	}
	defer os.RemoveAll(dir)
	workflowRef := DeployWorkflowRef()
	files, err := templates.Render(app.Framework, templates.Data{
		Name:              app.Name,
		DeployWorkflowRef: workflowRef,
		DeployGateURL:     app.DeployGateURL,
		MigrationCommand:  app.MigrationCommand,
	}, dir)
	if err != nil {
		return Result{}, err
	}

	url := "https://github.com/" + owner + "/" + app.Name
	created, err := c.Client.CreateRepository(ctx, owner, app.Name, app.Private)
	if err != nil {
		return Result{}, err
	}
	res := Result{URL: url, CloneURL: created.CloneURL, Files: files, DeployWorkflowRef: workflowRef}
	res.Binding, err = Binding(owner+"/"+app.Name, created.ID, created.Owner)
	if err != nil {
		return res, err
	}

	repo, err := git.Init(ctx, dir, Branch, created.CloneURL, c.Auth)
	if err != nil {
		return res, fmt.Errorf("preparing the initial commit for %s/%s: %w", owner, app.Name, err)
	}
	if err := repo.Add(ctx, files...); err != nil {
		return res, err
	}
	if err := repo.Commit(ctx, "Initial commit from iidp"); err != nil {
		return res, err
	}
	if err := repo.Push(ctx, Branch); err != nil {
		return res, fmt.Errorf("pushing the initial commit to %s/%s: %w", owner, app.Name, err)
	}
	if created.DefaultBranch != Branch {
		if err := c.Client.SetDefaultBranch(ctx, owner, app.Name, Branch); err != nil {
			return res, err
		}
	}
	return res, nil
}

// DeployWorkflowRef is the reusable deploy workflow at the major tag of the
// running binary's version, for example
// Itema-as/iidp/.github/workflows/application-deploy.yaml@v0. Callers get
// every release in that major version with no edit. A dev build, or any
// version that isn't vX.Y.Z-shaped, gets @v0, the first major.
func DeployWorkflowRef() string {
	return platform.DeployWorkflow + "@" + majorTag(version.Version)
}

func majorTag(v string) string {
	major, _, ok := strings.Cut(strings.TrimPrefix(v, "v"), ".")
	if !ok || major == "" || strings.Trim(major, "0123456789") != "" {
		return "v0"
	}
	return "v" + major
}
