// Package application_test drives the generic Application chart from the
// outside: it renders fixture values files with helm template, parses the
// manifests, and asserts on what an Environment would receive.
package application_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// object is one rendered Kubernetes manifest.
type object = map[string]any

// render runs helm template on the chart with the given fixture values file
// and returns the rendered objects keyed by "Kind/name".
func render(t *testing.T, fixture string) map[string]object {
	t.Helper()
	out, err := helmTemplate(t, fixture)
	if err != nil {
		t.Fatalf("helm template %s: %v\n%s", fixture, err, out)
	}
	return parseObjects(t, out)
}

// helmTemplate runs helm template with a fixture values file and returns the
// combined output and the command's error, so tests can assert on both
// successful renders and refusals.
func helmTemplate(t *testing.T, fixture string) (string, error) {
	t.Helper()
	requireTool(t, "helm")
	cmd := exec.Command("helm", "template", "test-release", ".", "--values", filepath.Join("testdata", fixture))
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return out.String(), err
}

func parseObjects(t *testing.T, manifests string) map[string]object {
	t.Helper()
	objects := map[string]object{}
	dec := yaml.NewDecoder(strings.NewReader(manifests))
	for {
		var obj object
		err := dec.Decode(&obj)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("parse rendered manifests: %v\n%s", err, manifests)
		}
		if obj == nil {
			continue
		}
		key := fmt.Sprintf("%s/%s", obj["kind"], get[string](t, obj, "metadata", "name"))
		if _, dup := objects[key]; dup {
			t.Fatalf("rendered %s twice", key)
		}
		objects[key] = obj
	}
	return objects
}

// requireTool skips the test when the tool is not on PATH, so go test ./...
// stays green on machines without helm or kubeconform.
func requireTool(t *testing.T, name string) {
	t.Helper()
	if _, err := exec.LookPath(name); err != nil {
		t.Skipf("%s not on PATH", name)
	}
}

// get walks nested maps by key and returns the value at the end of the path
// as T, failing the test when the path is missing or of another type.
func get[T any](t *testing.T, obj any, path ...string) T {
	t.Helper()
	current := obj
	for i, key := range path {
		m, ok := current.(map[string]any)
		if !ok {
			t.Fatalf("%s: not a map at %q", strings.Join(path, "."), strings.Join(path[:i], "."))
		}
		next, found := m[key]
		if !found {
			t.Fatalf("%s: missing key %q", strings.Join(path, "."), key)
		}
		current = next
	}
	v, ok := current.(T)
	if !ok {
		var zero T
		t.Fatalf("%s: got %T (%v), want %T", strings.Join(path, "."), current, current, zero)
	}
	return v
}

// container returns the single container of the named Deployment.
func container(t *testing.T, objects map[string]object, deployment string) map[string]any {
	t.Helper()
	dep, ok := objects["Deployment/"+deployment]
	if !ok {
		t.Fatalf("no Deployment/%s rendered; got %v", deployment, keys(objects))
	}
	containers := get[[]any](t, dep, "spec", "template", "spec", "containers")
	if len(containers) != 1 {
		t.Fatalf("Deployment/%s has %d containers, want 1", deployment, len(containers))
	}
	return containers[0].(map[string]any)
}

// envVars returns the container's env as name to value.
func envVars(t *testing.T, c map[string]any) map[string]string {
	t.Helper()
	vars := map[string]string{}
	for _, item := range get[[]any](t, c, "env") {
		entry := item.(map[string]any)
		vars[get[string](t, entry, "name")] = get[string](t, entry, "value")
	}
	return vars
}

func keys(objects map[string]object) []string {
	out := make([]string, 0, len(objects))
	for k := range objects {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestEverySizeSetsRequestsAndLimits(t *testing.T) {
	cases := []struct {
		fixture, deployment, cpu, memory string
	}{
		{"prod-small.yaml", "shop", "250m", "256Mi"},
		{"staging-medium.yaml", "shop-staging", "500m", "512Mi"},
		{"prod-large.yaml", "warehouse", "1", "1Gi"},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			c := container(t, render(t, tc.fixture), tc.deployment)
			for _, field := range []string{"requests", "limits"} {
				if cpu := get[string](t, c, "resources", field, "cpu"); cpu != tc.cpu {
					t.Errorf("%s.cpu = %q, want %q", field, cpu, tc.cpu)
				}
				if mem := get[string](t, c, "resources", field, "memory"); mem != tc.memory {
					t.Errorf("%s.memory = %q, want %q", field, mem, tc.memory)
				}
			}
		})
	}
}

func TestPortIsInjectedAndExposedByTheContainer(t *testing.T) {
	cases := []struct {
		fixture, deployment string
		port                int
	}{
		{"prod-small.yaml", "shop", 3000},
		{"staging-medium.yaml", "shop-staging", 8080},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			c := container(t, render(t, tc.fixture), tc.deployment)

			if got := envVars(t, c)["PORT"]; got != fmt.Sprint(tc.port) {
				t.Errorf("PORT = %q, want %d", got, tc.port)
			}
			ports := get[[]any](t, c, "ports")
			if len(ports) != 1 {
				t.Fatalf("container has %d ports, want 1", len(ports))
			}
			if got := get[int](t, ports[0], "containerPort"); got != tc.port {
				t.Errorf("containerPort = %d, want %d", got, tc.port)
			}
		})
	}
}

func TestProbesRequestTheProbePath(t *testing.T) {
	cases := []struct {
		fixture, deployment, path string
	}{
		{"prod-small.yaml", "shop", "/"},
		{"staging-medium.yaml", "shop-staging", "/healthz"},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			c := container(t, render(t, tc.fixture), tc.deployment)
			for _, probe := range []string{"readinessProbe", "livenessProbe"} {
				if path := get[string](t, c, probe, "httpGet", "path"); path != tc.path {
					t.Errorf("%s path = %q, want %q", probe, path, tc.path)
				}
				if port := get[string](t, c, probe, "httpGet", "port"); port != "http" {
					t.Errorf("%s port = %q, want the named http port", probe, port)
				}
			}
		})
	}
}

func TestPlainEnvIsPassedToTheContainer(t *testing.T) {
	c := container(t, render(t, "staging-medium.yaml"), "shop-staging")
	vars := envVars(t, c)

	want := map[string]string{"PORT": "8080", "NODE_ENV": "production", "LOG_LEVEL": "debug"}
	if len(vars) != len(want) {
		t.Errorf("env has %d entries %v, want %v", len(vars), vars, want)
	}
	for name, value := range want {
		if vars[name] != value {
			t.Errorf("env %s = %q, want %q", name, vars[name], value)
		}
	}
}

func TestServiceIsClusterIPOnThePort(t *testing.T) {
	objects := render(t, "staging-medium.yaml")
	svc, ok := objects["Service/shop-staging"]
	if !ok {
		t.Fatalf("no Service/shop-staging rendered; got %v", keys(objects))
	}

	if typ := get[string](t, svc, "spec", "type"); typ != "ClusterIP" {
		t.Errorf("type = %q, want ClusterIP", typ)
	}
	ports := get[[]any](t, svc, "spec", "ports")
	if len(ports) != 1 {
		t.Fatalf("Service has %d ports, want 1", len(ports))
	}
	if port := get[int](t, ports[0], "port"); port != 8080 {
		t.Errorf("port = %d, want 8080", port)
	}
	if target := get[string](t, ports[0], "targetPort"); target != "http" {
		t.Errorf("targetPort = %q, want the named http port", target)
	}

	// The Service must select exactly the Pods the Deployment creates.
	podLabels := get[map[string]any](t, objects["Deployment/shop-staging"], "spec", "template", "metadata", "labels")
	for key, value := range get[map[string]any](t, svc, "spec", "selector") {
		if podLabels[key] != value {
			t.Errorf("selector %s=%v does not match pod label %v", key, value, podLabels[key])
		}
	}
}

func TestIngressHostFollowsTheEnvironment(t *testing.T) {
	cases := []struct {
		fixture, name, host, tlsSecret string
		port                           int
	}{
		{"prod-small.yaml", "shop", "shop.app.itma.no", "wildcard-tls", 3000},
		{"staging-medium.yaml", "shop-staging", "shop-staging.app.itma.no", "wildcard-tls", 8080},
		{"prod-large.yaml", "warehouse", "warehouse.app.itma.no", "platform-wildcard-tls", 3000},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			objects := render(t, tc.fixture)
			ing, ok := objects["Ingress/"+tc.name]
			if !ok {
				t.Fatalf("no Ingress/%s rendered; got %v", tc.name, keys(objects))
			}

			if class := get[string](t, ing, "spec", "ingressClassName"); class != "traefik" {
				t.Errorf("ingressClassName = %q, want traefik", class)
			}
			annotations := get[map[string]any](t, ing, "metadata", "annotations")
			if got := annotations["traefik.ingress.kubernetes.io/router.entrypoints"]; got != "websecure" {
				t.Errorf("router.entrypoints = %v, want websecure", got)
			}
			if got := annotations["traefik.ingress.kubernetes.io/router.tls"]; got != "true" {
				t.Errorf("router.tls = %v, want \"true\"", got)
			}

			rules := get[[]any](t, ing, "spec", "rules")
			if len(rules) != 1 {
				t.Fatalf("Ingress has %d rules, want 1", len(rules))
			}
			if host := get[string](t, rules[0], "host"); host != tc.host {
				t.Errorf("host = %q, want %q", host, tc.host)
			}
			paths := get[[]any](t, rules[0], "http", "paths")
			if len(paths) != 1 {
				t.Fatalf("rule has %d paths, want 1", len(paths))
			}
			if path := get[string](t, paths[0], "path"); path != "/" {
				t.Errorf("path = %q, want /", path)
			}
			if pathType := get[string](t, paths[0], "pathType"); pathType != "Prefix" {
				t.Errorf("pathType = %q, want Prefix", pathType)
			}
			if svc := get[string](t, paths[0], "backend", "service", "name"); svc != tc.name {
				t.Errorf("backend service = %q, want %q", svc, tc.name)
			}
			if port := get[int](t, paths[0], "backend", "service", "port", "number"); port != tc.port {
				t.Errorf("backend port = %d, want %d", port, tc.port)
			}

			tls := get[[]any](t, ing, "spec", "tls")
			if len(tls) != 1 {
				t.Fatalf("Ingress has %d tls entries, want 1", len(tls))
			}
			if secret := get[string](t, tls[0], "secretName"); secret != tc.tlsSecret {
				t.Errorf("tls secretName = %q, want %q", secret, tc.tlsSecret)
			}
			if hosts := get[[]any](t, tls[0], "hosts"); len(hosts) != 1 || hosts[0] != tc.host {
				t.Errorf("tls hosts = %v, want [%s]", hosts, tc.host)
			}
		})
	}
}

func TestEveryObjectIsLabelledWithApplicationAndEnvironment(t *testing.T) {
	cases := []struct {
		fixture, application, environment, instance string
	}{
		{"prod-small.yaml", "shop", "prod", "shop"},
		{"staging-medium.yaml", "shop", "staging", "shop-staging"},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			objects := render(t, tc.fixture)
			if want := []string{"Deployment/" + tc.instance, "Ingress/" + tc.instance, "Service/" + tc.instance}; !equalStrings(keys(objects), want) {
				t.Fatalf("rendered %v, want exactly %v", keys(objects), want)
			}

			want := map[string]string{
				"app.kubernetes.io/name":     tc.application,
				"app.kubernetes.io/instance": tc.instance,
				"iidp.itema.no/application":  tc.application,
				"iidp.itema.no/environment":  tc.environment,
			}
			labelSets := map[string]map[string]any{}
			for key, obj := range objects {
				labelSets[key] = get[map[string]any](t, obj, "metadata", "labels")
			}
			labelSets["Pod template"] = get[map[string]any](t, objects["Deployment/"+tc.instance], "spec", "template", "metadata", "labels")

			for where, labels := range labelSets {
				for key, value := range want {
					if labels[key] != value {
						t.Errorf("%s: label %s = %v, want %q", where, key, labels[key], value)
					}
				}
			}
		})
	}
}

func TestRenderingRefusesInvalidValues(t *testing.T) {
	cases := []struct {
		fixture, message string
	}{
		{"unknown-size.yaml", `size must be one of large, medium, small, got "xlarge"`},
		{"unknown-kind.yaml", `kind must be web-service or static-site, got "cron-job"`},
		{"static-site.yaml", `kind static-site is not implemented yet; only web-service renders`},
		{"missing-name.yaml", `application.name is required`},
		{"bad-name.yaml", `application.name must be lowercase letters, digits and dashes, start with a letter and be at most 55 characters, got "1shop"`},
		{"env-sets-port.yaml", `env must not set PORT; it is injected from port`},
	}
	for _, tc := range cases {
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
}

// kubernetesVersion is the Kubernetes version the rendered manifests are
// validated against: the minor of the k3s release pinned by k3s_version in
// infra/platform/variables.tf. Bump it here when that changes.
const kubernetesVersion = "1.36.0"

func TestRenderedManifestsPassKubeconform(t *testing.T) {
	requireTool(t, "kubeconform")
	for _, fixture := range []string{"prod-small.yaml", "staging-medium.yaml", "prod-large.yaml"} {
		t.Run(fixture, func(t *testing.T) {
			manifests, err := helmTemplate(t, fixture)
			if err != nil {
				t.Fatalf("helm template %s: %v\n%s", fixture, err, manifests)
			}
			cmd := exec.Command("kubeconform", "-strict", "-summary", "-kubernetes-version", kubernetesVersion)
			cmd.Stdin = strings.NewReader(manifests)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("kubeconform: %v\n%s", err, out)
			}
			t.Logf("kubeconform: %s", strings.TrimSpace(string(out)))
		})
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
