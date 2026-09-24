package apprepo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Itema-as/iidp/internal/git"
	"github.com/Itema-as/iidp/internal/github"
	"github.com/Itema-as/iidp/internal/migrate"
	"github.com/Itema-as/iidp/internal/platform"
	"github.com/Itema-as/iidp/internal/platformrepo"
	"github.com/Itema-as/iidp/internal/templates"
)

// AdoptBranch is the branch Adopt commits to and opens the pull request
// from, named for the Platform rather than the Application: every Adopt
// run against a given repository uses the same branch, so a second run
// before the first pull request is merged or closed is refused instead of
// opening a competing one (docs/implementation-notes/15-cli-adopt-path.md).
const AdoptBranch = "iidp/adopt"

// ErrNoPushAccess is wrapped when the developer's token has no push access
// to the target repository.
var ErrNoPushAccess = errors.New("no push access to the repository")

// ErrBranchExists is wrapped when AdoptBranch already exists on the target
// repository.
var ErrBranchExists = errors.New("branch already exists")

// ErrNothingToAdd is wrapped when the target repository already has both a
// Dockerfile and an identical deploy workflow: there is nothing left for
// Adopt to add, so no branch, commit or pull request is made.
var ErrNothingToAdd = errors.New("nothing to add")

// Detection is what cloning an Application repository's default branch
// found: whether it already has a Dockerfile (Adopt never modifies it) and,
// when it does not, which framework was detected to generate one from, plus
// any migration tooling found — the same detection Create runs against its
// own freshly rendered template (docs/implementation-notes/13-cli-capabilities.md),
// here against the real clone, per the ticket's "migration tooling
// detection runs against the repository's default branch".
type Detection struct {
	HasDockerfile bool
	// Framework is "" when HasDockerfile is true: there is nothing to
	// derive a Dockerfile from, and none is generated.
	Framework templates.Framework
	// HasDeployWorkflow is true when the repository already has a file at
	// templates.DeployWorkflowPath — identical to what Adopt would
	// generate or not, Adopt never writes over it
	// (docs/implementation-notes/15-cli-adopt-path.md).
	HasDeployWorkflow bool
	Migration         migrate.Detection
	MigrationOK       bool
}

// Files reports which paths Adopt would add for this Detection: a
// Dockerfile and .dockerignore when none exists, and the deploy workflow
// when none exists there — sorted, the same order Adopt itself writes
// them in. A preview (the wizard's summary) uses this to list exactly what
// the pull request will add before anything happens.
func (d Detection) Files() []string {
	var files []string
	if !d.HasDockerfile {
		files = append(files, "Dockerfile", ".dockerignore")
	}
	if !d.HasDeployWorkflow {
		files = append(files, templates.DeployWorkflowPath)
	}
	sort.Strings(files)
	return files
}

// ResolveKind decides the Kind Adopt will use. explicit (the developer's
// --kind, or "" when not given) always wins when given, overriding
// whatever detection would otherwise derive. Left empty, it is derived
// from det: refused when the repository already has a Dockerfile (there is
// no way to tell a Static site image from a Web service one from an
// existing Dockerfile alone) or when no known framework was detected (the
// Other stub, same as Create) — recorded in
// docs/implementation-notes/15-cli-adopt-path.md.
func ResolveKind(explicit string, det Detection) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	switch {
	case det.HasDockerfile:
		return "", errors.New("an existing Dockerfile was found; --kind is required (web-service or static-site), since iidp cannot tell a Static site image from a Web service one from an existing Dockerfile alone")
	case det.Framework == templates.Other:
		return "", errors.New("no known framework was detected (looked for Next.js and Vite React); --kind is required, and the generated Dockerfile will be the Other stub")
	default:
		return det.Framework.Kind(), nil
	}
}

// AdoptPreview is what Adopter.Preview reports about owner/name without writing
// or pushing anything: the wizard's source of truth for deciding whether to
// ask Kind (skipped whenever it can be resolved without asking, per
// docs/implementation-notes/15-cli-adopt-path.md) and for showing the
// migration command suggestion before the summary asks for confirmation.
type AdoptPreview struct {
	DefaultBranch string
	CanPush       bool
	// BranchExists is true when AdoptBranch already exists on the
	// repository: Adopt will refuse, so the wizard can say so immediately
	// rather than asking every other question first.
	BranchExists bool
	Detection
}

// Adopter opens a pull request against an existing Application repository:
// GitHub API reads to validate it, a shallow clone of its default branch to
// detect what is missing, then a commit on AdoptBranch with only the added
// files, pushed and opened as a pull request through the API.
type Adopter struct {
	// Client talks to the GitHub API.
	Client *github.Client
	// Auth is the credential git presents when cloning and pushing: the
	// same developer token used for the Platform repository.
	Auth git.Auth
}

// Preview clones owner/name's default branch to report what Adopt would
// do — and whether AdoptBranch already exists — without writing anything.
func (a *Adopter) Preview(ctx context.Context, owner, name string) (AdoptPreview, error) {
	repo, err := a.Client.GetRepository(ctx, owner, name)
	if err != nil {
		return AdoptPreview{}, err
	}
	branchExists, err := a.Client.BranchExists(ctx, owner, name, AdoptBranch)
	if err != nil {
		return AdoptPreview{}, err
	}
	det, err := a.detect(ctx, repo.CloneURL, repo.DefaultBranch)
	if err != nil {
		return AdoptPreview{}, err
	}
	return AdoptPreview{
		DefaultBranch: repo.DefaultBranch,
		CanPush:       repo.CanPush,
		BranchExists:  branchExists,
		Detection:     det,
	}, nil
}

// AdoptRequest is what Adopt needs to write and open the pull request.
type AdoptRequest struct {
	Owner string
	Name  string
	// AppName is the Application name written into the generated files
	// (the deploy workflow's image reference and comments): the resolved
	// name, which defaults to Name but can be overridden with --name.
	AppName string
	// Kind, when non-empty, is the developer-given --kind and is always
	// used as the final Kind, overriding whatever would otherwise be
	// derived from detection
	// (docs/implementation-notes/15-cli-adopt-path.md). Left empty, Adopt
	// derives it: refused when the repository already has a Dockerfile
	// (there is no way to tell a Static site image from a Web service one
	// from an existing Dockerfile alone) or when no known framework was
	// detected (the Other stub, same as Create).
	Kind string
	// Postgres is the developer's --postgres: Adopt refuses a Static site
	// Kind (given explicitly or derived) with Postgres requested, the same
	// rule createOptions.plan applies to Create, but checked here, right
	// after Kind is resolved and before anything is written, committed or
	// pushed — never after the pull request already exists
	// (docs/implementation-notes/15-cli-adopt-path.md).
	Postgres bool
	// Scopes are the developer's token's OAuth scopes, as
	// github.Client.TokenScopes read them. When the pull request would add
	// the deploy workflow, Adopt refuses with CheckWorkflowScope right
	// after detection, before anything is written or pushed; a repository
	// that already has the workflow needs no scope. The zero value
	// (unknown) never refuses.
	Scopes github.TokenScopes
}

// AdoptResult is what Adopt wrote and opened.
type AdoptResult struct {
	RepoURL        string
	PullRequestURL string
	DefaultBranch  string
	Branch         string
	// Files are the paths added to the pull request, relative to the
	// repository root, sorted.
	Files []string
	Kind  string
	Detection
}

// Adopt reads req.Owner/req.Name through the GitHub API, refuses without
// push access or with AdoptBranch already present, clones the default
// branch, detects what is missing (refusing a token without the workflow
// scope when that includes the deploy workflow), writes only that (a Dockerfile and
// .dockerignore when none exists, the deploy workflow unless one already
// exists there — identical or not, Adopt never modifies an existing file),
// commits as the developer, pushes AdoptBranch and opens a pull request
// against the default branch.
func (a *Adopter) Adopt(ctx context.Context, req AdoptRequest) (AdoptResult, error) {
	repo, err := a.Client.GetRepository(ctx, req.Owner, req.Name)
	if err != nil {
		return AdoptResult{}, err
	}
	if !repo.CanPush {
		return AdoptResult{}, fmt.Errorf("%w: you do not have write access to %s/%s; Adopt needs it to open a pull request", ErrNoPushAccess, req.Owner, req.Name)
	}
	exists, err := a.Client.BranchExists(ctx, req.Owner, req.Name, AdoptBranch)
	if err != nil {
		return AdoptResult{}, err
	}
	if exists {
		return AdoptResult{}, fmt.Errorf("%w: %s already exists on %s/%s; merge or delete it before adopting again", ErrBranchExists, AdoptBranch, req.Owner, req.Name)
	}

	dir, err := os.MkdirTemp("", "iidp-adopt-")
	if err != nil {
		return AdoptResult{}, err
	}
	defer os.RemoveAll(dir)
	gitRepo, err := git.Clone(ctx, repo.CloneURL, repo.DefaultBranch, dir, a.Auth)
	if err != nil {
		return AdoptResult{}, fmt.Errorf("cloning %s/%s: %w", req.Owner, req.Name, err)
	}
	det, err := detectDir(dir)
	if err != nil {
		return AdoptResult{}, err
	}

	kind, err := ResolveKind(req.Kind, det)
	if err != nil {
		return AdoptResult{}, fmt.Errorf("%s/%s: %w", req.Owner, req.Name, err)
	}
	if req.Postgres && kind == platformrepo.KindStaticSite {
		return AdoptResult{}, fmt.Errorf("%s/%s: --postgres needs --kind web-service (or a framework/detected framework that derives it); a Static site has no server to use a database", req.Owner, req.Name)
	}

	files := det.Files()
	if len(files) == 0 {
		return AdoptResult{}, fmt.Errorf("%w: %s/%s already has a Dockerfile and a deploy workflow", ErrNothingToAdd, req.Owner, req.Name)
	}
	if !det.HasDeployWorkflow {
		if err := CheckWorkflowScope(req.Scopes); err != nil {
			return AdoptResult{}, err
		}
	}

	iidpVersion := templateIidpVersion()
	data := templates.Data{Name: req.AppName, Owner: strings.ToLower(req.Owner), IidpVersion: iidpVersion}

	if !det.HasDockerfile {
		if _, err := templates.RenderDockerfile(det.Framework, data, dir); err != nil {
			return AdoptResult{}, err
		}
	}
	if !det.HasDeployWorkflow {
		dest := filepath.Join(dir, filepath.FromSlash(templates.DeployWorkflowPath))
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return AdoptResult{}, err
		}
		if err := os.WriteFile(dest, templates.RenderDeployWorkflow(data), 0o644); err != nil {
			return AdoptResult{}, err
		}
	}

	if err := gitRepo.CreateBranch(ctx, AdoptBranch); err != nil {
		return AdoptResult{}, err
	}
	if err := gitRepo.Add(ctx, files...); err != nil {
		return AdoptResult{}, err
	}
	if err := gitRepo.Commit(ctx, "iidp app create --path adopt "+req.AppName); err != nil {
		return AdoptResult{}, err
	}
	if err := gitRepo.Push(ctx, AdoptBranch); err != nil {
		return AdoptResult{}, fmt.Errorf("pushing %s to %s/%s: %w", AdoptBranch, req.Owner, req.Name, err)
	}

	prURL, err := a.Client.CreatePullRequest(ctx, req.Owner, req.Name, github.PullRequest{
		Title: "Add the iidp deploy pipeline",
		Head:  AdoptBranch,
		Base:  repo.DefaultBranch,
		Body:  pullRequestBody(req, files, det),
	})
	if err != nil {
		return AdoptResult{}, err
	}

	return AdoptResult{
		RepoURL:        "https://github.com/" + req.Owner + "/" + req.Name,
		PullRequestURL: prURL,
		DefaultBranch:  repo.DefaultBranch,
		Branch:         AdoptBranch,
		Files:          files,
		Kind:           kind,
		Detection:      det,
	}, nil
}

// detect clones cloneURL's branch shallowly into a temporary directory,
// removed before it returns, and inspects it.
func (a *Adopter) detect(ctx context.Context, cloneURL, branch string) (Detection, error) {
	dir, err := os.MkdirTemp("", "iidp-adopt-detect-")
	if err != nil {
		return Detection{}, err
	}
	defer os.RemoveAll(dir)
	if _, err := git.Clone(ctx, cloneURL, branch, dir, a.Auth); err != nil {
		return Detection{}, fmt.Errorf("cloning %s: %w", cloneURL, err)
	}
	return detectDir(dir)
}

// detectDir is Detection's logic against an already-cloned (or otherwise
// present) directory: an existing Dockerfile, else the framework detected
// from package.json, plus migration tooling (internal/migrate), exactly
// the way Adopt and Adopter.Preview both need it.
func detectDir(dir string) (Detection, error) {
	var det Detection
	det.HasDockerfile = fileExists(filepath.Join(dir, "Dockerfile"))
	if !det.HasDockerfile {
		fw, err := detectFramework(dir)
		if err != nil {
			return Detection{}, err
		}
		det.Framework = fw
	}
	det.HasDeployWorkflow = fileExists(filepath.Join(dir, filepath.FromSlash(templates.DeployWorkflowPath)))
	m, ok, err := migrate.Detect(dir)
	if err != nil {
		return Detection{}, err
	}
	det.Migration, det.MigrationOK = m, ok
	return det, nil
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// detectFramework looks at package.json's dependencies and devDependencies
// for next (Next.js) or vite plus react (Vite React) — the same two
// frameworks Create generates from, matching docs/design.md's wizard text
// ("Next.js from next in package.json dependencies, Vite React from vite
// plus react"). Both dependencies and devDependencies are checked, since a
// project scaffolded by the Vite tooling itself keeps vite as a
// devDependency with react as a plain dependency. Anything else, including
// no package.json at all, is Other.
func detectFramework(dir string) (templates.Framework, error) {
	data, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return templates.Other, nil
		}
		return "", fmt.Errorf("reading package.json: %w", err)
	}
	var pkg struct {
		Dependencies    map[string]string `json:"dependencies"`
		DevDependencies map[string]string `json:"devDependencies"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		return "", fmt.Errorf("parsing package.json: %w", err)
	}
	has := func(name string) bool {
		if _, ok := pkg.Dependencies[name]; ok {
			return true
		}
		_, ok := pkg.DevDependencies[name]
		return ok
	}
	switch {
	case has("next"):
		return templates.NextJS, nil
	case has("vite") && has("react"):
		return templates.ViteReact, nil
	default:
		return templates.Other, nil
	}
}

// pullRequestBody describes what each added file does and what happens on
// merge, per docs/design.md's Adopt paragraph and
// docs/implementation-notes/12-deploy-workflow.md's write-back.
func pullRequestBody(req AdoptRequest, files []string, det Detection) string {
	var b strings.Builder
	fmt.Fprintf(&b, "iidp adopts %s onto Itema's Platform. This pull request adds only what is missing; nothing else in the repository is created, modified or deleted.\n\n", req.AppName)
	b.WriteString("## What each file does\n\n")
	for _, f := range files {
		switch f {
		case "Dockerfile":
			fmt.Fprintf(&b, "- `Dockerfile`: builds the image the deploy workflow below pushes (%s detected).\n", frameworkLabel(det.Framework))
		case ".dockerignore":
			b.WriteString("- `.dockerignore`: keeps the build context small and the image free of files it does not need.\n")
		case templates.DeployWorkflowPath:
			b.WriteString("- `.github/workflows/deploy.yaml`: on a push to the default branch, builds the image with buildx, pushes it to GHCR tagged with the commit SHA, and writes that tag into the Platform repository (`iidp ci set-image`); on a `v*` tag, retags the same image with the version, with no rebuild, and promotes it to prod. **The first merged run of this workflow is what deploys the Application for the first time.**\n")
		}
	}
	if det.MigrationOK {
		fmt.Fprintf(&b, "\nDetected %s in the repository; if the Postgres Capability is enabled, its migration command is set to `%s`.\n", det.Migration.Tool, det.Migration.Command)
	}
	if !strings.EqualFold(req.Owner, platform.Org) {
		fmt.Fprintf(&b, "\n%s is not the %s org, so the deploy workflow's write-back step needs credentials that only reach org repositories automatically. Once this is merged, add them by hand in this repository's settings: secret `IIDP_DEPLOY_APP_PRIVATE_KEY` (the org GitHub App's private key) and variable `IIDP_DEPLOY_APP_ID` (the org GitHub App's id).\n", req.Owner, platform.Org)
	}
	return b.String()
}

func frameworkLabel(fw templates.Framework) string {
	switch fw {
	case templates.NextJS:
		return "Next.js"
	case templates.ViteReact:
		return "Vite React"
	default:
		return "no known framework; a commented stub"
	}
}
