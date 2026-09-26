package application_test

import (
	"fmt"
	"maps"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// The guardrails (#90): every container the chart renders carries the
// securityContext Pod Security's restricted level asks for, as far as the
// image allows; an image that runs as non-root passes restricted in full;
// the Environment's declared domains reach the bootstrap's Ingress-host
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

// Every container of every workload in every fixture has the RuntimeDefault
// seccomp profile and no privilege escalation; the chart renders a
// Deployment, the migration Job and the final Backup hook Job, and the
// fixtures between them render all three.
func TestEveryContainerHasTheSecurityContext(t *testing.T) {
	seen := map[string]bool{}
	for _, fixture := range renderingFixtures(t) {
		for key, podSpec := range podSpecs(t, render(t, fixture)) {
			seen[strings.SplitN(key, "/", 2)[0]+"/"+componentOf(key)] = true
			for _, c := range allContainers(t, podSpec) {
				where := fmt.Sprintf("%s: %s container %s", fixture, key, c["name"])
				if got := get[string](t, c, "securityContext", "seccompProfile", "type"); got != "RuntimeDefault" {
					t.Errorf("%s: seccompProfile.type = %q, want RuntimeDefault", where, got)
				}
				if got := get[bool](t, c, "securityContext", "allowPrivilegeEscalation"); got {
					t.Errorf("%s: allowPrivilegeEscalation = true, want false", where)
				}
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

// An image that may run as root (the default, and every Adopted image):
// nothing that would stop it starting. No runAsNonRoot, and the runtime's
// default capabilities, which nginx as root needs to start (CHOWN, SETUID,
// SETGID, NET_BIND_SERVICE for port 80).
func TestARootImageKeepsWhatItNeedsToStart(t *testing.T) {
	for _, tc := range []struct{ fixture, deployment string }{
		{"prod-small.yaml", "shop"},
		{"static-site.yaml", "brochure"},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			objects := render(t, tc.fixture)
			sc := get[map[string]any](t, container(t, objects, tc.deployment), "securityContext")
			for _, field := range []string{"runAsNonRoot", "capabilities", "runAsUser"} {
				if v, has := sc[field]; has {
					t.Errorf("securityContext.%s = %v, want it unset for an image that may run as root", field, v)
				}
			}
			podSpec := get[map[string]any](t, mustObject(t, objects, "Deployment/"+tc.deployment), "spec", "template", "spec")
			if v, has := podSpec["securityContext"]; has {
				t.Errorf("Pod securityContext = %v, want none: everything is on the container", v)
			}
		})
	}
	// The migration Job runs the same image, so it gets the same answer.
	objects := render(t, "postgres-prod.yaml")
	migrate := allContainers(t, podSpecs(t, objects)["Job/shop-migrate"])[0]
	if _, has := get[map[string]any](t, migrate, "securityContext")["runAsNonRoot"]; has {
		t.Errorf("the migration Job of a root image requires non-root")
	}
}

// The final Backup hook runs bitnami/kubectl, uid 1001, whatever the
// Application's image: it passes restricted even beside a root image.
func TestTheFinalBackupHookPassesRestricted(t *testing.T) {
	objects := render(t, "postgres-prod.yaml")
	podSpec, ok := podSpecs(t, objects)["Job/shop-final-backup"]
	if !ok {
		t.Fatalf("no Job/shop-final-backup in %v", keys(objects))
	}
	for _, violation := range restrictedViolations(t, podSpec) {
		t.Errorf("Job/shop-final-backup: %s", violation)
	}
}

// With runAsNonRoot, what iidp's templates build, every workload the chart
// renders passes Pod Security's restricted level: the Deployment, the
// migration Job and the final Backup hook.
func TestANonRootImagePassesRestricted(t *testing.T) {
	for _, fixture := range []string{"static-site-non-root.yaml", "postgres-non-root.yaml"} {
		t.Run(fixture, func(t *testing.T) {
			specs := podSpecs(t, render(t, fixture))
			if len(specs) == 0 {
				t.Fatal("no workload rendered")
			}
			for key, podSpec := range specs {
				for _, violation := range restrictedViolations(t, podSpec) {
					t.Errorf("%s: %s", key, violation)
				}
			}
		})
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

// iidp's Vite React template serves on 8080 as a non-root nginx: the
// container, the Service and both Ingress backends follow. A root nginx
// Static site (static_site_test.go) stays on 80.
func TestANonRootStaticSiteServesOn8080(t *testing.T) {
	objects := render(t, "static-site-non-root.yaml")
	c := container(t, objects, "brochure")
	if got := get[int](t, get[[]any](t, c, "ports")[0], "containerPort"); got != 8080 {
		t.Errorf("containerPort = %d, want 8080", got)
	}
	if env, has := c["env"]; has {
		t.Errorf("env = %v, want none: a Static site is told no PORT", env)
	}
	if got := get[int](t, get[[]any](t, mustObject(t, objects, "Service/brochure"), "spec", "ports")[0], "port"); got != 8080 {
		t.Errorf("Service port = %d, want 8080", got)
	}
	paths := get[[]any](t, get[[]any](t, mustObject(t, objects, "Ingress/brochure"), "spec", "rules")[0], "http", "paths")
	if got := get[int](t, paths[0], "backend", "service", "port", "number"); got != 8080 {
		t.Errorf("Ingress backend port = %d, want 8080", got)
	}
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

// The new fixtures pass kubeconform like the others.
func TestGuardrailFixturesPassKubeconform(t *testing.T) {
	requireTool(t, "kubeconform")
	version := kubernetesVersion(t)
	for _, fixture := range []string{"static-site-non-root.yaml", "postgres-non-root.yaml"} {
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
