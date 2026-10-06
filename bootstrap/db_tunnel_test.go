package bootstrap_test

import (
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"testing"
)

func TestDatabaseTunnelIsOnByDefaultAndOffRendersNothing(t *testing.T) {
	args := []string{"--values", "values.yaml", "--set", "baseDomain=app.example.test", "--set", "cloudflareZone=example.test",
		"--set", "bootstrap.repoURL=https://example.test/iidp.git", "--set", "bootstrap.targetRevision=v9.9.9"}
	tunnel, ok := renderApplications(t, args...)["db-tunnel"]
	if !ok {
		t.Fatal("values.yaml renders no db-tunnel Application; the Database tunnel is on by default")
	}
	for path, want := range map[string]string{
		"repoURL":        "https://example.test/iidp.git",
		"targetRevision": "v9.9.9",
		"path":           "bootstrap/components/db-tunnel",
	} {
		if got := get[string](t, tunnel, "spec", "source", path); got != want {
			t.Errorf("db-tunnel source %s = %q, want %q", path, got, want)
		}
	}
	if got := get[string](t, tunnel, "spec", "destination", "namespace"); got != "iidp-db-tunnel" {
		t.Errorf("db-tunnel namespace = %q, want iidp-db-tunnel, where the application chart's Roles bind its ServiceAccount", got)
	}
	labels := get[object](t, tunnel, "spec", "syncPolicy", "managedNamespaceMetadata", "labels")
	if labels["pod-security.kubernetes.io/enforce"] != "restricted" {
		t.Errorf("the tunnel's namespace labels = %v, want Pod Security restricted enforced", labels)
	}
	values := get[object](t, tunnel, "spec", "source", "helm", "valuesObject")
	for path, want := range map[string]any{
		"baseDomain":         "app.example.test",
		"bootstrapRevision":  "v9.9.9",
		"githubOrgId":        1230559,
		"githubAPI":          "",
		"platformRepository": "https://github.com/Itema-as/iidp-platform.git",
		"image.repository":   "ghcr.io/itema-as/iidp-db-tunnel",
	} {
		if got := get[any](t, values, strings.Split(path, ".")...); fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("db-tunnel %s = %v, want %v", path, got, want)
		}
	}

	// The fixture's tunnel asks the harness's fake GitHub, with the image
	// the harness builds.
	fixtureValues := get[object](t, renderApplications(t, "--values", fixture)["db-tunnel"], "spec", "source", "helm", "valuesObject")
	if got := get[string](t, fixtureValues, "githubAPI"); !strings.Contains(got, "iidp-e2e.svc.cluster.local") {
		t.Errorf("the fixture's githubAPI = %q, want the harness's fake GitHub", got)
	}
	if got := get[string](t, fixtureValues, "image", "tag"); got == "" {
		t.Errorf("the fixture's image.tag is empty, want the harness's locally built image")
	}

	// Off, it renders nothing, and leaves every other component as it was.
	on := renderApplications(t, args...)
	off := renderApplications(t, append(append([]string{}, args...), "--set", "dbTunnel.enabled=false")...)
	if rendered := renderText(t, append(append([]string{}, args...), "--set", "dbTunnel.enabled=false")...); strings.Contains(rendered, "tunnel") {
		t.Errorf("with the Database tunnel off, the bootstrap still renders it:\n%s", rendered)
	}
	delete(on, "db-tunnel")
	if fmt.Sprint(on) != fmt.Sprint(off) {
		t.Errorf("turning the Database tunnel off changes the other components")
	}
}

func TestDatabaseTunnelComponentRendersTheTunnel(t *testing.T) {
	objects := parseObjects(t, helmTemplate(t, "components/db-tunnel", "--namespace", "iidp-db-tunnel",
		"--set", "baseDomain=app.example.test", "--set", "bootstrapRevision=v1.2.3", "--set", "githubOrgId=1230559",
		"--set", "platformRepository=https://github.com/Itema-as/iidp-platform.git"))
	if got := strings.Join(keys(objects), " "); got != "ClusterRole/iidp-db-tunnel ClusterRoleBinding/iidp-db-tunnel Deployment/iidp-db-tunnel Ingress/iidp-db-tunnel Role/iidp-db-tunnel-events RoleBinding/iidp-db-tunnel-events Service/iidp-db-tunnel ServiceAccount/iidp-db-tunnel" {
		t.Errorf("rendered %s", got)
	}

	deployment := objects["Deployment/iidp-db-tunnel"]
	if got := get[int](t, deployment, "spec", "replicas"); got != 1 {
		t.Errorf("replicas = %d, want 1", got)
	}
	pod := get[object](t, deployment, "spec", "template", "spec")
	if get[string](t, pod, "serviceAccountName") != "iidp-db-tunnel" || !get[bool](t, pod, "automountServiceAccountToken") {
		t.Errorf("pod service account = %v, token %v; want its own, mounted", pod["serviceAccountName"], pod["automountServiceAccountToken"])
	}
	// What Pod Security's restricted level requires of the pod and its
	// container.
	podSecurity := get[object](t, pod, "securityContext")
	if podSecurity["runAsNonRoot"] != true || get[string](t, podSecurity, "seccompProfile", "type") != "RuntimeDefault" {
		t.Errorf("pod securityContext = %v, want non-root with the RuntimeDefault seccomp profile", podSecurity)
	}
	container := get[[]any](t, pod, "containers")[0].(object)
	security := get[object](t, container, "securityContext")
	if security["allowPrivilegeEscalation"] != false || security["readOnlyRootFilesystem"] != true || fmt.Sprint(get[[]any](t, security, "capabilities", "drop")) != "[ALL]" {
		t.Errorf("container securityContext = %v, want no privilege escalation, a read-only root and every capability dropped", security)
	}
	if got := container["image"]; got != "ghcr.io/itema-as/iidp-db-tunnel:1.2.3" {
		t.Errorf("image = %v, want the bootstrap release's version", got)
	}
	if got := fmt.Sprint(get[object](t, container, "resources")); got != "map[limits:map[memory:128Mi] requests:map[cpu:5m memory:32Mi]]" {
		t.Errorf("resources = %s, want small ones: cpu 5m, memory 32Mi, limit 128Mi", got)
	}
	env := map[string]string{}
	for _, e := range get[[]any](t, container, "env") {
		m := e.(object)
		env[m["name"].(string)] = fmt.Sprint(m["value"])
	}
	if fmt.Sprint(env) != fmt.Sprint(map[string]string{
		"HOME":                      "/tmp",
		"IIDP_TUNNEL_ORG_ID":        "1230559",
		"IIDP_TUNNEL_PLATFORM_REPO": "https://github.com/Itema-as/iidp-platform.git",
	}) {
		t.Errorf("env = %v", env)
	}
	// Each check clones the Platform repository; nothing else is mounted.
	volumes := get[[]any](t, pod, "volumes")
	if len(volumes) != 1 || volumes[0].(object)["name"] != "tmp" || volumes[0].(object)["emptyDir"] == nil {
		t.Errorf("volumes = %v, want only an emptyDir for the clones", volumes)
	}

	// Not behind Itema login: the developer's GitHub token is what lets
	// them in.
	ingress := objects["Ingress/iidp-db-tunnel"]
	rule := get[[]any](t, ingress, "spec", "rules")[0].(object)
	if got := get[string](t, rule, "host"); got != "db.app.example.test" {
		t.Errorf("Ingress host = %q, want db.<baseDomain>", got)
	}
	annotations := get[object](t, ingress, "metadata", "annotations")
	if _, ok := annotations["traefik.ingress.kubernetes.io/router.middlewares"]; ok {
		t.Errorf("the Ingress has middlewares %v; the tunnel is not behind Itema login", annotations)
	}
	if annotations["traefik.ingress.kubernetes.io/router.entrypoints"] != "websecure" || annotations["traefik.ingress.kubernetes.io/router.tls"] != "true" {
		t.Errorf("Ingress annotations = %v, want websecure with TLS", annotations)
	}
	if tls := get[[]any](t, ingress, "spec", "tls")[0].(object); tls["secretName"] != nil {
		t.Errorf("the Ingress names a TLS secret; the wildcard comes from Traefik's default store")
	}
}

// get on CloudNativePG Clusters everywhere, and create on Events
// in argocd only, bound to no one but the tunnel. The Secrets it reads are
// granted Environment by Environment, by the application chart.
func TestDatabaseTunnelHasExactlyItsRBAC(t *testing.T) {
	objects := parseObjects(t, helmTemplate(t, "components/db-tunnel", "--namespace", "iidp-db-tunnel",
		"--set", "baseDomain=app.example.test", "--set", "bootstrapRevision=v1.2.3"))
	var grants []string
	for key, obj := range objects {
		kind := obj["kind"].(string)
		switch kind {
		case "Role", "ClusterRole":
			namespace, _ := get[object](t, obj, "metadata")["namespace"].(string)
			if kind == "ClusterRole" {
				namespace = "every namespace"
			}
			for _, r := range get[[]any](t, obj, "rules") {
				rule := r.(object)
				for _, field := range []string{"nonResourceURLs", "resourceNames"} {
					if _, ok := rule[field]; ok {
						t.Errorf("%s has %s: %v", key, field, rule)
					}
				}
				var verbs []string
				for _, v := range rule["verbs"].([]any) {
					verbs = append(verbs, v.(string))
				}
				sort.Strings(verbs)
				for _, g := range rule["apiGroups"].([]any) {
					for _, res := range rule["resources"].([]any) {
						grants = append(grants, fmt.Sprintf("%s in %s: %s/%s %s", key, namespace, g, res, strings.Join(verbs, ",")))
					}
				}
			}
		case "RoleBinding", "ClusterRoleBinding":
			roleKind := strings.TrimSuffix(kind, "Binding")
			if get[string](t, obj, "roleRef", "kind") != roleKind || objects[roleKind+"/"+get[string](t, obj, "roleRef", "name")] == nil {
				t.Errorf("%s refers to %v, which the chart does not render as a %s", key, obj["roleRef"], roleKind)
			}
			subjects := get[[]any](t, obj, "subjects")
			if len(subjects) != 1 || fmt.Sprint(subjects[0]) != fmt.Sprint(object{"kind": "ServiceAccount", "name": "iidp-db-tunnel", "namespace": "iidp-db-tunnel"}) {
				t.Errorf("%s binds %v, want only iidp-db-tunnel/iidp-db-tunnel", key, subjects)
			}
		}
	}
	sort.Strings(grants)
	want := []string{
		"ClusterRole/iidp-db-tunnel in every namespace: postgresql.cnpg.io/clusters get",
		"Role/iidp-db-tunnel-events in argocd: events.k8s.io/events create",
	}
	if strings.Join(grants, "\n") != strings.Join(want, "\n") {
		t.Errorf("the tunnel may:\n%s\nwant exactly:\n%s", strings.Join(grants, "\n"), strings.Join(want, "\n"))
	}
	if ns := get[string](t, objects["RoleBinding/iidp-db-tunnel-events"], "metadata", "namespace"); ns != "argocd" {
		t.Errorf("RoleBinding/iidp-db-tunnel-events is in %q, want argocd", ns)
	}
	if get[bool](t, objects["ServiceAccount/iidp-db-tunnel"], "automountServiceAccountToken") {
		t.Errorf("the ServiceAccount mounts its token everywhere; only the tunnel's pod should")
	}
}

// The application chart binds its Secret Roles to the ServiceAccount the
// component runs as, by name.
func TestTheApplicationChartsTunnelIsTheComponentsServiceAccount(t *testing.T) {
	chart := readYAML(t, "../chart/application/values.yaml")
	if got := get[string](t, chart, "platform", "databaseTunnel", "serviceAccount"); got != "iidp-db-tunnel" {
		t.Errorf("the application chart binds %q, want the component's ServiceAccount iidp-db-tunnel", got)
	}
	tunnel := renderApplications(t, "--values", fixture)["db-tunnel"]
	if got, want := get[string](t, tunnel, "spec", "destination", "namespace"), get[string](t, chart, "platform", "databaseTunnel", "namespace"); got != want {
		t.Errorf("the component runs in %q, and the application chart binds a ServiceAccount in %q", got, want)
	}
}

func TestDatabaseTunnelImageTagCanBePinnedButNotGuessed(t *testing.T) {
	objects := parseObjects(t, helmTemplate(t, "components/db-tunnel",
		"--set", "bootstrapRevision=main", "--set", "image.tag=dev", "--set", "image.repository=iidp-e2e.local/db-tunnel"))
	container := get[[]any](t, objects["Deployment/iidp-db-tunnel"], "spec", "template", "spec", "containers")[0].(object)
	if got := container["image"]; got != "iidp-e2e.local/db-tunnel:dev" {
		t.Errorf("image = %v, want the pinned tag", got)
	}

	requireHelm(t)
	out, err := exec.Command("helm", "template", "t", "components/db-tunnel", "--set", "bootstrapRevision=main").CombinedOutput()
	if err == nil || !strings.Contains(string(out), "not a v* release tag") {
		t.Errorf("rendering with the bootstrap on a branch and no tag: err = %v, output:\n%s\nwant a refusal naming the release tag", err, out)
	}
}
