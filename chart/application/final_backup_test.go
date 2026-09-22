package application_test

import (
	"strings"
	"testing"
)

// TestFinalBackupHookRendersOnlyWithPostgres asserts the PreDelete hook and
// its RBAC render only when Postgres is enabled: nothing here could target
// a Cluster that does not exist.
func TestFinalBackupHookRendersOnlyWithPostgres(t *testing.T) {
	objects := render(t, "prod-small.yaml")
	for _, key := range []string{
		"ServiceAccount/shop-final-backup",
		"Role/shop-final-backup",
		"RoleBinding/shop-final-backup",
		"Job/shop-final-backup",
	} {
		if _, ok := objects[key]; ok {
			t.Errorf("rendered %s with Postgres off", key)
		}
	}
}

// TestFinalBackupOnlyTheJobIsAPreDeleteHook covers a deliberate design
// choice (docs/implementation-notes/39-final-backup-predelete-hook.md):
// only the Job carries a PreDelete hook annotation. The ServiceAccount,
// Role and RoleBinding are ordinary chart resources -- no hook annotation,
// no sync-wave -- present whenever postgres.enabled and pruned with the
// rest of the Environment's resources, the same as the Cluster or the
// Deployment. Having four hook objects instead of one, each one's own
// creation event asking ArgoCD to refresh the Application again, was found
// to trigger a real ArgoCD race under concurrent reconciles; with the RBAC
// no longer hooks, deletion creates exactly one hook object.
func TestFinalBackupOnlyTheJobIsAPreDeleteHook(t *testing.T) {
	objects := render(t, "postgres-prod.yaml")

	for _, key := range []string{"ServiceAccount/shop-final-backup", "Role/shop-final-backup", "RoleBinding/shop-final-backup"} {
		obj := mustObject(t, objects, key)
		annotations := annotationsOf(t, obj)
		if hook, set := annotations["argocd.argoproj.io/hook"]; set {
			t.Errorf("%s sets argocd.argoproj.io/hook = %v; it must be an ordinary resource, not a hook", key, hook)
		}
		if _, set := annotations["argocd.argoproj.io/hook-delete-policy"]; set {
			t.Errorf("%s sets a hook-delete-policy; it must be an ordinary resource, not a hook", key)
		}
		if _, set := annotations["argocd.argoproj.io/sync-wave"]; set {
			t.Errorf("%s sets a sync-wave; ordinary resources need no wave relative to the hook Job", key)
		}
	}

	job := mustObject(t, objects, "Job/shop-final-backup")
	annotations := annotationsOf(t, job)
	if hook := annotations["argocd.argoproj.io/hook"]; hook != "PreDelete" {
		t.Errorf("Job hook = %v, want PreDelete", hook)
	}
	if policy := annotations["argocd.argoproj.io/hook-delete-policy"]; policy != "BeforeHookCreation,HookSucceeded" {
		t.Errorf("Job hook-delete-policy = %v, want BeforeHookCreation,HookSucceeded", policy)
	}
}

func TestFinalBackupRBACIsScopedToBackups(t *testing.T) {
	objects := render(t, "postgres-prod.yaml")

	role := mustObject(t, objects, "Role/shop-final-backup")
	rules := get[[]any](t, role, "rules")
	if len(rules) != 1 {
		t.Fatalf("Role has %d rules, want 1", len(rules))
	}
	rule := get[map[string]any](t, map[string]any{"r": rules[0]}, "r")
	if groups := get[[]any](t, rule, "apiGroups"); len(groups) != 1 || groups[0] != "postgresql.cnpg.io" {
		t.Errorf("apiGroups = %v, want [postgresql.cnpg.io]", groups)
	}
	if resources := get[[]any](t, rule, "resources"); len(resources) != 1 || resources[0] != "backups" {
		t.Errorf("resources = %v, want [backups]", resources)
	}
	verbs := get[[]any](t, rule, "verbs")
	wantVerbs := []any{"create", "get", "list", "watch"}
	if len(verbs) != len(wantVerbs) {
		t.Fatalf("verbs = %v, want %v", verbs, wantVerbs)
	}
	for i, v := range wantVerbs {
		if verbs[i] != v {
			t.Errorf("verbs[%d] = %v, want %v", i, verbs[i], v)
		}
	}

	binding := mustObject(t, objects, "RoleBinding/shop-final-backup")
	if kind := get[string](t, binding, "roleRef", "kind"); kind != "Role" {
		t.Errorf("roleRef.kind = %q, want Role", kind)
	}
	if name := get[string](t, binding, "roleRef", "name"); name != "shop-final-backup" {
		t.Errorf("roleRef.name = %q, want shop-final-backup", name)
	}
	subjects := get[[]any](t, binding, "subjects")
	if len(subjects) != 1 {
		t.Fatalf("subjects = %v, want exactly one", subjects)
	}
	subject := get[map[string]any](t, map[string]any{"s": subjects[0]}, "s")
	if kind := get[string](t, subject, "kind"); kind != "ServiceAccount" {
		t.Errorf("subject kind = %q, want ServiceAccount", kind)
	}
	if name := get[string](t, subject, "name"); name != "shop-final-backup" {
		t.Errorf("subject name = %q, want shop-final-backup", name)
	}

	job := mustObject(t, objects, "Job/shop-final-backup")
	if sa := get[string](t, job, "spec", "template", "spec", "serviceAccountName"); sa != "shop-final-backup" {
		t.Errorf("Job serviceAccountName = %q, want shop-final-backup", sa)
	}
}

func TestFinalBackupJobShapeAndScript(t *testing.T) {
	cases := []struct {
		fixture, cluster, namespace string
	}{
		// The namespace is .Release.Namespace: real ArgoCD renders the
		// chart with the Environment's Application destination namespace,
		// application-environment (shop-prod, shop-staging, from
		// render.Environment.Name in internal/render), passed explicitly
		// here the same way.
		{"postgres-prod.yaml", "shop-db", "shop-prod"},
		{"postgres-staging.yaml", "shop-staging-db", "shop-staging"},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			objects := render(t, tc.fixture, "--namespace", tc.namespace)
			job := mustObject(t, objects, "Job/"+strings.TrimSuffix(tc.cluster, "-db")+"-final-backup")

			if limit := get[int](t, job, "spec", "backoffLimit"); limit != 0 {
				t.Errorf("backoffLimit = %d, want 0", limit)
			}
			if policy := get[string](t, job, "spec", "template", "spec", "restartPolicy"); policy != "Never" {
				t.Errorf("restartPolicy = %q, want Never", policy)
			}

			containers := get[[]any](t, job, "spec", "template", "spec", "containers")
			if len(containers) != 1 {
				t.Fatalf("Job has %d containers, want 1", len(containers))
			}
			c := get[map[string]any](t, map[string]any{"c": containers[0]}, "c")

			if image := get[string](t, c, "image"); image != "docker.io/bitnami/kubectl@sha256:6e9c5284a0dac06e84de9f4d97852d2e6513442ee7ec3a66d35009eec86e1e62" {
				t.Errorf("image = %q, want the pinned kubectl image", image)
			}
			for _, field := range []string{"requests", "limits"} {
				if _, ok := get[map[string]any](t, c, "resources", field)["cpu"]; !ok {
					t.Errorf("resources.%s has no cpu", field)
				}
			}

			args := get[[]any](t, c, "args")
			if len(args) != 1 {
				t.Fatalf("args = %v, want exactly one script", args)
			}
			script := get[string](t, map[string]any{"s": args[0]}, "s")
			for _, want := range []string{
				"CLUSTER=\"" + tc.cluster + "\"",
				"NAMESPACE=\"" + tc.namespace + "\"",
				"method: plugin",
				"name: ${PLUGIN}",
				"barman-cloud.cloudnative-pg.io",
				"iidp.itema.no/retain-until",
				"date -u -d '+30 days' +%Y-%m-%d",
				"status.phase",
				"completed",
				"failed",
				"TIMEOUT=1800",
			} {
				if !strings.Contains(script, want) {
					t.Errorf("script does not contain %q:\n%s", want, script)
				}
			}
		})
	}
}

func TestFinalBackupTimeoutIsConfigurable(t *testing.T) {
	objects := render(t, "final-backup-timeout.yaml")
	job := mustObject(t, objects, "Job/shop-final-backup")
	c := get[map[string]any](t, map[string]any{"c": get[[]any](t, job, "spec", "template", "spec", "containers")[0]}, "c")
	script := get[string](t, map[string]any{"s": get[[]any](t, c, "args")[0]}, "s")
	if !strings.Contains(script, "TIMEOUT=60") {
		t.Errorf("script does not honour postgres.finalBackupTimeout=60:\n%s", script)
	}
}
