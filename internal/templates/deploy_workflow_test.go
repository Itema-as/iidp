package templates_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/Itema-as/iidp/internal/platform"
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

// workflowRef is what apprepo.DeployWorkflowRef returns for a v0 release.
const workflowRef = platform.DeployWorkflow + "@v0"

// reusableWorkflowPath is the reusable workflow in this repository that
// every rendered caller names (platform.DeployWorkflow).
const reusableWorkflowPath = "../../.github/workflows/application-deploy.yaml"

func renderCaller(t *testing.T, fw templates.Framework) (map[string]any, string) {
	t.Helper()
	dir := t.TempDir()
	files, err := templates.Render(fw, templates.Data{
		Name:              "shop",
		DeployWorkflowRef: workflowRef,
		DeployGateURL:     "https://deploy.app.itma.no",
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
	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("deploy.yaml is not valid YAML: %v\n%s", err, data)
	}
	return doc, string(data)
}

func readReusableWorkflow(t *testing.T) (map[string]any, string) {
	t.Helper()
	data, err := os.ReadFile(reusableWorkflowPath)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("%s is not valid YAML: %v", reusableWorkflowPath, err)
	}
	return doc, string(data)
}

// The rendered deploy.yaml is only a caller: the triggers, the
// permissions the reusable workflow needs, and its inputs
// (docs/implementation-notes/74-reusable-deploy-workflow.md).
func TestDeployWorkflowIsAShortCaller(t *testing.T) {
	for _, fw := range []templates.Framework{templates.NextJS, templates.ViteReact, templates.Other} {
		t.Run(string(fw), func(t *testing.T) {
			doc, content := renderCaller(t, fw)
			if got := lookup(t, doc, "on", "push", "branches", 0); got != "main" {
				t.Errorf("on.push.branches[0] = %v, want main", got)
			}
			if got := lookup(t, doc, "on", "push", "tags", 0); got != "v*" {
				t.Errorf(`on.push.tags[0] = %v, want "v*"`, got)
			}
			jobs := lookup(t, doc, "jobs").(map[string]any)
			if len(jobs) != 1 {
				t.Fatalf("jobs = %v, want exactly one job calling the reusable workflow", jobs)
			}
			if got := lookup(t, doc, "jobs", "deploy", "uses"); got != workflowRef {
				t.Errorf("jobs.deploy.uses = %v, want %s", got, workflowRef)
			}
			if _, hasSteps := jobs["deploy"].(map[string]any)["steps"]; hasSteps {
				t.Error("jobs.deploy has steps; a job that calls a reusable workflow can't")
			}
			if got := lookup(t, doc, "jobs", "deploy", "with", "application"); got != "shop" {
				t.Errorf("jobs.deploy.with.application = %v, want shop", got)
			}
			if got := lookup(t, doc, "jobs", "deploy", "with", "deploy-gate-url"); got != "https://deploy.app.itma.no" {
				t.Errorf("jobs.deploy.with.deploy-gate-url = %v, want the gate's URL", got)
			}
			if strings.Contains(content, "__IIDP_") {
				t.Errorf("a placeholder was left unsubstituted:\n%s", content)
			}
			for _, banned := range []string{"secrets", "vars.", "IIDP_DEPLOY_APP"} {
				if strings.Contains(content, banned) {
					t.Errorf("deploy.yaml mentions %q; it must hold and pass no secret or variable:\n%s", banned, content)
				}
			}
		})
	}
}

// The rendered caller and the reusable workflow agree: every input the
// caller passes is declared, with a string type, every required input is
// passed, and the caller grants each permission the reusable workflow's
// jobs ask for, since a called workflow can only keep or reduce them.
func TestDeployWorkflowCallerMatchesTheReusableWorkflow(t *testing.T) {
	caller, _ := renderCaller(t, templates.NextJS)
	reusable, _ := readReusableWorkflow(t)

	if !strings.HasSuffix(reusableWorkflowPath, strings.TrimPrefix(platform.DeployWorkflow, platform.CLIRepository+"/")) {
		t.Fatalf("platform.DeployWorkflow %s is not %s", platform.DeployWorkflow, reusableWorkflowPath)
	}
	declared := lookup(t, reusable, "on", "workflow_call", "inputs").(map[string]any)
	passed := lookup(t, caller, "jobs", "deploy", "with").(map[string]any)
	for name := range passed {
		input, ok := declared[name].(map[string]any)
		if !ok {
			t.Errorf("the caller passes %q, which the reusable workflow does not declare", name)
			continue
		}
		if input["type"] != "string" {
			t.Errorf("input %q has type %v, want string", name, input["type"])
		}
	}
	for name, v := range declared {
		if input, _ := v.(map[string]any); input["required"] == true {
			if _, ok := passed[name]; !ok {
				t.Errorf("the reusable workflow requires input %q, which the caller doesn't pass", name)
			}
		}
	}

	granted := lookup(t, caller, "jobs", "deploy", "permissions").(map[string]any)
	rank := map[string]int{"none": 0, "read": 1, "write": 2}
	for jobName, job := range lookup(t, reusable, "jobs").(map[string]any) {
		perms, _ := job.(map[string]any)["permissions"].(map[string]any)
		if perms == nil {
			t.Errorf("reusable job %s declares no permissions; it would get the caller's defaults", jobName)
		}
		for scope, level := range perms {
			if rank[fmt.Sprint(granted[scope])] < rank[fmt.Sprint(level)] {
				t.Errorf("reusable job %s needs %s: %v, and the caller grants %v", jobName, scope, level, granted[scope])
			}
		}
	}
}

// The reusable workflow keeps the behaviour the full per-Application
// workflow had: build on main, promote on a v* tag, each in a checkout of
// the commit it deploys (iidp.yaml, #66), through the Deploy gate with an
// OIDC token and no stored secret, and it installs the iidp release its
// own commit belongs to.
func TestReusableDeployWorkflowJobs(t *testing.T) {
	doc, content := readReusableWorkflow(t)
	if got := lookup(t, doc, "env", "IIDP_DEPLOY_GATE_URL"); got != "${{ inputs.deploy-gate-url }}" {
		t.Errorf("env.IIDP_DEPLOY_GATE_URL = %v, want the deploy-gate-url input", got)
	}
	for job, want := range map[string]string{
		"build":   `iidp ci set-image "${APPLICATION}" auto "${GITHUB_SHA}"`,
		"promote": `iidp ci set-image "${APPLICATION}" prod "${GITHUB_REF_NAME#v}"`,
	} {
		if got := lookup(t, doc, "jobs", job, "permissions", "id-token"); got != "write" {
			t.Errorf("jobs.%s.permissions.id-token = %v, want write", job, got)
		}
		steps := lookup(t, doc, "jobs", job, "steps").([]any)
		checkout, install, setImage := -1, -1, -1
		for i, s := range steps {
			step, _ := s.(map[string]any)
			if uses, _ := step["uses"].(string); strings.HasPrefix(uses, "actions/checkout") {
				checkout = i
				if with, ok := step["with"].(map[string]any); ok && (with["ref"] != nil || with["repository"] != nil) {
					t.Errorf("jobs.%s checks out %v; want the caller's repository at the triggering commit", job, with)
				}
			}
			run, _ := step["run"].(string)
			if strings.Contains(run, "gh release download") {
				install = i
				env, _ := step["env"].(map[string]any)
				if env["WORKFLOW_SHA"] != "${{ job.workflow_sha }}" {
					t.Errorf("jobs.%s installs iidp without WORKFLOW_SHA: ${{ job.workflow_sha }}", job)
				}
			}
			if strings.Contains(run, want) {
				setImage = i
			}
		}
		if checkout < 0 || install < 0 || setImage < 0 || checkout > setImage || install > setImage {
			t.Errorf("jobs.%s: checkout at %d, install at %d, %q at %d; want checkout and install before it", job, checkout, install, want, setImage)
		}
	}
	if got := lookup(t, doc, "jobs", "build", "if"); got != "github.ref == 'refs/heads/main'" {
		t.Errorf("jobs.build.if = %v", got)
	}
	if got := lookup(t, doc, "jobs", "promote", "if"); got != "startsWith(github.ref, 'refs/tags/v')" {
		t.Errorf("jobs.promote.if = %v", got)
	}
	for _, banned := range []string{"IIDP_DEPLOY_APP", "vars.", "secrets.IIDP"} {
		if strings.Contains(content, banned) {
			t.Errorf("the reusable workflow uses %q; it must use no org secret or variable", banned)
		}
	}
	if a, b := installScript(t, doc, "build"), installScript(t, doc, "promote"); a != b {
		t.Errorf("the two jobs' install scripts differ; keep them identical")
	}
}

func installScript(t *testing.T, doc map[string]any, job string) string {
	t.Helper()
	for _, s := range lookup(t, doc, "jobs", job, "steps").([]any) {
		step, _ := s.(map[string]any)
		if run, _ := step["run"].(string); strings.Contains(run, "gh release download") {
			return run
		}
	}
	t.Fatalf("jobs.%s has no install step", job)
	return ""
}

// The install step picks the vX.Y.Z release whose commit the workflow ran
// at, never the moving major tag itself, whether the tags are annotated
// (peeled ^{} lines) or lightweight, and refuses a commit no release
// points at. git, gh, tar and sudo are stubs; the script is the workflow's.
func TestReusableDeployWorkflowInstallsItsOwnRelease(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not installed")
	}
	doc, _ := readReusableWorkflow(t)
	script := installScript(t, doc, "build")
	const lsRemote = `1111111111111111111111111111111111111111	refs/tags/v0
aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa	refs/tags/v0.2.1
2222222222222222222222222222222222222222	refs/tags/v0.2.2
cccccccccccccccccccccccccccccccccccccccc	refs/tags/v0.2.2^{}
3333333333333333333333333333333333333333	refs/tags/v0^{}
cccccccccccccccccccccccccccccccccccccccc	refs/tags/v0^{}
dddddddddddddddddddddddddddddddddddddddd	refs/tags/v0.3.0-rc1
`
	for _, tc := range []struct {
		name, sha, wantTag, wantErr string
	}{
		{"annotated release", strings.Repeat("c", 40), "v0.2.2", ""},
		{"lightweight release", strings.Repeat("a", 40), "v0.2.1", ""},
		{"pre-release only", strings.Repeat("d", 40), "", "no iidp release points at"},
		{"not a release", strings.Repeat("e", 40), "", "no iidp release points at"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin := t.TempDir()
			stub := func(name, body string) {
				if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/usr/bin/env bash\n"+body), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			stub("git", "cat <<'EOF'\n"+lsRemote+"EOF\n")
			stub("gh", `echo "gh $*" >> "$STUB_LOG"`+"\n")
			stub("tar", "exit 0\n")
			stub("sudo", "exit 0\n")
			stub("iidp", "echo iidp stub\n")
			log := filepath.Join(t.TempDir(), "log")
			cmd := exec.Command("bash", "-c", script)
			cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "WORKFLOW_SHA="+tc.sha, "STUB_LOG="+log, "TMPDIR="+t.TempDir())
			out, err := cmd.CombinedOutput()
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(string(out), tc.wantErr) {
					t.Fatalf("got %v, want a failure saying %q:\n%s", err, tc.wantErr, out)
				}
				return
			}
			if err != nil {
				t.Fatalf("install step: %v\n%s", err, out)
			}
			calls, _ := os.ReadFile(log)
			if want := "gh release download " + tc.wantTag + " --repo Itema-as/iidp"; !strings.Contains(string(calls), want) {
				t.Errorf("gh calls = %q, want %q", calls, want)
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
			files, err := templates.Render(fw, templates.Data{Name: "shop", DeployWorkflowRef: workflowRef, DeployGateURL: "https://deploy.app.itma.no", MigrationCommand: tc.command}, dir)
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
		if _, err := templates.Render(fw, templates.Data{Name: "shop", DeployWorkflowRef: workflowRef, DeployGateURL: "https://deploy.app.itma.no"}, dir); err != nil {
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
