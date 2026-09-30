package application_test

import (
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"
)

// The guardrails (#90, #110): every workload the chart renders passes Pod
// Security's restricted level; the Environment's declared domains reach the bootstrap's Ingress-host
// policy through a ConfigMap; and every container, the database's backup
// sidecar included, has limits.

// podSpecs returns every Pod template the rendered objects carry, keyed by
// "Kind/name", whatever the workload kind: a Deployment's or a Job's
// spec.template, a CronJob's spec.jobTemplate.spec.template. A workload the
// chart adds later is found here without the test changing.
func podSpecs(t *testing.T, objects map[string]object) map[string]map[string]any {
	t.Helper()
	specs := map[string]map[string]any{}
	for key, obj := range objects {
		spec, ok := obj["spec"].(map[string]any)
		if !ok {
			continue
		}
		if jobTemplate, ok := spec["jobTemplate"].(map[string]any); ok {
			spec, _ = jobTemplate["spec"].(map[string]any)
		}
		template, ok := spec["template"].(map[string]any)
		if !ok {
			continue
		}
		if podSpec, ok := template["spec"].(map[string]any); ok {
			specs[key] = podSpec
		}
	}
	return specs
}

// allContainers returns a Pod spec's containers and init containers.
func allContainers(t *testing.T, podSpec map[string]any) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, field := range []string{"initContainers", "containers"} {
		list, _ := podSpec[field].([]any)
		for _, c := range list {
			out = append(out, get[map[string]any](t, map[string]any{"c": c}, "c"))
		}
	}
	return out
}

// renderingFixtures lists every fixture that renders (not refuse-*).
func renderingFixtures(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir("testdata")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".yaml") && !strings.HasPrefix(e.Name(), "refuse-") {
			fixtures = append(fixtures, e.Name())
		}
	}
	return fixtures
}

// Every workload in every fixture passes Pod Security's restricted level,
// which Application namespaces enforce (#110): the Deployment, the
// migration Job, the final Backup hook Job and the Scheduled task
// CronJobs, and the fixtures between them render all four.
func TestEveryWorkloadPassesRestricted(t *testing.T) {
	seen := map[string]bool{}
	for _, fixture := range renderingFixtures(t) {
		for key, podSpec := range podSpecs(t, render(t, fixture)) {
			seen[strings.SplitN(key, "/", 2)[0]+"/"+componentOf(key)] = true
			for _, violation := range restrictedViolations(t, podSpec) {
				t.Errorf("%s: %s: %s", fixture, key, violation)
			}
		}
	}
	for _, want := range []string{"Deployment/app", "Job/migrate", "Job/final-backup", "CronJob/app"} {
		if !seen[want] {
			t.Errorf("no fixture renders a %s workload, so its securityContext is untested (saw %v)", want, slices.Sorted(maps.Keys(seen)))
		}
	}
}

// componentOf names a workload by what it is, not by its Environment:
// shop-migrate is "migrate", shop-staging-final-backup is "final-backup",
// the Application's own Deployment is "app".
func componentOf(key string) string {
	name := strings.SplitN(key, "/", 2)[1]
	for _, suffix := range []string{"final-backup", "migrate"} {
		if strings.HasSuffix(name, "-"+suffix) {
			return suffix
		}
	}
	return "app"
}

// runAsNonRoot was a value before #110, and an Environment written before
// it may still carry runAsNonRoot: false. The chart ignores it: the
// container still runs as non-root, and a Static site still on 8080.
func TestALeftoverRunAsNonRootChangesNothing(t *testing.T) {
	objects := render(t, "static-site.yaml", "--set", "runAsNonRoot=false")
	c := container(t, objects, "brochure")
	if got := get[bool](t, c, "securityContext", "runAsNonRoot"); !got {
		t.Errorf("securityContext.runAsNonRoot = false, want true whatever the values say")
	}
	if got := get[int](t, get[[]any](t, c, "ports")[0], "containerPort"); got != 8080 {
		t.Errorf("containerPort = %d, want 8080", got)
	}
}

// restrictedViolations checks a Pod spec against the controls of Pod
// Security's restricted level (kubernetes.io/docs/concepts/security/
// pod-security-standards, v1.36), baseline's included, the way the
// admission controller reads them: a container-level field wins over the
// Pod-level one. It is the offline half of the check; the kind
// end-to-end test and the Templates CI job apply real Pods to a namespace
// that enforces restricted.
func restrictedViolations(t *testing.T, podSpec map[string]any) []string {
	t.Helper()
	var out []string
	podSC, _ := podSpec["securityContext"].(map[string]any)
	for _, field := range []string{"hostNetwork", "hostPID", "hostIPC"} {
		if v, ok := podSpec[field].(bool); ok && v {
			out = append(out, field+" is true")
		}
	}
	for _, v := range asList(podSpec["volumes"]) {
		volume, _ := v.(map[string]any)
		allowed := false
		for _, kind := range []string{"configMap", "csi", "downwardAPI", "emptyDir", "ephemeral", "persistentVolumeClaim", "projected", "secret"} {
			if _, ok := volume[kind]; ok {
				allowed = true
			}
		}
		if !allowed {
			out = append(out, fmt.Sprintf("volume %v is of a type restricted does not allow", volume["name"]))
		}
	}
	podNonRoot, _ := podSC["runAsNonRoot"].(bool)
	podSeccomp := ""
	if p, ok := podSC["seccompProfile"].(map[string]any); ok {
		podSeccomp, _ = p["type"].(string)
	}
	if uid, ok := podSC["runAsUser"].(int); ok && uid == 0 {
		out = append(out, "Pod runAsUser is 0")
	}
	for _, c := range allContainers(t, podSpec) {
		name := c["name"]
		sc, _ := c["securityContext"].(map[string]any)
		if v, ok := sc["privileged"].(bool); ok && v {
			out = append(out, fmt.Sprintf("container %v is privileged", name))
		}
		for _, p := range asList(c["ports"]) {
			if port, _ := p.(map[string]any); port["hostPort"] != nil {
				out = append(out, fmt.Sprintf("container %v sets a hostPort", name))
			}
		}
		if v, ok := sc["allowPrivilegeEscalation"].(bool); !ok || v {
			out = append(out, fmt.Sprintf("container %v does not set allowPrivilegeEscalation: false", name))
		}
		nonRoot := podNonRoot
		if v, ok := sc["runAsNonRoot"].(bool); ok {
			nonRoot = v
		}
		if !nonRoot {
			out = append(out, fmt.Sprintf("container %v does not set runAsNonRoot: true", name))
		}
		if uid, ok := sc["runAsUser"].(int); ok && uid == 0 {
			out = append(out, fmt.Sprintf("container %v runs as uid 0", name))
		}
		seccomp := podSeccomp
		if p, ok := sc["seccompProfile"].(map[string]any); ok {
			seccomp, _ = p["type"].(string)
		}
		if seccomp != "RuntimeDefault" && seccomp != "Localhost" {
			out = append(out, fmt.Sprintf("container %v has seccomp profile %q", name, seccomp))
		}
		caps, _ := sc["capabilities"].(map[string]any)
		if !slices.Contains(asList(caps["drop"]), any("ALL")) {
			out = append(out, fmt.Sprintf("container %v does not drop ALL capabilities", name))
		}
		for _, add := range asList(caps["add"]) {
			if add != "NET_BIND_SERVICE" {
				out = append(out, fmt.Sprintf("container %v adds capability %v", name, add))
			}
		}
	}
	return out
}

func asList(v any) []any {
	list, _ := v.([]any)
	return list
}

// The declared domains reach the bootstrap's Ingress-host policy as the
// keys of ConfigMap iidp-domains, every custom domain whichever certificate
// it gets; an Environment without custom domains still has the ConfigMap,
// empty, since the policy does not check an Ingress in a namespace without
// it at all.
func TestDeclaredDomainsAreTheKeysOfTheDomainsConfigMap(t *testing.T) {
	for _, tc := range []struct {
		fixture string
		want    []string
	}{
		{"custom-domains-mixed.yaml", []string{"butikk-staging.app.itma.no", "shop-staging.itma.no", "test.shop.app.itma.no"}},
		{"prod-small.yaml", nil},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			cm := mustObject(t, render(t, tc.fixture), "ConfigMap/iidp-domains")
			data, _ := cm["data"].(map[string]any)
			var got []string
			for key, value := range data {
				if value != "" {
					t.Errorf("data[%s] = %v, want an empty value: the key is the domain", key, value)
				}
				got = append(got, key)
			}
			slices.Sort(got)
			if !slices.Equal(got, tc.want) {
				t.Errorf("declared domains = %v, want %v", got, tc.want)
			}
			// Applied before the Ingresses that use it.
			if got := get[string](t, cm, "metadata", "annotations", "argocd.argoproj.io/sync-wave"); got != "-1" {
				t.Errorf("sync-wave = %q, want -1, before the Ingresses", got)
			}
		})
	}
}

// The Barman Cloud plugin's sidecar in the database Pod has limits, like
// every other container in an Application namespace, with a small request
// for the CPU-tight node and kind runner.
func TestTheBackupSidecarHasLimits(t *testing.T) {
	store := mustObject(t, render(t, "postgres-prod.yaml"), "ObjectStore/shop-db")
	resources := get[map[string]any](t, store, "spec", "instanceSidecarConfiguration", "resources")
	for _, field := range []string{"cpu", "memory"} {
		get[string](t, resources, "limits", field)
	}
	if got := get[string](t, resources, "requests", "cpu"); got != "10m" {
		t.Errorf("sidecar cpu request = %q, want 10m", got)
	}
}
