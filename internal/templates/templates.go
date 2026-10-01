// Package templates holds the built-in Application repository templates
// Create renders: a minimal Next.js Web service, a minimal Vite React
// Static site, and a commented Dockerfile stub for a framework iidp does
// not generate.
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

// DeployWorkflowPath is where every rendered template gets the shared
// deploy workflow, relative to the repository root.
const DeployWorkflowPath = ".github/workflows/deploy.yaml"

//go:embed deploy-workflow.yaml
var deployWorkflowSource []byte

// Data is the Application-specific value the templates are rendered with.
type Data struct {
	Name string
	// DeployWorkflowRef is what .github/workflows/deploy.yaml calls, such
	// as Itema-as/iidp/.github/workflows/application-deploy.yaml@v0.
	DeployWorkflowRef string
	// DeployGateURL is the Platform's Deploy gate. CI can't read the
	// private Platform repository to find it, so it's rendered into the
	// caller, but only when it isn't the reusable workflow's default.
	DeployGateURL string
	// MigrationCommand is written into iidp.yaml; "" for none.
	MigrationCommand string
}

// AppConfigPath is where every rendered template gets iidp.yaml.
const AppConfigPath = appconfig.FileName

// RenderAppConfig returns iidp.yaml's rendered bytes for data.
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
// dir, sorted. Adopt uses it for a repository that has its own source but
// no Dockerfile.
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
// data, without writing anything.
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

// renderWorkflow substitutes deploy-workflow.yaml's placeholder tokens.
// It doesn't use text/template because GitHub Actions' ${{ ... }}
// expressions use the same delimiters.
func renderWorkflow(content []byte, data Data) []byte {
	r := strings.NewReplacer(
		"__IIDP_APP_NAME__", data.Name,
		"__IIDP_DEPLOY_WORKFLOW_REF__", data.DeployWorkflowRef,
		"__IIDP_DEPLOY_WORKFLOW__", platform.DeployWorkflow,
		"__IIDP_DEPLOY_GATE_URL_INPUT__\n", deployGateURLInput(data.DeployGateURL),
	)
	return []byte(r.Replace(string(content)))
}

// deployGateURLInput is the caller's deploy-gate-url line, with its
// comment, or nothing when gateURL is empty or the reusable workflow's
// default.
func deployGateURLInput(gateURL string) string {
	if gateURL == "" || gateURL == platform.DefaultDeployGateURL {
		return ""
	}
	return "      # The Platform's Deploy gate, https://deploy.<baseDomain>, written\n" +
		"      # here by iidp app create because it isn't the reusable workflow's\n" +
		"      # default (" + platform.DefaultDeployGateURL + ").\n" +
		"      deploy-gate-url: " + gateURL + "\n"
}
