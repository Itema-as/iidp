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
	"regexp"
	"sort"
	"strings"
	"text/template"

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
	sort.Strings(written)
	return written, nil
}

// workflowPath matches a GitHub Actions workflow file's path within a
// template. Those files are full of the ${{ ... }} expression syntax
// GitHub Actions itself uses, which collides with text/template's own {{ }}
// delimiters, so they are rendered by plain string substitution
// (renderWorkflow) instead of being parsed as a Go template.
var workflowPath = regexp.MustCompile(`(^|/)\.github/workflows/[^/]+\.ya?ml$`)

func renderFile(name string, content []byte, data Data) ([]byte, error) {
	if workflowPath.MatchString(name) {
		return renderWorkflow(content, data), nil
	}
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

// renderWorkflow substitutes the placeholder tokens a workflow file uses
// in place of Go template actions (see workflowPath) with data's values.
func renderWorkflow(content []byte, data Data) []byte {
	r := strings.NewReplacer(
		"__IIDP_APP_NAME__", data.Name,
		"__IIDP_OWNER__", data.Owner,
		"__IIDP_VERSION__", data.IidpVersion,
		"__IIDP_CLI_REPO__", platform.CLIRepository,
	)
	return []byte(r.Replace(string(content)))
}
