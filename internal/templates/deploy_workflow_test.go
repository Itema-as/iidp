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

			// Both jobs run iidp ci set-image in a checkout of the commit
			// they deploy, since it reads iidp.yaml there: promote checks
			// out the tagged commit (the triggering ref, so no ref: of its
			// own) before it promotes
			// (docs/implementation-notes/66-migration-command-in-repo.md).
			for _, job := range []string{"build", "promote"} {
				steps, ok := lookup(t, doc, "jobs", job, "steps").([]any)
				if !ok {
					t.Fatalf("jobs.%s.steps is missing or not a list", job)
				}
				checkout, setImage := -1, -1
				for i, s := range steps {
					step, _ := s.(map[string]any)
					if uses, _ := step["uses"].(string); strings.HasPrefix(uses, "actions/checkout") {
						checkout = i
						if with, ok := step["with"].(map[string]any); ok && with["ref"] != nil {
							t.Errorf("jobs.%s checks out ref %v, want the triggering commit", job, with["ref"])
						}
					}
					if run, _ := step["run"].(string); strings.Contains(run, "iidp ci set-image") {
						setImage = i
					}
				}
				if checkout < 0 || setImage < 0 || checkout > setImage {
					t.Errorf("jobs.%s: checkout at step %d, iidp ci set-image at step %d; want a checkout before set-image", job, checkout, setImage)
				}
			}
		})
	}
}

func TestEveryTemplateGetsIidpYAML(t *testing.T) {
	for _, tc := range []struct {
		command string
		want    string
	}{
		{"npx prisma migrate deploy", "\nmigrationCommand: npx prisma migrate deploy\n"},
		{"", "\n# migrationCommand: npx prisma migrate deploy\n"},
	} {
		for _, fw := range []templates.Framework{templates.NextJS, templates.ViteReact, templates.Other} {
			dir := t.TempDir()
			files, err := templates.Render(fw, templates.Data{Name: "shop", Owner: "itema-as", IidpVersion: "0.3.1", DeployGateURL: "https://deploy.app.itma.no", MigrationCommand: tc.command}, dir)
			if err != nil {
				t.Fatalf("Render(%s): %v", fw, err)
			}
			if !contains(files, "iidp.yaml") {
				t.Fatalf("Render(%s) did not write iidp.yaml; files: %v", fw, files)
			}
			data, err := os.ReadFile(filepath.Join(dir, "iidp.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(data), tc.want) {
				t.Errorf("%s iidp.yaml for %q lacks %q:\n%s", fw, tc.command, tc.want, data)
			}
			for _, says := range []string{"before every rollout", "DATABASE_URL", "sh -c", "Postgres Capability", "Remove the line"} {
				if !strings.Contains(string(data), says) {
					t.Errorf("%s iidp.yaml's comment does not say %q:\n%s", fw, says, data)
				}
			}
		}
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
