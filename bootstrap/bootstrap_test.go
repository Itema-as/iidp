// Package bootstrap_test renders the app-of-apps with helm template, the
// way ArgoCD does, and asserts on the component Applications the Platform
// would receive: which ones exist, that every version is the pinned one,
// and that platform.yaml values land where they should.
package bootstrap_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// fixture is the Platform repository the kind harness serves; its
// platform.yaml is the values file here too, so both tests see one Platform.
const fixture = "../test/e2e/fixtures/platform-repo/platform.yaml"

type object = map[string]any

func TestRendersOneApplicationPerComponent(t *testing.T) {
	apps := renderApplications(t, "--values", fixture)
	var got []string
	for name := range apps {
		got = append(got, name)
	}
	sort.Strings(got)
	want := []string{"argocd", "cert-manager", "cloudnative-pg", "cnpg-barman-cloud", "external-dns", "monitoring", "oauth2-proxy", "platform-tls"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("rendered Applications = %v, want %v", got, want)
	}
	for name, app := range apps {
		if ns := get[string](t, app, "metadata", "namespace"); ns != "argocd" {
			t.Errorf("%s: namespace = %q, want argocd", name, ns)
		}
		if server := get[string](t, app, "spec", "destination", "server"); server != "https://kubernetes.default.svc" {
			t.Errorf("%s: destination server = %q", name, server)
		}
		get[object](t, app, "spec", "syncPolicy", "automated")
	}
}

func TestEveryChartVersionIsThePinnedOne(t *testing.T) {
	versions := readYAML(t, "versions.yaml")
	apps := renderApplications(t, "--values", fixture)
	pins := map[string][]string{
		"argocd":            {"argocd", "chart"},
		"cert-manager":      {"certManager", "chart"},
		"external-dns":      {"externalDNS", "chart"},
		"cloudnative-pg":    {"cloudnativePG", "chart"},
		"cnpg-barman-cloud": {"cloudnativePG", "barmanCloudPluginChart"},
		"monitoring":        {"k8sMonitoring", "chart"},
	}
	for name, path := range pins {
		want := get[string](t, versions, path...)
		got := get[string](t, apps[name], "spec", "source", "targetRevision")
		if got != want {
			t.Errorf("%s: targetRevision = %q, want %q from versions.yaml", name, got, want)
		}
		if chart := get[string](t, apps[name], "spec", "source", "chart"); chart == "" {
			t.Errorf("%s: no chart in source", name)
		}
	}
	image := get[string](t, versions, "ksops", "image")
	rendered := renderText(t, "--values", fixture)
	if !strings.Contains(rendered, "image: "+image) {
		t.Errorf("argocd Application does not install KSOPS from %s", image)
	}
}

func TestComponentsThatPointBackAtTheBootstrapFollowItsRevision(t *testing.T) {
	apps := renderApplications(t, "--values", fixture,
		"--set", "bootstrap.repoURL=https://example.test/iidp.git",
		"--set", "bootstrap.targetRevision=v9.9.9")
	tls := apps["platform-tls"]
	if got := get[string](t, tls, "spec", "source", "repoURL"); got != "https://example.test/iidp.git" {
		t.Errorf("platform-tls repoURL = %q", got)
	}
	if got := get[string](t, tls, "spec", "source", "targetRevision"); got != "v9.9.9" {
		t.Errorf("platform-tls targetRevision = %q", got)
	}
	if got := get[string](t, tls, "spec", "source", "path"); got != "bootstrap/components/tls" {
		t.Errorf("platform-tls path = %q", got)
	}
}

func TestPlatformValuesReachTheComponents(t *testing.T) {
	apps := renderApplications(t, "--values", fixture)
	values := func(app string) object {
		return get[object](t, apps[app], "spec", "source", "helm", "valuesObject")
	}

	argocd := values("argocd")
	if got := get[string](t, argocd, "global", "domain"); got != "argocd.app.example.test" {
		t.Errorf("argocd domain = %q", got)
	}
	dex := get[string](t, argocd, "configs", "cm", "dex.config")
	for _, want := range []string{"type: microsoft", "$argocd-entra:clientID", "$argocd-entra:clientSecret", "$argocd-entra:tenant", "redirectURI: https://argocd.app.example.test/api/dex/callback"} {
		if !strings.Contains(dex, want) {
			t.Errorf("dex.config lacks %q:\n%s", want, dex)
		}
	}
	if got := get[string](t, argocd, "configs", "rbac", "policy.csv"); !strings.Contains(got, "g, 00000000-0000-0000-0000-000000000003, role:admin") {
		t.Errorf("rbac policy.csv = %q", got)
	}
	if got := get[bool](t, argocd, "configs", "params", "server.insecure"); !got {
		t.Errorf("argocd server.insecure not set; Traefik terminates TLS")
	}

	dns := values("external-dns")
	if got := get[string](t, dns, "policy"); got != "sync" {
		t.Errorf("external-dns policy = %q", got)
	}
	if got := get[string](t, dns, "registry"); got != "txt" {
		t.Errorf("external-dns registry = %q", got)
	}
	if got := get[string](t, dns, "txtOwnerId"); got != "iidp" {
		t.Errorf("external-dns txtOwnerId = %q", got)
	}
	if got := fmt.Sprint(dns["domainFilters"]); got != "[example.test]" {
		t.Errorf("external-dns domainFilters = %s", got)
	}

	tls := values("platform-tls")
	if got := get[string](t, tls, "baseDomain"); got != "app.example.test" {
		t.Errorf("platform-tls baseDomain = %q", got)
	}
	if got := get[string](t, tls, "acme", "server"); got != "https://acme-staging-v02.api.letsencrypt.org/directory" {
		t.Errorf("platform-tls acme server = %q", got)
	}

	monitoring := values("monitoring")
	if got := get[string](t, monitoring, "cluster", "name"); got != "iidp-e2e" {
		t.Errorf("monitoring cluster name = %q", got)
	}
	for _, dest := range []string{"grafana-cloud-metrics", "grafana-cloud-logs"} {
		if got := get[string](t, monitoring, "destinations", dest, "secret", "name"); got != "grafana-cloud" {
			t.Errorf("monitoring %s secret = %q", dest, got)
		}
	}
}

func TestOauth2ProxyPointsAtThePinnedChartAndPlatformValues(t *testing.T) {
	versions := readYAML(t, "versions.yaml")
	apps := renderApplications(t, "--values", fixture,
		"--set", "bootstrap.repoURL=https://example.test/iidp.git",
		"--set", "bootstrap.targetRevision=v9.9.9")
	app := apps["oauth2-proxy"]
	sources := get[[]any](t, app, "spec", "sources")
	if len(sources) != 2 {
		t.Fatalf("oauth2-proxy has %d sources, want 2", len(sources))
	}
	chartSource := get[object](t, map[string]any{"s": sources[0]}, "s")
	if got := get[string](t, chartSource, "chart"); got != "oauth2-proxy" {
		t.Errorf("chart source chart = %q, want oauth2-proxy", got)
	}
	want := get[string](t, versions, "oauth2Proxy", "chart")
	if got := get[string](t, chartSource, "targetRevision"); got != want {
		t.Errorf("oauth2-proxy targetRevision = %q, want %q from versions.yaml", got, want)
	}

	middlewareSource := get[object](t, map[string]any{"s": sources[1]}, "s")
	if got := get[string](t, middlewareSource, "repoURL"); got != "https://example.test/iidp.git" {
		t.Errorf("middleware source repoURL = %q, want it to follow the bootstrap pin", got)
	}
	if got := get[string](t, middlewareSource, "targetRevision"); got != "v9.9.9" {
		t.Errorf("middleware source targetRevision = %q, want it to follow the bootstrap pin", got)
	}
	if got := get[string](t, middlewareSource, "path"); got != "bootstrap/components/oauth2-proxy-login" {
		t.Errorf("middleware source path = %q", got)
	}

	values := get[object](t, chartSource, "helm", "valuesObject")
	if got := get[string](t, values, "extraArgs", "cookie-domain"); got != ".app.example.test" {
		t.Errorf("cookie-domain = %q, want .app.example.test", got)
	}
	if got := get[string](t, values, "extraArgs", "whitelist-domain"); got != ".app.example.test" {
		t.Errorf("whitelist-domain = %q, want .app.example.test", got)
	}
	if got := get[string](t, values, "extraArgs", "redirect-url"); got != "https://auth.app.example.test/oauth2/callback" {
		t.Errorf("redirect-url = %q", got)
	}
	if got := fmt.Sprint(get[[]any](t, values, "ingress", "hosts")); got != "[auth.app.example.test]" {
		t.Errorf("ingress hosts = %s, want [auth.app.example.test]", got)
	}
	provider := get[[]any](t, values, "alphaConfig", "configData", "providers")[0]
	providerMap := get[object](t, map[string]any{"p": provider}, "p")
	if got := get[string](t, providerMap, "provider"); got != "entra-id" {
		t.Errorf("provider = %q, want entra-id", got)
	}
}

func TestTLSComponentRendersTheWildcardIntoTraefiksNamespace(t *testing.T) {
	objects := parseObjects(t, helmTemplate(t, "components/tls", "--set", "baseDomain=app.example.test"))
	cert, ok := objects["Certificate/wildcard"]
	if !ok {
		t.Fatalf("no Certificate/wildcard in %v", keys(objects))
	}
	if got := get[string](t, cert, "metadata", "namespace"); got != "kube-system" {
		t.Errorf("Certificate namespace = %q, want kube-system", got)
	}
	if got := get[string](t, cert, "spec", "secretName"); got != "wildcard-tls" {
		t.Errorf("Certificate secretName = %q", got)
	}
	if got := fmt.Sprint(get[[]any](t, cert, "spec", "dnsNames")); got != "[*.app.example.test app.example.test]" {
		t.Errorf("Certificate dnsNames = %s", got)
	}
	store, ok := objects["TLSStore/default"]
	if !ok {
		t.Fatalf("no TLSStore/default in %v", keys(objects))
	}
	if got := get[string](t, store, "spec", "defaultCertificate", "secretName"); got != "wildcard-tls" {
		t.Errorf("TLSStore secretName = %q", got)
	}
	if got := get[string](t, store, "metadata", "namespace"); got != "kube-system" {
		t.Errorf("TLSStore namespace = %q", got)
	}
	if _, ok := objects["ClusterIssuer/letsencrypt"]; !ok {
		t.Errorf("no ClusterIssuer/letsencrypt in %v", keys(objects))
	}
	// The application chart's platform.httpIssuer default; a custom domain
	// off Cloudflare gets its certificate through it.
	http01, ok := objects["ClusterIssuer/letsencrypt-http01"]
	if !ok {
		t.Fatalf("no ClusterIssuer/letsencrypt-http01 in %v", keys(objects))
	}
	if got := fmt.Sprint(get[[]any](t, http01, "spec", "acme", "solvers")); !strings.Contains(got, "traefik") {
		t.Errorf("letsencrypt-http01 solver does not use ingressClassName traefik: %s", got)
	}
	redirect, ok := objects["HelmChartConfig/traefik"]
	if !ok {
		t.Fatalf("no HelmChartConfig/traefik in %v", keys(objects))
	}
	if got := get[string](t, redirect, "spec", "valuesContent"); !strings.Contains(got, "to: websecure") {
		t.Errorf("HelmChartConfig does not redirect web to websecure:\n%s", got)
	}
}

// renderApplications renders the bootstrap and returns its Applications by
// name.
func renderApplications(t *testing.T, args ...string) map[string]object {
	t.Helper()
	apps := map[string]object{}
	for key, obj := range parseObjects(t, helmTemplate(t, ".", args...)) {
		if obj["kind"] != "Application" {
			t.Fatalf("the bootstrap rendered a %s; it should only render Applications", key)
		}
		apps[get[string](t, obj, "metadata", "name")] = obj
	}
	return apps
}

func renderText(t *testing.T, args ...string) string {
	t.Helper()
	return helmTemplate(t, ".", args...)
}

// helmTemplate renders a chart directory relative to this package.
func helmTemplate(t *testing.T, chart string, args ...string) string {
	t.Helper()
	requireHelm(t)
	cmd := exec.Command("helm", append([]string{"template", "test-release", chart}, args...)...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("helm template %s %v: %v\n%s", chart, args, err, out.String())
	}
	return out.String()
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

func readYAML(t *testing.T, path string) object {
	t.Helper()
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatal(err)
	}
	var obj object
	if err := yaml.Unmarshal(data, &obj); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return obj
}

// get walks nested maps and returns the leaf as T, failing when the path
// is missing or has another type.
func get[T any](t *testing.T, obj object, path ...string) T {
	t.Helper()
	var current any = obj
	for i, key := range path {
		m, ok := current.(object)
		if !ok {
			t.Fatalf("%s is not a map (looking for %s)", strings.Join(path[:i], "."), strings.Join(path, "."))
		}
		current, ok = m[key]
		if !ok {
			t.Fatalf("%s: missing", strings.Join(path[:i+1], "."))
		}
	}
	value, ok := current.(T)
	if !ok {
		var zero T
		t.Fatalf("%s: is %T, want %T", strings.Join(path, "."), current, zero)
	}
	return value
}

func keys(objects map[string]object) []string {
	var out []string
	for key := range objects {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// requireHelm skips when helm is missing, like the application chart's
// tests; CI sets IIDP_REQUIRE_CHART_TOOLS so that a missing helm fails.
func requireHelm(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		if os.Getenv("IIDP_REQUIRE_CHART_TOOLS") != "" {
			t.Fatal("helm not on PATH and IIDP_REQUIRE_CHART_TOOLS is set")
		}
		t.Skip("helm not on PATH")
	}
}

// TestVersionsAgreeWithCloudInit keeps the two places that must name the
// same ArgoCD and k3s release in step: cloud-init installs from
// infra/platform/variables.tf, the harness and this chart from
// versions.yaml.
func TestVersionsAgreeWithCloudInit(t *testing.T) {
	versions := readYAML(t, "versions.yaml")
	variables, err := os.ReadFile("../infra/platform/variables.tf")
	if err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string][]string{
		"argocd_version": {"argocd", "manifest"},
		"k3s_version":    {"k3s", "version"},
	} {
		want := tfDefault(t, string(variables), name)
		if got := get[string](t, versions, path...); got != want {
			t.Errorf("versions.yaml %s = %q, infra/platform/variables.tf %s default = %q", strings.Join(path, "."), got, name, want)
		}
	}
}

// tfDefault returns the default of a variable block in an OpenTofu file.
func tfDefault(t *testing.T, tf, name string) string {
	t.Helper()
	block := regexp.MustCompile(`(?s)variable "` + regexp.QuoteMeta(name) + `" \{.*?default\s*=\s*"([^"]+)"`)
	m := block.FindStringSubmatch(tf)
	if m == nil {
		t.Fatalf("no default for variable %q", name)
	}
	return m[1]
}
