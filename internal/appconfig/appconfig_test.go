package appconfig_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/Itema-as/iidp/internal/appconfig"
)

// Scheduled tasks in iidp.yaml (docs/implementation-notes/91-scheduled-tasks.md).
// The migration command's own parsing is covered through ci set-image
// (internal/cli/ci_set_image_test.go).

func TestParseReadsTasks(t *testing.T) {
	for _, tc := range []struct {
		name, file string
		want       []appconfig.Task
	}{
		{"no tasks key", "migrationCommand: npm run migrate\n", nil},
		{"null tasks", "tasks:\n", nil},
		{"an empty list", "tasks: []\n", nil},
		{"one task", "tasks:\n  - name: nightly-cleanup\n    schedule: \"0 3 * * *\"\n    command: node scripts/cleanup.js\n",
			[]appconfig.Task{{Name: "nightly-cleanup", Schedule: "0 3 * * *", Command: "node scripts/cleanup.js"}}},
		{"an unquoted schedule and a folded command", "tasks:\n  - name: report\n    schedule: 30 7 * * mon-fri\n    command: >\n      npm run report\n      && echo done\n",
			[]appconfig.Task{{Name: "report", Schedule: "30 7 * * mon-fri", Command: "npm run report && echo done"}}},
		{"every field form robfig/cron and Kubernetes accept", "tasks:\n  - {name: a, schedule: \"*/15 0-6,22,23 1-31/2 JAN-dec 0\", command: x}\n  - {name: b1, schedule: \"5 4 * * sun\", command: y}\n",
			[]appconfig.Task{{Name: "a", Schedule: "*/15 0-6,22,23 1-31/2 JAN-dec 0", Command: "x"}, {Name: "b1", Schedule: "5 4 * * sun", Command: "y"}}},
		{"the maximum count", "tasks:\n" + tasksYAML(5), fiveTasks()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, err := appconfig.Parse([]byte(tc.file))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if !slices.Equal(f.Tasks, tc.want) {
				t.Errorf("tasks = %+v, want %+v", f.Tasks, tc.want)
			}
		})
	}
}

// tasksYAML is n valid tasks as the list items of tasks:.
func tasksYAML(n int) string {
	var b strings.Builder
	for _, task := range fiveTasks()[:n] {
		b.WriteString("  - name: " + task.Name + "\n    schedule: \"" + task.Schedule + "\"\n    command: " + task.Command + "\n")
	}
	return b.String()
}

func fiveTasks() []appconfig.Task {
	var tasks []appconfig.Task
	for _, name := range []string{"one", "two", "three", "four", "five"} {
		tasks = append(tasks, appconfig.Task{Name: name, Schedule: "0 3 * * *", Command: "echo " + name})
	}
	return tasks
}

func TestParseRefusesBadTasks(t *testing.T) {
	task := func(name, schedule, command string) string {
		return "tasks:\n  - name: " + name + "\n    schedule: " + schedule + "\n    command: " + command + "\n"
	}
	for _, tc := range []struct {
		name, file, want string
	}{
		{"not a list", "tasks: nightly\n", "tasks must be a list"},
		{"a mapping", "tasks:\n  nightly: {schedule: \"0 3 * * *\"}\n", "tasks must be a list"},
		{"an item that is not a mapping", "tasks:\n  - nightly\n", "each task must be a mapping"},
		{"a missing name", "tasks:\n  - schedule: \"0 3 * * *\"\n    command: x\n", "the task has no name"},
		{"a missing schedule", "tasks:\n  - name: a\n    command: x\n", "the task has no schedule"},
		{"a missing command", "tasks:\n  - name: a\n    schedule: \"0 3 * * *\"\n", "the task has no command"},
		{"an unknown task setting", task("a", `"0 3 * * *"`, "x") + "    timeZone: UTC\n", `unknown task setting "timeZone"`},
		{"a setting given twice", task("a", `"0 3 * * *"`, "x") + "    command: y\n", "the task's command is set twice"},
		{"a name YAML reads as a number", task("2024", `"0 3 * * *"`, "x"), "a task's name must be a string"},
		{"a command that is a list", task("a", `"0 3 * * *"`, "[a, b]"), "a task's command must be a string"},
		{"an uppercase name", task("Nightly", `"0 3 * * *"`, "x"), `task name "Nightly" must be lowercase letters`},
		{"a name with an underscore", task("nightly_cleanup", `"0 3 * * *"`, "x"), "must be lowercase letters"},
		{"a name starting with a digit", task("1st", `"0 3 * * *"`, "x"), "start with a letter"},
		{"a name ending with a dash", task("a-", `"0 3 * * *"`, "x"), "not end with a dash"},
		{"an empty name", task(`""`, `"0 3 * * *"`, "x"), "must be lowercase letters"},
		{"a name longer than any CronJob", task(strings.Repeat("a", 53), `"0 3 * * *"`, "x"), "is 53 characters"},
		{"the same name twice", "tasks:\n" + tasksYAML(1) + tasksYAML(1), `task name "one" is used twice`},
		{"more than the maximum", "tasks:\n" + tasksYAML(5) + "  - {name: six, schedule: \"0 3 * * *\", command: x}\n", "6 tasks are declared, more than the 5 allowed"},
		{"an empty schedule", task("a", `""`, "x"), "the schedule is empty"},
		{"six fields", task("a", `"0 0 3 * * *"`, "x"), "has 6 fields"},
		{"four fields", task("a", `"0 3 * *"`, "x"), "has 4 fields"},
		{"a macro", task("a", "'@daily'", "x"), "is a macro"},
		{"a time zone", task("a", `"CRON_TZ=UTC 0 3 * *"`, "x"), "names a time zone"},
		{"a TZ prefix", task("a", `"TZ=UTC 0 3 * * *"`, "x"), "names a time zone"},
		{"a minute out of range", task("a", `"60 3 * * *"`, "x"), "is not a valid cron expression"},
		{"a day of week out of range", task("a", `"0 3 * * 7"`, "x"), "is not a valid cron expression"},
		{"a backwards range", task("a", `"0 5-3 * * *"`, "x"), "is not a valid cron expression"},
		{"a zero step", task("a", `"*/0 * * * *"`, "x"), "is not a valid cron expression"},
		{"a word", task("a", `"0 3 * * everyday"`, "x"), "is not a valid cron expression"},
		{"an unquoted schedule starting with *", task("a", "*/5 * * * *", "x"), `quote a schedule that starts with *`},
		{"an empty command", task("a", `"0 3 * * *"`, `""`), `task "a" has no command`},
		{"a command on two lines", "tasks:\n  - name: a\n    schedule: \"0 3 * * *\"\n    command: |\n      npm run a\n      npm run b\n", `task "a": the command must be one line`},
		{"a control character", task("a", `"0 3 * * *"`, `"echo \a"`), "control character"},
		{"a command over the limit", task("a", `"0 3 * * *"`, strings.Repeat("x", appconfig.MaxTaskCommandBytes+1)), "1025 bytes, more than the 1024 allowed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := appconfig.Parse([]byte(tc.file))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Parse(%q) error = %v, want one containing %q", tc.file, err, tc.want)
			}
		})
	}
}

func TestACommandAtTheLimitIsAccepted(t *testing.T) {
	command := strings.Repeat("x", appconfig.MaxTaskCommandBytes)
	if err := appconfig.ValidateTasks([]appconfig.Task{{Name: "a", Schedule: "0 3 * * *", Command: command}}); err != nil {
		t.Fatalf("ValidateTasks with a %d-byte command: %v", len(command), err)
	}
}

func TestTaskNamesMustFitTheApplicationsCronJobNames(t *testing.T) {
	// <application>-staging-<task> is at most 52 characters.
	for _, tc := range []struct {
		application, task string
		ok                bool
	}{
		{"shop", strings.Repeat("a", 52-len("shop-staging-")), true},
		{"shop", strings.Repeat("a", 52-len("shop-staging-")+1), false},
		{"hello", "nightly-cleanup", true},
		{strings.Repeat("s", 42), "a", true},
		{strings.Repeat("s", 43), "a", false},
	} {
		err := appconfig.CheckTaskNamesFit(tc.application, []appconfig.Task{{Name: tc.task, Schedule: "0 3 * * *", Command: "x"}})
		if (err == nil) != tc.ok {
			t.Errorf("CheckTaskNamesFit(%q, task %q) = %v, want ok = %v", tc.application, tc.task, err, tc.ok)
		}
		if err != nil && !strings.Contains(err.Error(), "52 characters") {
			t.Errorf("the refusal does not give the limit: %v", err)
		}
	}
}

func TestRenderedExampleTaskParses(t *testing.T) {
	// The commented-out example in the file iidp app create writes must
	// be a valid task once a developer uncomments it.
	rendered := string(appconfig.Render(""))
	_, example, found := strings.Cut(rendered, "\n#\n# tasks:\n")
	if !found {
		t.Fatalf("Render has no commented-out tasks example:\n%s", rendered)
	}
	uncommented := "tasks:\n"
	for line := range strings.Lines(example) {
		uncommented += strings.TrimPrefix(line, "# ")
	}
	f, err := appconfig.Parse([]byte(uncommented))
	if err != nil {
		t.Fatalf("the uncommented example does not parse: %v\n%s", err, uncommented)
	}
	if want := []appconfig.Task{{Name: "nightly-cleanup", Schedule: "0 3 * * *", Command: "node scripts/cleanup.js"}}; !slices.Equal(f.Tasks, want) {
		t.Errorf("the example's tasks = %+v, want %+v", f.Tasks, want)
	}
	// And the file as rendered declares none.
	f, err = appconfig.Parse([]byte(rendered))
	if err != nil || f.Tasks != nil {
		t.Errorf("Parse(Render) = %+v, %v; want no tasks", f, err)
	}
	for _, says := range []string{"Europe/Oslo", "for a Web service only", "sh -c", "stopped after an", "At most 5 tasks", "the next deploy stops it"} {
		if !strings.Contains(rendered, says) {
			t.Errorf("the tasks comment does not say %q:\n%s", says, rendered)
		}
	}
}
