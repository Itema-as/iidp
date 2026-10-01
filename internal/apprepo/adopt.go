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

	"github.com/Itema-as/iidp/internal/appconfig"
	"github.com/Itema-as/iidp/internal/git"
	"github.com/Itema-as/iidp/internal/github"
	"github.com/Itema-as/iidp/internal/migrate"
	"github.com/Itema-as/iidp/internal/platformrepo"
	"github.com/Itema-as/iidp/internal/templates"
)

// AdoptBranch is the branch Adopt opens the pull request from. It is fixed,
// so a second run before the first pull request is merged or closed is
// refused instead of opening a competing one.
const AdoptBranch = "iidp/adopt"

// ErrNoPushAccess is wrapped when the developer's token has no push access
// to the target repository.
var ErrNoPushAccess = errors.New("no push access to the repository")

// ErrBranchExists is wrapped when AdoptBranch already exists on the target
// repository.
var ErrBranchExists = errors.New("branch already exists")

// ErrNothingToAdd is wrapped when the target repository already has every
// file Adopt would add.
var ErrNothingToAdd = errors.New("nothing to add")

// Detection is what an Application repository's default branch already
// has, and which framework and migration tooling it uses. Adopt never
// writes over an existing file, except to make a Dockerfile run as non-root.
type Detection struct {
	HasDockerfile bool
	dockerfile    []byte
	// Framework is "" when HasDockerfile is true: no Dockerfile is
	// generated.
	Framework         templates.Framework
	HasDeployWorkflow bool
	HasAppConfig      bool
	Migration         migrate.Detection
	MigrationOK       bool
}

// NonRoot is how Adopt makes the repository's own Dockerfile run as
// non-root, or nothing when it has none: a generated one already does.
func (d Detection) NonRoot(kind string) NonRootFix {
	if !d.HasDockerfile {
		return NonRootFix{}
	}
	return fixNonRoot(d.dockerfile, kind)
}

// Files reports, sorted, which paths Adopt would add or change for this
// Detection and kind.
func (d Detection) Files(kind string) []string {
	var files []string
	if !d.HasDockerfile {
		files = append(files, "Dockerfile", ".dockerignore")
	} else if d.NonRoot(kind).Dockerfile != nil {
		files = append(files, "Dockerfile")
	}
	if !d.HasDeployWorkflow {
		files = append(files, templates.DeployWorkflowPath)
	}
	if !d.HasAppConfig {
		files = append(files, templates.AppConfigPath)
	}
	sort.Strings(files)
	return files
}

// MigrationCommand is the migration command Adopt writes into iidp.yaml:
// the developer's own when set, even as "", otherwise, with Postgres, the
// one detected. Without Postgres the Deploy gate would refuse a command.
func (d Detection) MigrationCommand(postgres bool, given string, set bool) string {
	switch {
	case set:
		return given
	case postgres && d.MigrationOK:
		return d.Migration.Command
	default:
		return ""
	}
}

// ResolveKind decides the Kind Adopt will use: explicit (the developer's
// --kind) when given, otherwise the detected framework's. Without --kind it
// refuses an existing Dockerfile or an unknown framework, since neither
// says which Kind the image is.
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

// AdoptPreview is what Adopter.Preview reports about owner/name without
// writing anything.
type AdoptPreview struct {
	DefaultBranch string
	CanPush       bool
	// BranchExists is true when AdoptBranch already exists, so Adopt will
	// refuse.
	BranchExists bool
	Detection
}

// Adopter opens a pull request that adds only what is missing to an
// existing Application repository.
type Adopter struct {
	Client *github.Client
	// Auth is the credential git presents when cloning and pushing.
	Auth git.Auth
}

// Preview clones owner/name's default branch to report what Adopt would do,
// without writing anything.
func (a *Adopter) Preview(ctx context.Context, owner, name string) (AdoptPreview, error) {
	repo, err := a.Client.GetRepository(ctx, owner, name)
	if err != nil {
		return AdoptPreview{}, err
	}
	if _, err := BindingForRepository(owner+"/"+name, repo); err != nil {
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
	// AppName is the Application name written into the generated files,
	// which may differ from Name.
	AppName string
	// Kind is the developer's --kind; "" lets ResolveKind derive it.
	Kind string
	// Postgres is the developer's --postgres. A Static site with Postgres
	// is refused before anything is pushed.
	Postgres bool
	// Scopes are the developer's token's OAuth scopes, checked only when
	// the pull request would add the deploy workflow.
	Scopes        github.TokenScopes
	DeployGateURL string
	// MigrationCommand and MigrationCommandSet are the developer's
	// --migration-command, and whether it was given at all.
	MigrationCommand    string
	MigrationCommandSet bool
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
	Binding platformrepo.RepositoryBinding
	// MigrationCommand is the one written when Adopt added iidp.yaml, or
	// the one the developer should check an existing file for.
	MigrationCommand string
	// NonRoot is what Adopt changed in the repository's own Dockerfile
	// so that it runs as non-root, or what the developer must change.
	NonRoot NonRootFix
}

// Adopt writes the files the repository is missing, plus any non-root fix
// to its Dockerfile, to AdoptBranch, and opens a pull request against the
// default branch. Every check runs before anything is pushed.
func (a *Adopter) Adopt(ctx context.Context, req AdoptRequest) (AdoptResult, error) {
	repo, err := a.Client.GetRepository(ctx, req.Owner, req.Name)
	if err != nil {
		return AdoptResult{}, err
	}
	binding, err := BindingForRepository(req.Owner+"/"+req.Name, repo)
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

	nonRoot := det.NonRoot(kind)
	files := det.Files(kind)
	if len(files) == 0 {
		err := fmt.Errorf("%w: %s/%s already has a Dockerfile, a deploy workflow and %s", ErrNothingToAdd, req.Owner, req.Name, templates.AppConfigPath)
		if nonRoot.Advice != "" {
			err = fmt.Errorf("%w. Its Dockerfile may run as root, which the Platform refuses: %s", err, nonRoot.Advice)
		}
		return AdoptResult{}, err
	}
	if !det.HasDeployWorkflow {
		if err := CheckWorkflowScope(req.Scopes); err != nil {
			return AdoptResult{}, err
		}
	}

	migrationCommand := det.MigrationCommand(req.Postgres, req.MigrationCommand, req.MigrationCommandSet)
	data := templates.Data{Name: req.AppName, DeployWorkflowRef: DeployWorkflowRef(), DeployGateURL: req.DeployGateURL, MigrationCommand: migrationCommand}

	if !det.HasDockerfile {
		if _, err := templates.RenderDockerfile(det.Framework, data, dir); err != nil {
			return AdoptResult{}, err
		}
	} else if nonRoot.Dockerfile != nil {
		if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), nonRoot.Dockerfile, 0o644); err != nil {
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
	if !det.HasAppConfig {
		if err := os.WriteFile(filepath.Join(dir, templates.AppConfigPath), templates.RenderAppConfig(data), 0o644); err != nil {
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
		Body:  pullRequestBody(req, files, det, migrationCommand, nonRoot),
	})
	if err != nil {
		return AdoptResult{}, err
	}

	return AdoptResult{
		RepoURL:          "https://github.com/" + req.Owner + "/" + req.Name,
		PullRequestURL:   prURL,
		DefaultBranch:    repo.DefaultBranch,
		Branch:           AdoptBranch,
		Files:            files,
		Kind:             kind,
		Detection:        det,
		Binding:          binding,
		MigrationCommand: migrationCommand,
		NonRoot:          nonRoot,
	}, nil
}

// detect clones cloneURL's branch into a temporary directory and inspects
// it.
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

func detectDir(dir string) (Detection, error) {
	var det Detection
	det.HasDockerfile = fileExists(filepath.Join(dir, "Dockerfile"))
	if det.HasDockerfile {
		dockerfile, err := os.ReadFile(filepath.Join(dir, "Dockerfile"))
		if err != nil {
			return Detection{}, err
		}
		det.dockerfile = dockerfile
	} else {
		fw, err := detectFramework(dir)
		if err != nil {
			return Detection{}, err
		}
		det.Framework = fw
	}
	det.HasDeployWorkflow = fileExists(filepath.Join(dir, filepath.FromSlash(templates.DeployWorkflowPath)))
	det.HasAppConfig = fileExists(filepath.Join(dir, templates.AppConfigPath))
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

// detectFramework looks in package.json for next (Next.js) or vite plus
// react (Vite React); anything else is Other. devDependencies count too,
// since a Vite scaffold keeps vite there.
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
// merge.
func pullRequestBody(req AdoptRequest, files []string, det Detection, migrationCommand string, nonRoot NonRootFix) string {
	var b strings.Builder
	if nonRoot.Dockerfile != nil {
		fmt.Fprintf(&b, "iidp adopts %s onto Itema's Platform. This pull request adds only what is missing, and changes the Dockerfile only so that the image runs as a non-root user; nothing else in the repository is created, modified or deleted.\n\n", req.AppName)
	} else {
		fmt.Fprintf(&b, "iidp adopts %s onto Itema's Platform. This pull request adds only what is missing; nothing else in the repository is created, modified or deleted.\n\n", req.AppName)
	}
	b.WriteString("## What each file does\n\n")
	for _, f := range files {
		switch {
		case f == "Dockerfile" && det.HasDockerfile:
			fmt.Fprintf(&b, "- `Dockerfile`: changed so the image runs as a non-root user, as the Platform requires of every container. %s\n", nonRoot.Change)
		case f == "Dockerfile":
			fmt.Fprintf(&b, "- `Dockerfile`: builds the image the deploy workflow below pushes (%s detected).\n", frameworkLabel(det.Framework))
		case f == ".dockerignore":
			b.WriteString("- `.dockerignore`: keeps the build context small and the image free of files it does not need.\n")
		case f == templates.DeployWorkflowPath:
			b.WriteString("- `.github/workflows/deploy.yaml`: calls iidp's reusable deploy workflow (`" + DeployWorkflowRef() + "`). On a push to the default branch, it builds the image with buildx, pushes it to GHCR tagged with the commit SHA, and deploys that tag through the Platform's Deploy gate (`iidp ci set-image`); on a `v*` tag, it retags the same image with the version, with no rebuild, and promotes it to prod. **The first merged run of this workflow is what deploys the Application for the first time.**\n")
		case f == templates.AppConfigPath:
			if migrationCommand != "" {
				fmt.Fprintf(&b, "- `%s`: settings the deploy workflow sends the Platform with every deploy, from the commit it deploys. It sets the migration command, `%s`, which runs before every rollout, in the new image, with `DATABASE_URL` set.\n", templates.AppConfigPath, migrationCommand)
			} else {
				fmt.Fprintf(&b, "- `%s`: settings the deploy workflow sends the Platform with every deploy, from the commit it deploys. It sets no migration command yet; its comments say how to add one.\n", templates.AppConfigPath)
			}
		}
	}
	if det.MigrationOK {
		if req.Postgres {
			fmt.Fprintf(&b, "\nDetected %s in the repository.\n", det.Migration.Tool)
		} else {
			fmt.Fprintf(&b, "\nDetected %s in the repository. The Application has no Postgres Capability, so `%s` sets no migration command; add `%s` there once it has one.\n", det.Migration.Tool, templates.AppConfigPath, appconfig.MigrationCommandLine(det.Migration.Command))
		}
	}
	if nonRoot.Advice != "" {
		fmt.Fprintf(&b, "\n## Before merging: the Dockerfile may run as root\n\n%s Until it does, the Application's Pods are refused.\n", nonRoot.Advice)
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
