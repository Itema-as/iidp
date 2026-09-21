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
		return "web-service"
	case ViteReact:
		return "static-site"
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
