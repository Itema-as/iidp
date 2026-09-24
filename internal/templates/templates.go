// Package templates holds the built-in Application repository templates
// Create renders: a minimal Next.js Web service, a minimal Vite React
// Static site, and a commented Dockerfile stub for a framework iidp does
// not generate. docs/implementation-notes/11-cli-create-path.md records the
// versions pinned and why.
package templates

import (
	"bytes"
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/template"

	"github.com/Itema-as/iidp/internal/appconfig"
	"github.com/Itema-as/iidp/internal/platform"
	"github.com/Itema-as/iidp/internal/platformrepo"
)

// Framework names a built-in template.
type Framework string

const (
	NextJS    Framework = "nextjs"
	ViteReact Framework = "vite-react"
	Other     Framework = "other"
)

// Valid reports whether f is one of the built-in frameworks.
func (f Framework) Valid() bool {
	switch f {
	case NextJS, ViteReact, Other:
		return true
	default:
		return false
	}
}

// Kind is the chart Kind f produces. Other has none: it does not generate a
// working image, so the developer chooses the Kind themselves with --kind.
func (f Framework) Kind() string {
	switch f {
	case NextJS:
		return platformrepo.KindWebService
	case ViteReact:
		return platformrepo.KindStaticSite
	default:
		return ""
	}
}

//go:embed all:nextjs all:vite-react all:other
var files embed.FS

// DeployWorkflowPath is where every framework's rendered template gets the
// shared deploy workflow, relative to the template's root. Exported so the
// Adopt path (internal/apprepo) can compare a target repository's existing
// workflow, if any, at the same path Create writes it to.
const DeployWorkflowPath = ".github/workflows/deploy.yaml"

//go:embed deploy-workflow.yaml
var deployWorkflowSource []byte

// Data is the Application-specific value the templates are rendered with.
type Data struct {
	// Name is the Application's name: used as the package.json name and in
	// the README, wherever the template says what it generated.
	Name string
	// Owner is the lowercased GitHub login the Application repository is
	// created under: the deploy workflow template pushes to
	// ghcr.io/<Owner>/<Name>, matching the image repository iidp writes to
	// the Platform repository
	// (docs/implementation-notes/11-cli-create-path.md, "Image repository").
	Owner string
	// IidpVersion is the iidp release .github/workflows/deploy.yaml pins
	// its own install step to: the version of the iidp binary that
	// rendered the template (internal/version), or "latest" for a dev
	// build ("dev").
	IidpVersion string
	// DeployGateURL is the Platform's Deploy gate,
	// https://deploy.<baseDomain> (platformrepo.Config.DeployGateURL): the
	// deploy workflow calls it, and asks for OIDC tokens with it as the
	// audience. Rendered in because CI cannot read the private Platform
	// repository to find it (docs/implementation-notes/60-deploy-gate.md).
	DeployGateURL string
	// MigrationCommand is written into iidp.yaml (AppConfigPath): the
	// detected or given --migration-command, or "" for none.
	MigrationCommand string
}

// AppConfigPath is where every rendered template gets iidp.yaml, the
// settings the deploy workflow sends the Deploy gate with each deploy
// (internal/appconfig). Like the deploy workflow, it is the same for every
// framework, so it is written here rather than kept in each framework's
// directory.
const AppConfigPath = appconfig.FileName

// RenderAppConfig returns iidp.yaml's rendered bytes for data. The Adopt
// path adds it only when the repository has none.
func RenderAppConfig(data Data) []byte {
	return appconfig.Render(data.MigrationCommand)
}

// Render writes framework's template into dir (which must already exist),
// each embedded file passed through text/template with data, and returns
// the written files' paths relative to dir, sorted.
func Render(framework Framework, data Data, dir string) ([]string, error) {
	if !framework.Valid() {
		return nil, fmt.Errorf("no built-in template for framework %q", framework)
	}
	root := string(framework)
	var written []string
	err := fs.WalkDir(files, root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel := strings.TrimPrefix(p, root+"/")
		content, err := fs.ReadFile(files, p)
		if err != nil {
			return err
		}
		rendered, err := renderFile(rel, content, data)
		if err != nil {
			return err
		}
		dest := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(dest, rendered, 0o644); err != nil {
			return err
		}
		written = append(written, rel)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("rendering the %s template: %w", framework, err)
	}

	// The deploy workflow is identical for every framework (it builds
	// whatever Dockerfile the template shipped and never inspects the
	// framework itself), so it lives once, here, rather than once per
	// framework directory under nextjs/vite-react/other.
	dest := filepath.Join(dir, filepath.FromSlash(DeployWorkflowPath))
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(dest, renderWorkflow(deployWorkflowSource, data), 0o644); err != nil {
		return nil, err
	}
	written = append(written, DeployWorkflowPath)

	if err := os.WriteFile(filepath.Join(dir, AppConfigPath), RenderAppConfig(data), 0o644); err != nil {
		return nil, err
	}
	written = append(written, AppConfigPath)

	sort.Strings(written)
	return written, nil
}

// RenderDockerfile writes only framework's Dockerfile and .dockerignore
// into dir (which must already exist) and returns their paths relative to
// dir, sorted. The Adopt path (internal/apprepo) uses this instead of
// Render when the target repository has no Dockerfile of its own: unlike
// Create, which generates a whole fresh repository, Adopt must not write
// the rest of the framework's template (package.json, source files,
// READMEs, .gitignore) into a repository that already has its own
// (docs/implementation-notes/15-cli-adopt-path.md).
func RenderDockerfile(framework Framework, data Data, dir string) ([]string, error) {
	if !framework.Valid() {
		return nil, fmt.Errorf("no built-in template for framework %q", framework)
	}
	root := string(framework)
	var written []string
	for _, rel := range []string{"Dockerfile", ".dockerignore"} {
		content, err := fs.ReadFile(files, root+"/"+rel)
		if err != nil {
			return nil, fmt.Errorf("reading the %s template's %s: %w", framework, rel, err)
		}
		rendered, err := renderFile(rel, content, data)
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(dir, rel), rendered, 0o644); err != nil {
			return nil, err
		}
		written = append(written, rel)
	}
	sort.Strings(written)
	return written, nil
}

// RenderDeployWorkflow returns the deploy workflow's rendered bytes for
// data, without writing anything. The Adopt path only calls this after
// checking that the target repository has no file at DeployWorkflowPath
// yet (a plain existence check, not a content comparison): an existing
// workflow there, identical or not, is left untouched rather than
// overwritten.
func RenderDeployWorkflow(data Data) []byte {
	return renderWorkflow(deployWorkflowSource, data)
}

func renderFile(name string, content []byte, data Data) ([]byte, error) {
	tmpl, err := template.New(name).Parse(string(content))
	if err != nil {
		return nil, fmt.Errorf("parsing %s: %w", name, err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("rendering %s: %w", name, err)
	}
	return buf.Bytes(), nil
}

// renderWorkflow substitutes the placeholder tokens deploy-workflow.yaml
// uses in place of Go template actions with data's values. Plain string
// substitution, not text/template, because the workflow is full of GitHub
// Actions' own ${{ ... }} expression syntax, which uses the same {{ }}
// delimiters text/template does.
func renderWorkflow(content []byte, data Data) []byte {
	r := strings.NewReplacer(
		"__IIDP_APP_NAME__", data.Name,
		"__IIDP_OWNER__", data.Owner,
		"__IIDP_VERSION__", data.IidpVersion,
		"__IIDP_CLI_REPO__", platform.CLIRepository,
		"__IIDP_DEPLOY_GATE_URL__", data.DeployGateURL,
	)
	return []byte(r.Replace(string(content)))
}
