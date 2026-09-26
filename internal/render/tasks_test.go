package render_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/Itema-as/iidp/internal/appconfig"
	"github.com/Itema-as/iidp/internal/render"
)

// SetTasks is the Deploy gate's edit for the Scheduled tasks a deploy
// carries (docs/implementation-notes/91-scheduled-tasks.md).

func tasksOf(t *testing.T, values []byte) []appconfig.Task {
	t.Helper()
	var doc struct {
		Tasks []appconfig.Task `yaml:"tasks"`
	}
	if err := yaml.Unmarshal(values, &doc); err != nil {
		t.Fatalf("parsing:\n%s\n%v", values, err)
	}
	return doc.Tasks
}

func TestSetTasksWritesChangesAndRemoves(t *testing.T) {
	tasks := []appconfig.Task{
		{Name: "nightly-cleanup", Schedule: "0 3 * * *", Command: "node scripts/cleanup.js"},
		{Name: "every-quarter", Schedule: "*/15 * * * *", Command: "true"},
	}
	out, changed, err := render.SetTasks([]byte(testValuesYAML), tasks)
	if err != nil || !changed {
		t.Fatalf("SetTasks: changed = %v, err = %v", changed, err)
	}
	if !strings.HasPrefix(string(out), "# Values for the prod Environment of shop.") || !strings.Contains(string(out), "repository: ghcr.io/itema-as/shop") {
		t.Errorf("the rest of the document was not kept:\n%s", out)
	}
	// Every value a string, even one YAML would read as a boolean.
	if got := tasksOf(t, out); !slices.Equal(got, tasks) {
		t.Errorf("tasks = %+v, want %+v\n%s", got, tasks, out)
	}

	same, changed, err := render.SetTasks(out, tasks)
	if err != nil || changed || string(same) != string(out) {
		t.Errorf("the same tasks again: changed = %v, err = %v, want an untouched document", changed, err)
	}

	fewer, changed, err := render.SetTasks(out, tasks[:1])
	if err != nil || !changed || !slices.Equal(tasksOf(t, fewer), tasks[:1]) {
		t.Errorf("one task fewer: changed = %v, err = %v, tasks = %+v", changed, err, tasksOf(t, fewer))
	}

	edited := slices.Clone(tasks[:1])
	edited[0].Schedule = "0 4 * * *"
	moved, changed, err := render.SetTasks(fewer, edited)
	if err != nil || !changed || !slices.Equal(tasksOf(t, moved), edited) {
		t.Errorf("a new schedule: changed = %v, err = %v, tasks = %+v", changed, err, tasksOf(t, moved))
	}

	for _, none := range [][]appconfig.Task{nil, {}} {
		removed, changed, err := render.SetTasks(moved, none)
		if err != nil || !changed {
			t.Fatalf("removing: changed = %v, err = %v", changed, err)
		}
		if strings.Contains(string(removed), "tasks") {
			t.Errorf("removing left a tasks key:\n%s", removed)
		}
	}
}

func TestSetTasksWithNoneChangesNothingWithoutTasks(t *testing.T) {
	out, changed, err := render.SetTasks([]byte(testValuesYAML), nil)
	if err != nil || changed || string(out) != testValuesYAML {
		t.Errorf("no tasks on values without any: changed = %v, err = %v, want an untouched document", changed, err)
	}
}

func TestSetTasksRefusesAStaticSite(t *testing.T) {
	static := strings.Replace(testValuesYAML, "kind: web-service", "kind: static-site", 1)
	if _, _, err := render.SetTasks([]byte(static), []appconfig.Task{{Name: "a", Schedule: "0 3 * * *", Command: "x"}}); !errors.Is(err, render.ErrTasksOnStaticSite) {
		t.Errorf("err = %v, want ErrTasksOnStaticSite", err)
	}
	// Removing what is not there is fine, and changes nothing.
	out, changed, err := render.SetTasks([]byte(static), nil)
	if err != nil || changed || string(out) != static {
		t.Errorf("no tasks on a Static site: changed = %v, err = %v, want an untouched document", changed, err)
	}
}

func TestCopyValuesForStagingDropsProdsTasks(t *testing.T) {
	withTasks, _, err := render.SetTasks([]byte(testValuesYAML), []appconfig.Task{{Name: "a", Schedule: "0 3 * * *", Command: "x"}})
	if err != nil {
		t.Fatal(err)
	}
	out, _, err := render.CopyValuesForStaging(withTasks)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "tasks") {
		t.Errorf("prod's tasks were copied to staging:\n%s", out)
	}
}
