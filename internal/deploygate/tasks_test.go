package deploygate_test

import (
	"fmt"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/Itema-as/iidp/internal/appconfig"
	"github.com/Itema-as/iidp/internal/deploygate"
)

// A deploy may carry the Scheduled tasks of the Application repository's
// iidp.yaml, which the gate writes with the tag and which replace the
// Environment's own (docs/implementation-notes/91-scheduled-tasks.md). The
// same HTTP boundary, fakes and bare Platform repository as gate_test.go.

// addKindApplication seeds shop like addApplication, bound to its
// repository, with kind as its Kind and tasks already in every
// Environment's values.yaml.
func addKindApplication(t *testing.T, url string, staging bool, kind string, tasks []appconfig.Task) {
	t.Helper()
	dir := cloneMain(t, url)
	envs := []string{"prod"}
	if staging {
		envs = append(envs, "staging")
	}
	existing := ""
	if len(tasks) > 0 {
		data, err := yaml.Marshal(map[string]any{"tasks": tasks})
		if err != nil {
			t.Fatal(err)
		}
		existing = string(data)
	}
	for _, environment := range envs {
		base := filepath.Join(dir, "applications", "shop", environment)
		writeFile(t, filepath.Join(base, "application.yaml"), "apiVersion: argoproj.io/v1alpha1\nkind: Application\n")
		writeFile(t, filepath.Join(base, "values.yaml"), fmt.Sprintf("# shop's %s values\napplication:\n  name: shop\nenvironment: %s\nkind: %s\nimage:\n  repository: ghcr.io/itema-as/shop\n  tag: \"\"\nsize: small\n%s", environment, environment, kind, existing))
	}
	writeFile(t, filepath.Join(dir, "applications", "shop", "repository.yaml"), binding(shopRepoID, orgID))
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "--amend", "--no-edit", "-m", "Seed the Platform repository")
	gitRun(t, dir, "push", "--force", "origin", "HEAD:main")
}

// deployWithTasks calls with a token carrying claims and the given tasks.
func (e *env) deployWithTasks(claims map[string]any, environment, tag string, tasks []appconfig.Task) (int, map[string]any) {
	e.t.Helper()
	return e.call(e.issuer.sign(e.t, claims), deploygate.Request{Application: "shop", Environment: environment, Tag: tag, Tasks: tasks})
}

// storedTasks is the tasks list of one of shop's Environments on main,
// and whether values.yaml has the key at all.
func storedTasks(t *testing.T, url, environment string) ([]appconfig.Task, bool) {
	t.Helper()
	var values map[string]any
	data := readFile(t, filepath.Join(cloneMain(t, url), "applications/shop", environment, "values.yaml"))
	if err := yaml.Unmarshal([]byte(data), &values); err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Tasks []appconfig.Task `yaml:"tasks"`
	}
	if err := yaml.Unmarshal([]byte(data), &doc); err != nil {
		t.Fatal(err)
	}
	_, present := values["tasks"]
	return doc.Tasks, present
}

var nightly = []appconfig.Task{
	{Name: "nightly-cleanup", Schedule: "0 3 * * *", Command: "node scripts/cleanup.js"},
	{Name: "report", Schedule: "*/15 7-17 * * mon-fri", Command: `npm run report && echo "sent: yes"`},
}

func TestTheGateWritesTheTasksWithTheTagInOneCommit(t *testing.T) {
	e := newEnv(t)
	addKindApplication(t, e.platform, false, "web-service", nil)

	status, body := e.deployWithTasks(e.issuer.claims(), "auto", shaDeployed, nightly)
	if status != http.StatusOK || body["tasksChanged"] != true {
		t.Fatalf("status = %d, body = %v, want 200 and the tasks changed", status, body)
	}

	clone := cloneMain(t, e.platform)
	if got := strings.TrimSpace(gitRun(t, clone, "rev-list", "--count", "HEAD")); got != "2" {
		t.Errorf("commits = %s, want the seed and one deploy", got)
	}
	if got := strings.TrimSpace(gitRun(t, clone, "show", "--name-only", "--format=", "HEAD")); got != "applications/shop/prod/values.yaml" {
		t.Errorf("the deploy changed %q, want only prod's values.yaml", got)
	}
	wantMessage := "Deploy shop prod " + shaDeployed + "\n\nScheduled tasks, from iidp.yaml:\n" +
		"- nightly-cleanup (0 3 * * *): node scripts/cleanup.js\n" +
		"- report (*/15 7-17 * * mon-fri): npm run report && echo \"sent: yes\"\n\n"
	if got := gitRun(t, clone, "log", "-1", "--format=%B"); !strings.HasPrefix(got, wantMessage) {
		t.Errorf("message = %q, want it to start %q", got, wantMessage)
	}
	values := readFile(t, filepath.Join(clone, "applications/shop/prod/values.yaml"))
	if !strings.Contains(values, "tag: "+shaDeployed) || !strings.HasPrefix(values, "# shop's prod values") {
		t.Errorf("values.yaml lost the tag or the rest of the file:\n%s", values)
	}
	if got, _ := storedTasks(t, e.platform, "prod"); !slices.Equal(got, nightly) {
		t.Errorf("tasks = %+v, want %+v", got, nightly)
	}

	// The same tag with the same tasks commits nothing; the same tag with
	// a changed task (a re-run after editing iidp.yaml) commits it.
	if status, body := e.deployWithTasks(e.issuer.claims(), "auto", shaDeployed, nightly); status != http.StatusOK || body["unchanged"] != true {
		t.Errorf("a repeat = %d %v, want 200 and unchanged", status, body)
	}
	moved := slices.Clone(nightly)
	moved[0].Schedule = "30 2 * * *"
	if status, body := e.deployWithTasks(e.issuer.claims(), "auto", shaDeployed, moved); status != http.StatusOK || body["unchanged"] == true || body["tasksChanged"] != true {
		t.Errorf("a changed schedule on the same tag = %d %v, want a commit", status, body)
	}
	if got, _ := storedTasks(t, e.platform, "prod"); !slices.Equal(got, moved) {
		t.Errorf("tasks = %+v, want %+v", got, moved)
	}
}

// Tasks only come from iidp.yaml, so a deploy without any removes those
// the Environment has: deleting a task from iidp.yaml stops it.
func TestTheGateRemovesTheTasksWhenADeployCarriesNone(t *testing.T) {
	for name, none := range map[string][]appconfig.Task{"no tasks key": nil, "an empty list": {}} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			addKindApplication(t, e.platform, false, "web-service", nightly)

			status, body := e.deployWithTasks(e.issuer.claims(), "auto", "sha1", none)
			if status != http.StatusOK || body["tasksChanged"] != true {
				t.Fatalf("status = %d, body = %v, want the tasks removed", status, body)
			}
			if got, present := storedTasks(t, e.platform, "prod"); present || got != nil {
				t.Errorf("tasks = %+v (key present: %v), want the key gone", got, present)
			}
			if got := gitRun(t, cloneMain(t, e.platform), "log", "-1", "--format=%b"); !strings.Contains(got, "Remove the Scheduled tasks: iidp.yaml declares none.") {
				t.Errorf("body = %q, want it to say the tasks were removed", got)
			}

			// And a deploy without tasks where there are none changes
			// nothing but the tag, and says nothing about tasks.
			if status, body := e.deployWithTasks(e.issuer.claims(), "auto", "sha2", nil); status != http.StatusOK || body["tasksChanged"] == true {
				t.Errorf("no tasks again = %d %v, want the tag deployed and nothing else", status, body)
			}
			if got := gitRun(t, cloneMain(t, e.platform), "log", "-1", "--format=%b"); strings.Contains(got, "Scheduled tasks") {
				t.Errorf("body = %q, want nothing about tasks", got)
			}
		})
	}
}

func TestTheGateRefusesTasksOnAStaticSite(t *testing.T) {
	e := newEnv(t)
	addKindApplication(t, e.platform, false, "static-site", nil)

	status, body := e.deployWithTasks(e.issuer.claims(), "auto", "sha1", nightly)
	e.refused(status, body, http.StatusConflict, "shop's prod Environment is a Static site", "Remove tasks from iidp.yaml")

	// Without tasks it deploys as always.
	if status, body := e.deployWithTasks(e.issuer.claims(), "auto", "sha1", nil); status != http.StatusOK || body["tasksChanged"] == true {
		t.Errorf("a Static site without tasks = %d %v, want the tag deployed and nothing else", status, body)
	}
}

// Tasks reach only the Environment the caller may deploy: main sets
// staging's, and prod keeps its own until a v* tag promotes the commit,
// which brings that commit's tasks (the promote job checks it out).
func TestTheGateCarriesTheTasksWithAPromotion(t *testing.T) {
	e := newEnv(t)
	old := []appconfig.Task{{Name: "old", Schedule: "0 1 * * *", Command: "echo old"}}
	addKindApplication(t, e.platform, true, "web-service", old)

	if status, body := e.deployWithTasks(e.issuer.claims(), "auto", "sha1", nightly); status != http.StatusOK || body["environment"] != "staging" {
		t.Fatalf("status = %d, body = %v, want staging written", status, body)
	}
	if got, _ := storedTasks(t, e.platform, "staging"); !slices.Equal(got, nightly) {
		t.Errorf("staging's tasks = %+v, want %+v", got, nightly)
	}
	if got, _ := storedTasks(t, e.platform, "prod"); !slices.Equal(got, old) {
		t.Errorf("prod's tasks = %+v, want them untouched by a main deploy", got)
	}

	claims := e.issuer.claims()
	claims["ref"], claims["ref_type"] = "refs/tags/v1.0.0", "tag"
	status, body := e.deployWithTasks(claims, "prod", "1.0.0", nightly)
	if status != http.StatusOK || body["environment"] != "prod" || body["tasksChanged"] != true {
		t.Fatalf("promote: status = %d, body = %v", status, body)
	}
	if got, _ := storedTasks(t, e.platform, "prod"); !slices.Equal(got, nightly) {
		t.Errorf("prod's tasks = %+v, want the promoted commit's", got)
	}

	// Promoting an older tag whose iidp.yaml had no tasks removes them.
	claims["ref"] = "refs/tags/v0.9.0"
	if status, body := e.deployWithTasks(claims, "prod", "0.9.0", nil); status != http.StatusOK || body["tasksChanged"] != true {
		t.Fatalf("promoting an older tag = %d %v, want the tasks removed", status, body)
	}
	if got, present := storedTasks(t, e.platform, "prod"); present {
		t.Errorf("prod's tasks = %+v, want none after promoting a commit without any", got)
	}
}

func TestTheGateRefusesTasksItCannotRun(t *testing.T) {
	e := newEnv(t)
	addKindApplication(t, e.platform, false, "web-service", nil)

	task := func(name, schedule, command string) []appconfig.Task {
		return []appconfig.Task{{Name: name, Schedule: schedule, Command: command}}
	}
	six := make([]appconfig.Task, 6)
	for i := range six {
		six[i] = appconfig.Task{Name: fmt.Sprintf("t%d", i), Schedule: "0 3 * * *", Command: "x"}
	}
	for name, tc := range map[string]struct {
		tasks []appconfig.Task
		want  string
	}{
		"too many":             {six, "6 tasks are declared, more than the 5 allowed"},
		"a bad name":           {task("Nightly", "0 3 * * *", "x"), "must be lowercase letters"},
		"a name used twice":    {append(task("a", "0 3 * * *", "x"), task("a", "0 4 * * *", "y")...), "used twice"},
		"a name too long":      {task(strings.Repeat("a", 40), "0 3 * * *", "x"), "shop's may be at most 39"},
		"a macro":              {task("a", "@hourly", "x"), "is a macro"},
		"a time zone":          {task("a", "TZ=UTC 0 3 * * *", "x"), "names a time zone"},
		"an invalid schedule":  {task("a", "0 25 * * *", "x"), "not a valid cron expression"},
		"no command":           {task("a", "0 3 * * *", ""), "has no command"},
		"a two-line command":   {task("a", "0 3 * * *", "a\nb"), "must be one line"},
		"a command over limit": {task("a", "0 3 * * *", strings.Repeat("x", 1025)), "1025 bytes"},
	} {
		t.Run(name, func(t *testing.T) {
			status, body := e.deployWithTasks(e.issuer.claims(), "auto", "sha1", tc.tasks)
			e.refused(status, body, http.StatusBadRequest, tc.want)
		})
	}
	// The longest name that fits shop-staging-<task> in 52 characters.
	if status, body := e.deployWithTasks(e.issuer.claims(), "auto", "sha1", task(strings.Repeat("a", 39), "0 3 * * *", "x")); status != http.StatusOK {
		t.Errorf("a 39-character task name = %d %v, want 200", status, body)
	}
}

// Tasks get no further than the tag would: a caller the gate refuses
// changes nothing, whatever it sends.
func TestTheGateChecksTheCallerBeforeTheTasks(t *testing.T) {
	e := newEnv(t)
	addKindApplication(t, e.platform, false, "web-service", nil)
	claims := e.issuer.claims()
	claims["repository"], claims["repository_id"] = "Itema-as/other", "999"

	status, body := e.deployWithTasks(claims, "auto", "sha1", nightly)
	e.refused(status, body, http.StatusForbidden, "repository id 999")
}
