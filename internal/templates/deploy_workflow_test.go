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
	"github.com/Itema-as/iidp/internal/render"
	"github.com/Itema-as/iidp/internal/templates"
)

// previewLabel is the pull request label that asks for a Preview
// Environment: the one the CLI's ApplicationSet filters on.
const previewLabel = render.PreviewLabel

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
	return renderCallerFor(t, fw, platform.DefaultDeployGateURL)
}

func renderCallerFor(t *testing.T, fw templates.Framework, gateURL string) (map[string]any, string) {
	t.Helper()
	dir := t.TempDir()
	files, err := templates.Render(fw, templates.Data{
		Name:              "shop",
		DeployWorkflowRef: workflowRef,
		DeployGateURL:     gateURL,
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
// permissions the reusable workflow needs, and its inputs.
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
			// The reusable workflow builds a labelled pull request's image,
			// so the caller passes the events that can change it or add the
			// label.
			if got := fmt.Sprint(lookup(t, doc, "on", "pull_request", "types")); got != "[opened synchronize reopened labeled]" {
				t.Errorf("on.pull_request.types = %v, want [opened synchronize reopened labeled]", got)
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
			// Itema's Platform is the reusable workflow's default, so its
			// callers don't repeat it.
			if with := lookup(t, doc, "jobs", "deploy", "with").(map[string]any); with["deploy-gate-url"] != nil {
				t.Errorf("jobs.deploy.with.deploy-gate-url = %v, want it left to the reusable workflow's default", with["deploy-gate-url"])
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

// A Platform whose gate isn't the reusable workflow's default gets it
// written into the caller, so a second Platform (the kind fixture's
// app.example.test, say) still deploys to its own gate.
func TestDeployWorkflowCallerNamesAGateOtherThanTheDefault(t *testing.T) {
	doc, content := renderCallerFor(t, templates.NextJS, "https://deploy.app.example.test")
	if got := lookup(t, doc, "jobs", "deploy", "with", "deploy-gate-url"); got != "https://deploy.app.example.test" {
		t.Errorf("jobs.deploy.with.deploy-gate-url = %v, want https://deploy.app.example.test\n%s", got, content)
	}
}

// The reusable workflow's default and platform.DefaultDeployGateURL, which
// decides when the CLI leaves the input out, are the same URL; otherwise a
// caller without the input would deploy to a gate the CLI didn't mean.
func TestReusableDeployWorkflowDefaultGateIsThePlatformDefault(t *testing.T) {
	doc, _ := readReusableWorkflow(t)
	input := lookup(t, doc, "on", "workflow_call", "inputs", "deploy-gate-url").(map[string]any)
	if input["default"] != platform.DefaultDeployGateURL {
		t.Errorf("deploy-gate-url default = %v, want platform.DefaultDeployGateURL %s", input["default"], platform.DefaultDeployGateURL)
	}
	if input["required"] != false {
		t.Errorf("deploy-gate-url required = %v, want false: callers on the default Platform leave it out", input["required"])
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

// The reusable workflow builds on main and promotes on a v* tag, each in a
// checkout of the commit it deploys (for iidp.yaml), through the Deploy
// gate with an OIDC token and no stored secret, and it installs the iidp
// release its own commit belongs to.
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

// pullRequestEvent is the github context of a pull_request event whose
// pull request carries labels; action and the label the event is about
// (for labeled) as GitHub sends them.
func pullRequestEvent(action, label string, labels ...string) map[string]any {
	var names []any
	for _, l := range labels {
		names = append(names, map[string]any{"name": l})
	}
	event := map[string]any{
		"action": action,
		"pull_request": map[string]any{
			"labels": names,
			"head":   map[string]any{"sha": "0123456789abcdef0123456789abcdef01234567"},
		},
	}
	if label != "" {
		event["label"] = map[string]any{"name": label}
	}
	return map[string]any{"github": map[string]any{
		"event_name": "pull_request",
		"ref":        "refs/pull/7/merge",
		"event":      event,
	}}
}

// Which jobs of the reusable workflow each event runs, from their if:
// conditions: main deploys, a v* tag promotes, and a pull request builds
// its preview image only while it has the preview label. Anything else
// runs nothing, so an ordinary pull request costs no Actions minutes.
func TestReusableDeployWorkflowRunsTheRightJobForEachEvent(t *testing.T) {
	doc, _ := readReusableWorkflow(t)
	jobs := lookup(t, doc, "jobs").(map[string]any)
	push := func(ref string) map[string]any {
		return map[string]any{"github": map[string]any{"event_name": "push", "ref": ref, "event": map[string]any{}}}
	}
	for _, tc := range []struct {
		name string
		ctx  map[string]any
		want string
	}{
		{"push to main", push("refs/heads/main"), "build"},
		{"v* tag", push("refs/tags/v1.2.3"), "promote"},
		{"push to another branch", push("refs/heads/feature"), ""},
		{"pull request opened with the label", pullRequestEvent("opened", "", "bug", "preview"), "preview"},
		{"pull request pushed to, labelled", pullRequestEvent("synchronize", "", "preview"), "preview"},
		{"pull request reopened, labelled", pullRequestEvent("reopened", "", "preview"), "preview"},
		{"the label added", pullRequestEvent("labeled", "preview", "preview"), "preview"},
		{"another label added to a labelled pull request", pullRequestEvent("labeled", "bug", "preview", "bug"), ""},
		{"pull request opened without the label", pullRequestEvent("opened", ""), ""},
		{"pull request pushed to, unlabelled", pullRequestEvent("synchronize", "", "bug"), ""},
		{"another label added to an unlabelled pull request", pullRequestEvent("labeled", "bug", "bug"), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var ran []string
			for name, job := range jobs {
				cond, _ := job.(map[string]any)["if"].(string)
				if cond == "" {
					t.Fatalf("job %s has no if:; every job must say which events it runs for", name)
				}
				if evalIf(t, cond, tc.ctx) {
					ran = append(ran, name)
				}
			}
			if got := strings.Join(ran, ","); got != tc.want {
				t.Errorf("jobs run = %q, want %q", got, tc.want)
			}
		})
	}
}

// The preview job builds the pull request's head and pushes it tagged
// with the head SHA, the tag the Preview Environment's ApplicationSet
// takes from the generator, and calls nothing on the Platform: no iidp,
// no OIDC token.
func TestReusableDeployWorkflowPreviewJobOnlyBuildsTheHead(t *testing.T) {
	doc, _ := readReusableWorkflow(t)
	job := lookup(t, doc, "jobs", "preview").(map[string]any)
	if perms := job["permissions"].(map[string]any); perms["id-token"] != nil {
		t.Errorf("jobs.preview.permissions = %v, want no id-token: it deploys nothing", perms)
	}
	if cond := job["if"].(string); !strings.Contains(cond, "'"+previewLabel+"'") {
		t.Errorf("jobs.preview.if = %q, want it to test the %s label", cond, previewLabel)
	}
	var checkedOut, pushed bool
	for _, s := range job["steps"].([]any) {
		step := s.(map[string]any)
		if run, _ := step["run"].(string); strings.Contains(run, "iidp") || strings.Contains(run, "curl") {
			t.Errorf("jobs.preview runs %q, want nothing that talks to the Platform", run)
		}
		uses, _ := step["uses"].(string)
		with, _ := step["with"].(map[string]any)
		switch {
		case strings.HasPrefix(uses, "actions/checkout"):
			checkedOut = with["ref"] == "${{ github.event.pull_request.head.sha }}"
		case strings.HasPrefix(uses, "docker/build-push-action"):
			pushed = with["push"] == true && with["tags"] == "${{ steps.image.outputs.name }}:${{ github.event.pull_request.head.sha }}"
		}
	}
	if !checkedOut {
		t.Error("jobs.preview does not check out the pull request's head SHA")
	}
	if !pushed {
		t.Error("jobs.preview does not push the image tagged with the head SHA")
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
