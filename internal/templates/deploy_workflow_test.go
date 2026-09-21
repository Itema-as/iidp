package templates_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/Itema-as/iidp/internal/templates"
)

// lookup walks nested maps and lists by string keys and integer indexes,
// failing the test if the path does not exist.
func lookup(t *testing.T, doc any, path ...any) any {
	t.Helper()
	cur := doc
	for _, p := range path {
		switch key := p.(type) {
		case string:
			m, ok := cur.(map[string]any)
			if !ok {
				t.Fatalf("looking up %v: %q is not a map (%T)", path, key, cur)
			}
			cur, ok = m[key]
			if !ok {
				t.Fatalf("looking up %v: key %q missing", path, key)
			}
		case int:
			l, ok := cur.([]any)
			if !ok || key >= len(l) {
				t.Fatalf("looking up %v: index %d out of range (%T)", path, key, cur)
			}
			cur = l[key]
		}
	}
	return cur
}

func TestDeployWorkflowIsRenderedForEveryFramework(t *testing.T) {
	for _, fw := range []templates.Framework{templates.NextJS, templates.ViteReact, templates.Other} {
		t.Run(string(fw), func(t *testing.T) {
			dir := t.TempDir()
			files, err := templates.Render(fw, templates.Data{
				Name:        "shop",
				Owner:       "itema-as",
				IidpVersion: "0.3.1",
			}, dir)
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			if !contains(files, ".github/workflows/deploy.yaml") {
				t.Fatalf("Render did not write .github/workflows/deploy.yaml; files: %v", files)
			}

			data, err := os.ReadFile(filepath.Join(dir, ".github", "workflows", "deploy.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			content := string(data)

			var doc map[string]any
			if err := yaml.Unmarshal(data, &doc); err != nil {
				t.Fatalf("deploy.yaml is not valid YAML: %v\n%s", err, content)
			}

			if got := lookup(t, doc, "on", "push", "branches", 0); got != "main" {
				t.Errorf("on.push.branches[0] = %v, want main", got)
			}
			if got := lookup(t, doc, "on", "push", "tags", 0); got != "v*" {
				t.Errorf(`on.push.tags[0] = %v, want "v*"`, got)
			}

			if !strings.Contains(content, "ghcr.io/itema-as/shop") {
				t.Errorf("deploy.yaml lacks the image reference ghcr.io/itema-as/shop:\n%s", content)
			}
			if n := strings.Count(content, "iidp ci set-image shop auto"); n != 1 {
				t.Errorf("deploy.yaml has %d 'iidp ci set-image shop auto' invocations, want 1:\n%s", n, content)
			}
			if n := strings.Count(content, "iidp ci set-image shop prod"); n != 1 {
				t.Errorf("deploy.yaml has %d 'iidp ci set-image shop prod' invocations, want 1:\n%s", n, content)
			}
			if strings.Contains(content, "__IIDP_") {
				t.Errorf("a placeholder was left unsubstituted:\n%s", content)
			}
			if !strings.Contains(content, "Itema-as/iidp") {
				t.Errorf("deploy.yaml does not name the iidp release repository:\n%s", content)
			}
		})
	}
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
