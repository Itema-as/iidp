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
				Name:          "shop",
				Owner:         "itema-as",
				IidpVersion:   "0.3.1",
				DeployGateURL: "https://deploy.app.itma.no",
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

			// The Deploy gate, not an org secret: the workflow names the
			// gate and asks for an OIDC token in both jobs that deploy,
			// and holds no credential of its own
			// (docs/implementation-notes/60-deploy-gate.md).
			if got := lookup(t, doc, "env", "IIDP_DEPLOY_GATE_URL"); got != "https://deploy.app.itma.no" {
				t.Errorf("env.IIDP_DEPLOY_GATE_URL = %v, want the gate's URL", got)
			}
			for _, job := range []string{"build", "promote"} {
				if got := lookup(t, doc, "jobs", job, "permissions", "id-token"); got != "write" {
					t.Errorf("jobs.%s.permissions.id-token = %v, want write", job, got)
				}
			}
			for _, banned := range []string{"IIDP_DEPLOY_APP", "vars.", "secrets.IIDP"} {
				if strings.Contains(content, banned) {
					t.Errorf("deploy.yaml still uses %q; it must use no org secret or variable:\n%s", banned, content)
				}
			}
			if n := strings.Count(content, "secrets.GITHUB_TOKEN"); n != 4 {
				t.Errorf("deploy.yaml uses secrets.GITHUB_TOKEN %d times, want 4 (GHCR login and the iidp download, in each job)", n)
			}

			promote, ok := lookup(t, doc, "jobs", "promote").(map[string]any)
			if !ok {
				t.Fatalf("jobs.promote is missing or not a map")
			}
			steps, ok := promote["steps"].([]any)
			if !ok {
				t.Fatalf("jobs.promote.steps is missing or not a list")
			}
			for _, s := range steps {
				step, _ := s.(map[string]any)
				if uses, _ := step["uses"].(string); strings.HasPrefix(uses, "actions/checkout") {
					t.Errorf("jobs.promote has an actions/checkout step, which retagging via imagetools needs no repository files for:\n%s", content)
				}
			}
		})
	}
}

func TestDeployWorkflowIsIdenticalAcrossFrameworks(t *testing.T) {
	var rendered [][]byte
	for _, fw := range []templates.Framework{templates.NextJS, templates.ViteReact, templates.Other} {
		dir := t.TempDir()
		if _, err := templates.Render(fw, templates.Data{Name: "shop", Owner: "itema-as", IidpVersion: "0.3.1", DeployGateURL: "https://deploy.app.itma.no"}, dir); err != nil {
			t.Fatalf("Render(%s): %v", fw, err)
		}
		data, err := os.ReadFile(filepath.Join(dir, ".github", "workflows", "deploy.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		rendered = append(rendered, data)
	}
	for i := 1; i < len(rendered); i++ {
		if string(rendered[i]) != string(rendered[0]) {
			t.Errorf("deploy.yaml rendered for a different framework than the first differs; it should be framework-independent")
		}
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
