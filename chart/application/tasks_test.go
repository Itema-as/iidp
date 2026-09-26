package application_test

import (
	"maps"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// Scheduled tasks: one CronJob per entry of tasks, for a Web service
// (docs/implementation-notes/91-scheduled-tasks.md).

// taskContainer returns the single container of a CronJob's Pod template.
func taskContainer(t *testing.T, cronJob object) map[string]any {
	t.Helper()
	containers := get[[]any](t, cronJob, "spec", "jobTemplate", "spec", "template", "spec", "containers")
	if len(containers) != 1 {
		t.Fatalf("the CronJob's Pod has %d containers, want 1", len(containers))
	}
	return get[map[string]any](t, map[string]any{"c": containers[0]}, "c")
}

func TestEachTaskRendersACronJob(t *testing.T) {
	objects := render(t, "tasks-staging-postgres.yaml")
	if got, want := objectsOfKind(objects, "CronJob"), []string{"CronJob/shop-staging-nightly-cleanup", "CronJob/shop-staging-report"}; !slices.Equal(got, want) {
		t.Fatalf("rendered CronJobs %v, want %v", got, want)
	}

	for _, tc := range []struct{ task, schedule, command string }{
		{"nightly-cleanup", "0 3 * * *", `node scripts/cleanup.js && echo "cleaned: yes"`},
		{"report", "*/15 7-17 * * mon-fri", "npm run report"},
	} {
		t.Run(tc.task, func(t *testing.T) {
			cronJob := mustObject(t, objects, "CronJob/shop-staging-"+tc.task)
			spec := get[map[string]any](t, cronJob, "spec")
			for field, want := range map[string]any{
				"schedule":                   tc.schedule,
				"timeZone":                   "Europe/Oslo",
				"concurrencyPolicy":          "Forbid",
				"startingDeadlineSeconds":    300,
				"successfulJobsHistoryLimit": 1,
				"failedJobsHistoryLimit":     3,
			} {
				if spec[field] != want {
					t.Errorf("spec.%s = %v, want %v", field, spec[field], want)
				}
			}
			job := get[map[string]any](t, cronJob, "spec", "jobTemplate", "spec")
			if job["backoffLimit"] != 0 {
				t.Errorf("backoffLimit = %v, want 0: a failed run waits for the next time on the schedule", job["backoffLimit"])
			}
			if job["activeDeadlineSeconds"] != 3600 {
				t.Errorf("activeDeadlineSeconds = %v, want 3600, the one-hour run timeout", job["activeDeadlineSeconds"])
			}
			if policy := get[string](t, job, "template", "spec", "restartPolicy"); policy != "Never" {
				t.Errorf("restartPolicy = %q, want Never", policy)
			}

			c := taskContainer(t, cronJob)
			if image := get[string](t, c, "image"); image != "ghcr.io/itema-as/shop:0123abc" {
				t.Errorf("image = %q, want the Application's image", image)
			}
			if command := get[[]any](t, c, "command"); !slices.Equal(command, []any{"sh", "-c", tc.command}) {
				t.Errorf("command = %v, want sh -c %q", command, tc.command)
			}
			// The environment the Application runs with: its env, the
			// Postgres DATABASE_URL and its named Secrets.
			if nodeEnv := envItem(t, c, "NODE_ENV"); nodeEnv == nil || nodeEnv["value"] != "production" {
				t.Errorf("NODE_ENV = %v, want the Application's value", nodeEnv)
			}
			url := envItem(t, c, "DATABASE_URL")
			if url == nil || get[string](t, url, "valueFrom", "secretKeyRef", "name") != "shop-staging-db-app" || get[string](t, url, "valueFrom", "secretKeyRef", "key") != "uri" {
				t.Errorf("DATABASE_URL = %v, want the uri key of shop-staging-db-app", url)
			}
			if item := envItem(t, c, "PORT"); item != nil {
				t.Errorf("PORT = %v; a task listens on nothing", item)
			}
			var refs []string
			for _, item := range get[[]any](t, c, "envFrom") {
				refs = append(refs, get[string](t, item, "secretRef", "name"))
			}
			if !slices.Equal(refs, []string{"shop-stripe"}) {
				t.Errorf("envFrom secretRefs = %v, want the Application's secrets", refs)
			}
			// The smallest size, whatever the Application's (medium here),
			// as requests and limits.
			for _, field := range []string{"requests", "limits"} {
				if cpu := get[string](t, c, "resources", field, "cpu"); cpu != "250m" {
					t.Errorf("%s.cpu = %q, want 250m", field, cpu)
				}
				if memory := get[string](t, c, "resources", field, "memory"); memory != "256Mi" {
					t.Errorf("%s.memory = %q, want 256Mi", field, memory)
				}
			}
		})
	}
}

func TestATaskWithoutPostgresOrEnvGetsNeither(t *testing.T) {
	c := taskContainer(t, mustObject(t, render(t, "tasks-prod.yaml"), "CronJob/warehouse-sync"))
	for _, key := range []string{"env", "envFrom"} {
		if value, set := c[key]; set {
			t.Errorf("%s = %v, want none", key, value)
		}
	}
	if cpu := get[string](t, c, "resources", "limits", "cpu"); cpu != "250m" {
		t.Errorf("limits.cpu = %q, want the small size's 250m, not the Application's large", cpu)
	}
}

// Alloy attributes a run's logs by the Pod's labels, and iidp app status
// finds an Environment's runs by the Job's. The Pod must not carry the
// Service's selector labels, or the Service would route to it.
func TestTaskRunsAreAttributedAndNeverServed(t *testing.T) {
	objects := render(t, "tasks-staging-postgres.yaml")
	cronJob := mustObject(t, objects, "CronJob/shop-staging-report")
	want := map[string]any{
		"iidp.itema.no/application":   "shop",
		"iidp.itema.no/environment":   "staging",
		"iidp.itema.no/task":          "report",
		"app.kubernetes.io/component": "scheduled-task",
	}
	for where, labels := range map[string]map[string]any{
		"CronJob":      get[map[string]any](t, cronJob, "metadata", "labels"),
		"Job template": get[map[string]any](t, cronJob, "spec", "jobTemplate", "metadata", "labels"),
		"Pod template": get[map[string]any](t, cronJob, "spec", "jobTemplate", "spec", "template", "metadata", "labels"),
	} {
		for key, value := range want {
			if labels[key] != value {
				t.Errorf("%s: label %s = %v, want %v", where, key, labels[key], value)
			}
		}
	}
	selector := get[map[string]any](t, mustObject(t, objects, "Service/shop-staging"), "spec", "selector")
	if pod := get[map[string]any](t, cronJob, "spec", "jobTemplate", "spec", "template", "metadata", "labels"); matches(selector, pod) {
		t.Errorf("the task Pod's labels %v match the Service's selector %v", pod, selector)
	}
}

func TestRunTasksFalseRendersNoCronJob(t *testing.T) {
	with := render(t, "tasks-staging-postgres.yaml")
	without := render(t, "tasks-staging-postgres.yaml", "--set", "runTasks=false")
	if found := objectsOfKind(without, "CronJob"); len(found) != 0 {
		t.Errorf("rendered %v with runTasks=false", found)
	}
	// Everything else is the same.
	withoutTasks := maps.Clone(with)
	for _, key := range objectsOfKind(with, "CronJob") {
		delete(withoutTasks, key)
	}
	if got, want := keys(without), keys(withoutTasks); !slices.Equal(got, want) {
		t.Errorf("runTasks=false rendered %v, want %v", got, want)
	}
	// A CronJob name that would be too long is not refused when no
	// CronJob is rendered (a Preview Environment's longer name).
	if out, err := helmTemplate(t, "refuse-task-cronjob-name-too-long.yaml", "--set", "runTasks=false"); err != nil {
		t.Errorf("runTasks=false still refused the task's CronJob name: %v\n%s", err, out)
	}
}

func TestNoTasksRenderNoCronJob(t *testing.T) {
	for _, fixture := range []string{"prod-small.yaml", "postgres-prod.yaml", "static-site.yaml"} {
		if found := objectsOfKind(render(t, fixture), "CronJob"); len(found) != 0 {
			t.Errorf("%s rendered %v without tasks", fixture, found)
		}
	}
}

func TestRenderingRefusesTasksItCannotHonour(t *testing.T) {
	for _, tc := range []struct{ fixture, message string }{
		{"refuse-tasks-static-site.yaml", "tasks needs kind: web-service; a Static site has no command of its own to run on a schedule"},
		{"refuse-task-name-used-twice.yaml", `tasks: name "cleanup" is used twice`},
		{"refuse-task-cronjob-name-too-long.yaml", `tasks: the CronJob name "shop-staging-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" is longer than the 52 characters Kubernetes allows`},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			out, err := helmTemplate(t, tc.fixture)
			if err == nil {
				t.Fatalf("rendering succeeded, want a failure:\n%s", out)
			}
			if !strings.Contains(out, tc.message) {
				t.Fatalf("error does not say %q:\n%s", tc.message, out)
			}
		})
	}
	// Values the Deploy gate would never write, but a hand edit could.
	for _, tc := range []struct{ set, message string }{
		{"tasks[0].name=Nightly", `tasks: name "Nightly" must be lowercase letters`},
		{"tasks[0].schedule=", `tasks: "sync" has no schedule`},
		{"tasks[0].command=", `tasks: "sync" has no command`},
	} {
		t.Run(tc.set, func(t *testing.T) {
			out, err := helmTemplate(t, "tasks-prod.yaml", "--set", tc.set)
			if err == nil || !strings.Contains(out, tc.message) {
				t.Errorf("err = %v, want a refusal saying %q:\n%s", err, tc.message, out)
			}
		})
	}
}

func TestTaskManifestsPassKubeconform(t *testing.T) {
	requireTool(t, "kubeconform")
	version := kubernetesVersion(t)
	// tasks-staging-postgres.yaml also renders the CloudNativePG objects,
	// whose schemas come from the CRDs catalog.
	for _, fixture := range []string{"tasks-prod.yaml", "tasks-staging-postgres.yaml"} {
		t.Run(fixture, func(t *testing.T) {
			manifests, err := helmTemplate(t, fixture)
			if err != nil {
				t.Fatalf("helm template %s: %v\n%s", fixture, err, manifests)
			}
			cmd := exec.Command("kubeconform", "-strict", "-summary", "-kubernetes-version", version,
				"-schema-location", "default", "-schema-location", crdSchemas)
			cmd.Stdin = strings.NewReader(manifests)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("kubeconform: %v\n%s", err, out)
			}
			t.Logf("kubeconform: %s", strings.TrimSpace(string(out)))
		})
	}
}
