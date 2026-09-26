package cli_test

import (
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// ci set-image sends the Scheduled tasks of iidp.yaml with the tag, and
// none (the key left out) without them, which the gate takes as "remove
// them" (docs/implementation-notes/91-scheduled-tasks.md). The fakes are
// ci_set_image_test.go's.

func TestCISetImageSendsTheTasksFromIidpYAML(t *testing.T) {
	for _, tc := range []struct {
		name string
		file string // "" means no iidp.yaml at all
		want []any
		says []string
	}{
		{"two tasks", "tasks:\n  - name: nightly-cleanup\n    schedule: \"0 3 * * *\"\n    command: node scripts/cleanup.js\n  - name: report\n    schedule: 30 7 * * mon-fri\n    command: npm run report\n",
			[]any{
				map[string]any{"name": "nightly-cleanup", "schedule": "0 3 * * *", "command": "node scripts/cleanup.js"},
				map[string]any{"name": "report", "schedule": "30 7 * * mon-fri", "command": "npm run report"},
			},
			[]string{"Scheduled tasks from iidp.yaml, on Europe/Oslo time:\n", "  nightly-cleanup (0 3 * * *): node scripts/cleanup.js\n", "  report (30 7 * * mon-fri): npm run report\n"}},
		{"no tasks", "migrationCommand: npm run migrate\n", nil, []string{"No Scheduled tasks in iidp.yaml."}},
		{"an empty list", "tasks: []\n", nil, []string{"No Scheduled tasks in iidp.yaml."}},
		{"no iidp.yaml", "", nil, []string{"No Scheduled tasks in iidp.yaml."}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			newFakeActionsTokenService(t)
			gate := newFakeGate(t, gateResponse{http.StatusOK, deployed})
			t.Setenv("IIDP_DEPLOY_GATE_URL", gate.srv.URL)
			dir := t.TempDir()
			if tc.file != "" {
				if err := os.WriteFile(filepath.Join(dir, "iidp.yaml"), []byte(tc.file), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			t.Chdir(dir)

			stdout, stderr, code := setImage(t, "shop", "auto", "abc123")
			if code != 0 || gate.callCount() != 1 {
				t.Fatalf("exit code = %d, gate calls = %d\n%s", code, gate.callCount(), stderr)
			}
			sent, present := gate.calls[0].body["tasks"]
			if tc.want == nil {
				if present {
					t.Errorf("tasks sent = %v, want the key left out", sent)
				}
			} else if !reflect.DeepEqual(sent, tc.want) {
				t.Errorf("tasks sent = %v, want %v", sent, tc.want)
			}
			for _, says := range tc.says {
				if !strings.Contains(stdout, says) {
					t.Errorf("stdout = %q, want %q", stdout, says)
				}
			}
		})
	}
}

func TestCISetImageRefusesBadTasksBeforeAnyCall(t *testing.T) {
	for _, tc := range []struct {
		name, content, want string
	}{
		{"a bad schedule", "tasks:\n  - {name: a, schedule: \"0 3 * *\", command: x}\n", `task "a": the schedule "0 3 * *" has 4 fields`},
		{"an unquoted schedule starting with *", "tasks:\n  - name: a\n    schedule: */5 * * * *\n    command: x\n", "quote a schedule that starts with *"},
		{"an unknown task setting", "tasks:\n  - {name: a, schedule: \"0 3 * * *\", command: x, timeZone: UTC}\n", `unknown task setting "timeZone"`},
		{"a name too long for the Application's CronJobs", "tasks:\n  - {name: " + strings.Repeat("a", 40) + ", schedule: \"0 3 * * *\", command: x}\n", "shop's may be at most 39"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tokens := newFakeActionsTokenService(t)
			gate := newFakeGate(t, gateResponse{http.StatusOK, deployed})
			t.Setenv("IIDP_DEPLOY_GATE_URL", gate.srv.URL)
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "iidp.yaml"), []byte(tc.content), 0o644); err != nil {
				t.Fatal(err)
			}
			t.Chdir(dir)

			_, stderr, code := setImage(t, "shop", "auto", "abc123")
			if code == 0 || !strings.Contains(stderr, tc.want) {
				t.Errorf("exit code = %d, stderr = %q, want %q", code, stderr, tc.want)
			}
			if len(tokens.audiences) != 0 || gate.callCount() != 0 {
				t.Errorf("made %d token requests and %d gate calls, want none", len(tokens.audiences), gate.callCount())
			}
		})
	}
}

func TestCISetImageSaysWhenTheTasksChanged(t *testing.T) {
	newFakeActionsTokenService(t)
	gate := newFakeGate(t, gateResponse{http.StatusOK, `{"application":"shop","environment":"staging","tag":"abc123","file":"applications/shop/staging/values.yaml","commit":"0123456789abcdef0123","tasksChanged":true}`})
	t.Setenv("IIDP_DEPLOY_GATE_URL", gate.srv.URL)
	t.Chdir(t.TempDir())

	stdout, stderr, code := setImage(t, "shop", "auto", "abc123")
	if code != 0 || !strings.Contains(stdout, "The Scheduled tasks changed with it") {
		t.Errorf("exit code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
}
