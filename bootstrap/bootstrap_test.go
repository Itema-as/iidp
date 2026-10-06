// Package bootstrap_test renders the app-of-apps with helm template, the way
// ArgoCD does, and checks the component Applications it produces.
package bootstrap_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// fixture is the kind harness's platform.yaml, so both test suites see one
// Platform.
const fixture = "../test/e2e/fixtures/platform-repo/platform.yaml"

type object = map[string]any

func TestRendersOneApplicationPerComponent(t *testing.T) {
	apps := renderApplications(t, "--values", fixture)
	var got []string
	for name := range apps {
		got = append(got, name)
	}
	sort.Strings(got)
	want := []string{"argocd", "cert-manager", "cloudnative-pg", "cnpg-barman-cloud", "deploy-gate", "external-dns", "guardrails", "monitoring", "oauth2-proxy", "platform-tls"}
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

// A retry pinned to the revision that failed never picks up a fix pushed
// after it, because ArgoCD starts no new automated sync while one runs.
func TestEveryApplicationRetriesForeverAgainstTheNewestRevision(t *testing.T) {
	apps := renderApplications(t, "--values", fixture)
	if len(apps) == 0 {
		t.Fatal("the bootstrap rendered no Applications")
	}
	for name, app := range apps {
		retry := get[object](t, app, "spec", "syncPolicy", "retry")
		if limit, ok := retry["limit"].(int); !ok || limit != -1 {
			t.Errorf("%s: retry.limit = %v, want -1 (unlimited)", name, retry["limit"])
		}
		if refresh, ok := retry["refresh"].(bool); !ok || !refresh {
			t.Errorf("%s: retry.refresh = %v, want true", name, retry["refresh"])
		}
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
	// Preview Environments are ApplicationSets.
	if got := get[int](t, argocd, "applicationSet", "replicas"); got != 1 {
		t.Errorf("argocd applicationSet.replicas = %d, want 1", got)
	}
	if got := fmt.Sprint(get[object](t, argocd, "applicationSet", "resources", "requests")); got != "map[cpu:10m memory:64Mi]" {
		t.Errorf("argocd applicationSet requests = %s, want 10m CPU and 64Mi", got)
	}
	if got := get[string](t, argocd, "applicationSet", "resources", "limits", "memory"); got != "256Mi" {
		t.Errorf("argocd applicationSet memory limit = %q, want 256Mi", got)
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
	if len(sources) != 3 {
		t.Fatalf("oauth2-proxy has %d sources, want 3: the shared proxy, the Middlewares and the host-only proxy", len(sources))
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
	// The fixture's cloudflareZone. TestOauth2ProxyCookieDomain covers the
	// rest.
	if got := get[string](t, values, "extraArgs", "cookie-domain"); got != ".example.test" {
		t.Errorf("cookie-domain = %q, want .example.test", got)
	}
	if got := get[string](t, values, "extraArgs", "whitelist-domain"); got != ".example.test" {
		t.Errorf("whitelist-domain = %q, want .example.test", got)
	}
	if got := get[string](t, values, "extraArgs", "redirect-url"); got != "https://auth.app.example.test/oauth2/callback" {
		t.Errorf("redirect-url = %q", got)
	}
	// Keeps the cookie under an nginx Static site's 8 KB header limit.
	if got := get[string](t, values, "extraArgs", "session-cookie-minimal"); got != "true" {
		t.Errorf("session-cookie-minimal = %q, want true", got)
	}
	// An unauthenticated browser goes straight to Entra ID.
	if got := get[string](t, values, "extraArgs", "skip-provider-button"); got != "true" {
		t.Errorf("skip-provider-button = %q, want true", got)
	}
	if got := get[string](t, values, "extraArgs", "footer"); got != "-" {
		t.Errorf("footer = %q, want - (no oauth2-proxy version on its pages)", got)
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

// The callback stays on auth.<baseDomain> whatever the cookie domain, so the
// Entra app registration never changes.
func TestOauth2ProxyCookieDomain(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"cloudflareZone set", []string{"--set", "baseDomain=app.itma.no", "--set", "cloudflareZone=itma.no"}, ".itma.no"},
		{"cloudflareZone is baseDomain", []string{"--set", "baseDomain=itma.no", "--set", "cloudflareZone=itma.no"}, ".itma.no"},
		{"no cloudflareZone", []string{"--set", "baseDomain=app.itma.no", "--set", "cloudflareZone="}, ".app.itma.no"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			apps := renderApplications(t, append([]string{"--values", fixture}, tc.args...)...)
			sources := get[[]any](t, apps["oauth2-proxy"], "spec", "sources")
			values := get[object](t, map[string]any{"s": sources[0]}, "s", "helm", "valuesObject")
			for _, arg := range []string{"cookie-domain", "whitelist-domain"} {
				if got := get[string](t, values, "extraArgs", arg); got != tc.want {
					t.Errorf("%s = %q, want %q", arg, got, tc.want)
				}
			}
			if got, want := get[string](t, values, "extraArgs", "redirect-url"), "https://auth."+strings.TrimPrefix(tc.args[1], "baseDomain=")+"/oauth2/callback"; got != want {
				t.Errorf("redirect-url = %q, want %q", got, want)
			}
			if got := get[string](t, values, "extraArgs", "cookie-name"); got != "__Secure-itema_login" {
				t.Errorf("cookie-name = %q, want __Secure-itema_login", got)
			}
		})
	}

	// A cookie for the zone set from auth.<baseDomain> is only accepted if
	// that host is inside the zone.
	requireHelm(t)
	out, err := exec.Command("helm", "template", "t", ".", "--values", fixture, "--set", "baseDomain=app.itma.no", "--set", "cloudflareZone=example.com").CombinedOutput()
	if err == nil || !strings.Contains(string(out), `baseDomain "app.itma.no" must be inside cloudflareZone "example.com"`) {
		t.Errorf("rendering with baseDomain outside cloudflareZone: err = %v, output:\n%s\nwant a refusal naming both", err, out)
	}
}

// The shared proxy's values are pinned, so an edit to the values both
// proxies share cannot change it without this file changing too.
func TestSharedOauth2ProxyIsPinned(t *testing.T) {
	apps := renderApplications(t, "--values", fixture)
	sources := get[[]any](t, apps["oauth2-proxy"], "spec", "sources")
	got := get[object](t, map[string]any{"s": sources[0]}, "s", "helm")
	want := get[object](t, readYAML(t, "testdata/oauth2-proxy-shared-source.yaml"), "helm")
	if !reflect.DeepEqual(got, want) {
		gotYAML, _ := yaml.Marshal(got)
		t.Errorf("the shared oauth2-proxy's helm block differs from testdata/oauth2-proxy-shared-source.yaml; got:\n%s", gotYAML)
	}
}

// The host-only proxy serves custom domains outside the login cookie domain.
// It is the shared proxy apart from where the callback and the cookie live:
// both on the requested host. Everything else, the Entra Secret included,
// must stay the same.
func TestHostOnlyOauth2ProxyIsTheSharedOneWithHostOnlyCookies(t *testing.T) {
	versions := readYAML(t, "versions.yaml")
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"cloudflareZone set", nil},
		{"no cloudflareZone", []string{"--set", "cloudflareZone="}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			apps := renderApplications(t, append([]string{"--values", fixture}, tc.args...)...)
			sources := get[[]any](t, apps["oauth2-proxy"], "spec", "sources")
			if len(sources) != 3 {
				t.Fatalf("oauth2-proxy has %d sources, want 3", len(sources))
			}
			shared := get[object](t, map[string]any{"s": sources[0]}, "s")
			host := get[object](t, map[string]any{"s": sources[2]}, "s")
			for _, key := range []string{"repoURL", "chart"} {
				if got, want := get[string](t, host, key), get[string](t, shared, key); got != want {
					t.Errorf("host-only source %s = %q, want the shared proxy's %q", key, got, want)
				}
			}
			if got, want := get[string](t, host, "targetRevision"), get[string](t, versions, "oauth2Proxy", "chart"); got != want {
				t.Errorf("host-only targetRevision = %q, want %q from versions.yaml", got, want)
			}
			if got := get[string](t, host, "helm", "releaseName"); got != "oauth2-proxy-host" {
				t.Errorf("host-only releaseName = %q, want oauth2-proxy-host", got)
			}

			sharedValues := get[object](t, shared, "helm", "valuesObject")
			hostValues := get[object](t, host, "helm", "valuesObject")
			if got := get[string](t, hostValues, "fullnameOverride"); got != "oauth2-proxy-host" {
				t.Errorf("host-only fullnameOverride = %q, want oauth2-proxy-host", got)
			}
			if got := get[bool](t, hostValues, "ingress", "enabled"); got {
				t.Errorf("host-only ingress.enabled = true; its /oauth2/ routes come from each Environment")
			}
			hostArgs := get[object](t, hostValues, "extraArgs")
			// No redirect-url: the callback is built from the requested
			// host. No cookie-domain: the cookies are host-only.
			for _, arg := range []string{"redirect-url", "cookie-domain", "whitelist-domain"} {
				if value, set := hostArgs[arg]; set {
					t.Errorf("host-only extraArgs.%s = %v, want it unset", arg, value)
				}
			}
			if got := get[string](t, hostArgs, "cookie-name"); got != "__Host-itema_login" {
				t.Errorf("host-only cookie-name = %q, want __Host-itema_login", got)
			}

			// Everything else is the shared proxy's.
			sharedArgs := get[object](t, sharedValues, "extraArgs")
			for key, value := range sharedArgs {
				if key == "redirect-url" || key == "cookie-domain" || key == "whitelist-domain" || key == "cookie-name" {
					continue
				}
				if hostArgs[key] != value {
					t.Errorf("host-only extraArgs.%s = %v, want the shared proxy's %v", key, hostArgs[key], value)
				}
			}
			for key := range hostArgs {
				if _, ok := sharedArgs[key]; !ok && key != "cookie-name" {
					t.Errorf("host-only extraArgs.%s is set, the shared proxy has no such argument", key)
				}
			}
			for key, value := range sharedValues {
				if key == "fullnameOverride" || key == "ingress" || key == "extraArgs" {
					continue
				}
				if !reflect.DeepEqual(hostValues[key], value) {
					t.Errorf("host-only %s = %v, want the shared proxy's %v", key, hostValues[key], value)
				}
			}
			for key := range hostValues {
				if _, ok := sharedValues[key]; !ok {
					t.Errorf("host-only %s is set, the shared proxy has no such value", key)
				}
			}
		})
	}
}

// ForwardAuth checks oauth2-proxy's root address, whose answer to an
// unauthenticated browser is a redirect to Entra ID, not /oauth2/auth's bare
// 401.
func TestLoginMiddlewares(t *testing.T) {
	data, err := os.ReadFile("components/oauth2-proxy-login/middleware.yaml")
	if err != nil {
		t.Fatal(err)
	}
	middlewares := map[string]object{}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	for {
		var obj object
		if err := dec.Decode(&obj); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		middlewares[get[string](t, obj, "metadata", "name")] = obj
	}
	auth, ok := middlewares["itema-login-auth"]
	if !ok {
		t.Fatal("no itema-login-auth middleware")
	}
	if got := get[string](t, auth, "spec", "forwardAuth", "address"); got != "http://oauth2-proxy.oauth2-proxy.svc.cluster.local/" {
		t.Errorf("itema-login-auth address = %q, want oauth2-proxy's root address", got)
	}

	// The host-only proxy's Middleware is the same but for the address.
	host := readYAML(t, "components/oauth2-proxy-login/middleware-host.yaml")
	if got := get[string](t, host, "metadata", "namespace") + "/" + get[string](t, host, "metadata", "name"); got != "oauth2-proxy/itema-login-host-auth" {
		t.Errorf("host-only Middleware = %s, want oauth2-proxy/itema-login-host-auth", got)
	}
	want := get[object](t, auth, "spec", "forwardAuth")
	got := get[object](t, host, "spec", "forwardAuth")
	if address := get[string](t, got, "address"); address != "http://oauth2-proxy-host.oauth2-proxy.svc.cluster.local/" {
		t.Errorf("itema-login-host-auth address = %q, want the host-only proxy's root address", address)
	}
	for key, value := range want {
		if key != "address" && !reflect.DeepEqual(got[key], value) {
			t.Errorf("itema-login-host-auth forwardAuth.%s = %v, want itema-login-auth's %v", key, got[key], value)
		}
	}
	for key := range got {
		if _, ok := want[key]; !ok {
			t.Errorf("itema-login-host-auth forwardAuth.%s is set, itema-login-auth has no such field", key)
		}
	}
}

func TestDeployGateFollowsTheBootstrapAndThePlatformValues(t *testing.T) {
	apps := renderApplications(t, "--values", "values.yaml", "--set", "baseDomain=app.example.test", "--set", "cloudflareZone=example.test",
		"--set", "bootstrap.repoURL=https://example.test/iidp.git",
		"--set", "bootstrap.targetRevision=v9.9.9")
	gate := apps["deploy-gate"]
	for path, want := range map[string]string{
		"repoURL":        "https://example.test/iidp.git",
		"targetRevision": "v9.9.9",
		"path":           "bootstrap/components/deploy-gate",
	} {
		if got := get[string](t, gate, "spec", "source", path); got != want {
			t.Errorf("deploy-gate source %s = %q, want %q", path, got, want)
		}
	}
	// In argocd, where cloud-init writes the Secret holding the GitHub App's
	// key, which the gate mounts.
	if got := get[string](t, gate, "spec", "destination", "namespace"); got != "argocd" {
		t.Errorf("deploy-gate namespace = %q, want argocd", got)
	}
	values := get[object](t, gate, "spec", "source", "helm", "valuesObject")
	if got := get[string](t, values, "baseDomain"); got != "app.example.test" {
		t.Errorf("baseDomain = %q", got)
	}
	if got := get[string](t, values, "bootstrapRevision"); got != "v9.9.9" {
		t.Errorf("bootstrapRevision = %q, want the bootstrap pin", got)
	}
	if got := get[int](t, values, "githubOrgId"); got != 1230559 {
		t.Errorf("githubOrgId = %d, want Itema-as's 1230559", got)
	}
	if got := get[string](t, values, "oidc", "issuer"); got != "https://token.actions.githubusercontent.com" {
		t.Errorf("oidc.issuer = %q, want GitHub Actions'", got)
	}
	if got := get[string](t, values, "platformRepository"); got != "https://github.com/Itema-as/iidp-platform.git" {
		t.Errorf("platformRepository = %q", got)
	}
	if got := get[string](t, values, "appSecret"); got != "platform-repo-github-app" {
		t.Errorf("appSecret = %q, want ArgoCD's Platform-repository credential", got)
	}
	if got := get[string](t, values, "ghcrSecret"); got != "ghcr-pull-token" {
		t.Errorf("ghcrSecret = %q, want the GHCR pull token cloud-init writes", got)
	}
}

func TestDeployGateTakesTheFixturesStandIns(t *testing.T) {
	values := get[object](t, renderApplications(t, "--values", fixture)["deploy-gate"], "spec", "source", "helm", "valuesObject")
	if got := get[string](t, values, "oidc", "issuer"); !strings.Contains(got, "iidp-e2e.svc.cluster.local") {
		t.Errorf("oidc.issuer = %q, want the harness's fake issuer", got)
	}
	if got := get[string](t, values, "image", "tag"); got == "" {
		t.Errorf("image.tag is empty, want the harness's locally built image")
	}
}

func TestDeployGateComponentRendersTheGate(t *testing.T) {
	objects := parseObjects(t, helmTemplate(t, "components/deploy-gate",
		"--set", "baseDomain=app.example.test", "--set", "bootstrapRevision=v1.2.3"))

	deployment, ok := objects["Deployment/iidp-deploy-gate"]
	if !ok {
		t.Fatalf("no Deployment/iidp-deploy-gate in %v", keys(objects))
	}
	pod := get[object](t, deployment, "spec", "template", "spec")
	if got := get[string](t, pod, "serviceAccountName"); got != "iidp-deploy-gate" {
		t.Errorf("serviceAccountName = %q, want the gate's own read-only service account", got)
	}
	if got := get[bool](t, pod, "automountServiceAccountToken"); !got {
		t.Errorf("the gate mounts no service account token; iidp app status reads the cluster with it")
	}
	container := get[object](t, map[string]any{"c": get[[]any](t, pod, "containers")[0]}, "c")
	if got := get[string](t, container, "image"); got != "ghcr.io/itema-as/iidp-deploy-gate:1.2.3" {
		t.Errorf("image = %q, want the bootstrap release's version", got)
	}
	env := map[string]string{}
	for _, e := range get[[]any](t, container, "env") {
		m := e.(object)
		env[m["name"].(string)] = fmt.Sprint(m["value"])
	}
	for name, want := range map[string]string{
		"IIDP_GATE_AUDIENCE":      "https://deploy.app.example.test",
		"IIDP_GATE_ORG_ID":        "1230559",
		"IIDP_GATE_OIDC_ISSUER":   "https://token.actions.githubusercontent.com",
		"IIDP_GATE_PLATFORM_REPO": "https://github.com/Itema-as/iidp-platform.git",
		"IIDP_GATE_APP_DIR":       "/var/run/iidp-deploy-gate/app",
		"IIDP_GATE_GHCR_DIR":      "/var/run/iidp-deploy-gate/ghcr",
	} {
		if env[name] != want {
			t.Errorf("env %s = %q, want %q", name, env[name], want)
		}
	}
	if got := get[string](t, container, "resources", "requests", "cpu"); got != "5m" {
		t.Errorf("cpu request = %q, want 5m: the node and the kind runner are near their CPU request limits", got)
	}

	// Each Secret is mounted with only the keys the gate reads, at the
	// directory its environment variable names.
	mounts := map[string]string{}
	for _, m := range get[[]any](t, container, "volumeMounts") {
		mounts[m.(object)["name"].(string)] = fmt.Sprint(m.(object)["mountPath"])
	}
	for _, want := range []struct{ volume, secretName, keys, mountPath, why string }{
		{"app", "platform-repo-github-app", "githubAppID,githubAppInstallationID,githubAppPrivateKey", env["IIDP_GATE_APP_DIR"], "ArgoCD's Platform-repository credential: only the App's id, installation id and key"},
		{"ghcr", "ghcr-pull-token", "username,token", env["IIDP_GATE_GHCR_DIR"], "the GHCR pull token cloud-init writes"},
	} {
		var secret object
		for _, v := range get[[]any](t, pod, "volumes") {
			if m := v.(object); m["name"] == want.volume {
				secret = get[object](t, m, "secret")
			}
		}
		if secret == nil {
			t.Errorf("no %s volume", want.volume)
			continue
		}
		if got := get[string](t, secret, "secretName"); got != want.secretName {
			t.Errorf("%s secretName = %q, want %s", want.volume, got, want.why)
		}
		if _, optional := secret["optional"]; optional {
			t.Errorf("the %s volume is optional; the gate must not start without it", want.volume)
		}
		var items []string
		for _, item := range get[[]any](t, secret, "items") {
			items = append(items, item.(object)["key"].(string))
		}
		if got := strings.Join(items, ","); got != want.keys {
			t.Errorf("%s secret items = %s, want %s: %s", want.volume, got, want.keys, want.why)
		}
		if mounts[want.volume] != want.mountPath {
			t.Errorf("the %s volume is mounted at %q, want %q", want.volume, mounts[want.volume], want.mountPath)
		}
	}

	ingress, ok := objects["Ingress/iidp-deploy-gate"]
	if !ok {
		t.Fatalf("no Ingress/iidp-deploy-gate in %v", keys(objects))
	}
	rule := get[object](t, map[string]any{"r": get[[]any](t, ingress, "spec", "rules")[0]}, "r")
	if got := get[string](t, rule, "host"); got != "deploy.app.example.test" {
		t.Errorf("Ingress host = %q, want deploy.<baseDomain>", got)
	}
	tls := get[object](t, map[string]any{"t": get[[]any](t, ingress, "spec", "tls")[0]}, "t")
	if _, has := tls["secretName"]; has {
		t.Errorf("the Ingress names a TLS secret; the wildcard comes from Traefik's default store")
	}
	if got := get[string](t, ingress, "metadata", "annotations", "traefik.ingress.kubernetes.io/router.entrypoints"); got != "websecure" {
		t.Errorf("entrypoints = %q", got)
	}
	if _, ok := objects["Service/iidp-deploy-gate"]; !ok {
		t.Errorf("no Service/iidp-deploy-gate in %v", keys(objects))
	}
}

// Only the five kinds iidp app status reads, the ArgoCD Applications only in
// the gate's own namespace, and bound to no one but the gate.
func TestDeployGateReadsTheClusterWithListOnly(t *testing.T) {
	objects := parseObjects(t, helmTemplate(t, "components/deploy-gate",
		"--namespace", "argocd", "--set", "baseDomain=app.example.test", "--set", "bootstrapRevision=v1.2.3"))

	readOnly := map[string]bool{"get": true, "list": true, "watch": true}
	granted := map[string]string{} // group/resource -> the kind of role granting it
	var rbac []string
	for key, obj := range objects {
		kind := obj["kind"].(string)
		switch kind {
		case "Role", "ClusterRole":
			rbac = append(rbac, key)
			if key == "Role/iidp-deploy-gate-refresh" || key == "Role/iidp-deploy-gate-events" {
				continue // TestDeployGateOnlyWritesTheRefreshAndEventsInArgoCD
			}
			for _, r := range get[[]any](t, obj, "rules") {
				rule := r.(object)
				if _, ok := rule["nonResourceURLs"]; ok {
					t.Errorf("%s grants nonResourceURLs: %v", key, rule)
				}
				for _, v := range rule["verbs"].([]any) {
					if !readOnly[v.(string)] {
						t.Errorf("%s grants %q: only get, list and watch are allowed", key, v)
					}
					if v != "list" {
						t.Errorf("%s grants %q: iidp app status only lists", key, v)
					}
				}
				for _, g := range rule["apiGroups"].([]any) {
					for _, res := range rule["resources"].([]any) {
						granted[g.(string)+"/"+res.(string)] = kind
					}
				}
			}
		case "RoleBinding", "ClusterRoleBinding":
			rbac = append(rbac, key)
			subjects := get[[]any](t, obj, "subjects")
			if len(subjects) != 1 {
				t.Errorf("%s binds %d subjects, want only the gate's service account", key, len(subjects))
			}
			for _, s := range subjects {
				subject := s.(object)
				if subject["kind"] != "ServiceAccount" || subject["name"] != "iidp-deploy-gate" || subject["namespace"] != "argocd" {
					t.Errorf("%s binds %v, want argocd/iidp-deploy-gate", key, subject)
				}
			}
			roleKind := strings.TrimSuffix(kind, "Binding")
			ref := get[string](t, obj, "roleRef", "name")
			if get[string](t, obj, "roleRef", "kind") != roleKind {
				t.Errorf("%s refers to a %s, want a %s", key, get[string](t, obj, "roleRef", "kind"), roleKind)
			}
			if _, ok := objects[roleKind+"/"+ref]; !ok {
				t.Errorf("%s refers to %s/%s, which the chart does not render: it would grant whatever that role is", key, roleKind, ref)
			}
		}
	}
	want := map[string]string{
		"argoproj.io/applications": "Role",
		"/pods":                    "ClusterRole",
		"apps/deployments":         "ClusterRole",
		"batch/jobs":               "ClusterRole",
		"batch/cronjobs":           "ClusterRole",
	}
	if fmt.Sprint(granted) != fmt.Sprint(want) {
		t.Errorf("the gate may list %v, want exactly %v", granted, want)
	}
	sort.Strings(rbac)
	if got := strings.Join(rbac, " "); got != "ClusterRole/iidp-deploy-gate-status ClusterRoleBinding/iidp-deploy-gate-status Role/iidp-deploy-gate-events Role/iidp-deploy-gate-refresh Role/iidp-deploy-gate-status RoleBinding/iidp-deploy-gate-events RoleBinding/iidp-deploy-gate-refresh RoleBinding/iidp-deploy-gate-status" {
		t.Errorf("RBAC objects = %s", got)
	}
	if _, ok := objects["ServiceAccount/iidp-deploy-gate"]; !ok {
		t.Errorf("no ServiceAccount/iidp-deploy-gate in %v", keys(objects))
	}
}

// Patch on ArgoCD Applications and create on events.k8s.io events, each a
// Role of its own in argocd bound to the gate alone.
func TestDeployGateOnlyWritesTheRefreshAndEventsInArgoCD(t *testing.T) {
	objects := parseObjects(t, helmTemplate(t, "components/deploy-gate",
		"--namespace", "argocd", "--set", "baseDomain=app.example.test", "--set", "bootstrapRevision=v1.2.3"))

	var writes []string // kind/name in namespace: group/resource verb
	for key, obj := range objects {
		kind := obj["kind"].(string)
		if kind != "Role" && kind != "ClusterRole" {
			continue
		}
		// A Role with no namespace of its own is in the release's, argocd;
		// a ClusterRole's grants are everywhere.
		namespace, _ := get[object](t, obj, "metadata")["namespace"].(string)
		switch {
		case kind == "ClusterRole":
			namespace = "every namespace"
		case namespace == "":
			namespace = "argocd"
		}
		for _, r := range get[[]any](t, obj, "rules") {
			rule := r.(object)
			if _, ok := rule["resourceNames"]; ok {
				t.Errorf("%s narrows by resourceNames: %v; the Environments' names are not known here, and create cannot use them", key, rule)
			}
			for _, v := range rule["verbs"].([]any) {
				if v == "list" {
					continue
				}
				for _, g := range rule["apiGroups"].([]any) {
					for _, res := range rule["resources"].([]any) {
						writes = append(writes, fmt.Sprintf("%s in %s: %s/%s %s", key, namespace, g, res, v))
					}
				}
			}
		}
	}
	sort.Strings(writes)
	want := "Role/iidp-deploy-gate-events in argocd: events.k8s.io/events create; Role/iidp-deploy-gate-refresh in argocd: argoproj.io/applications patch"
	if got := strings.Join(writes, "; "); got != want {
		t.Errorf("the gate may do, beyond list: %s\nwant only: %s", got, want)
	}

	for name, rule := range map[string]object{
		"iidp-deploy-gate-refresh": {"apiGroups": []any{"argoproj.io"}, "resources": []any{"applications"}, "verbs": []any{"patch"}},
		"iidp-deploy-gate-events":  {"apiGroups": []any{"events.k8s.io"}, "resources": []any{"events"}, "verbs": []any{"create"}},
	} {
		role, ok := objects["Role/"+name]
		if !ok {
			t.Fatalf("no Role/%s in %v", name, keys(objects))
		}
		if got, want := fmt.Sprint(get[[]any](t, role, "rules")), fmt.Sprint([]any{rule}); got != want {
			t.Errorf("Role/%s rules = %s, want only %s", name, got, want)
		}
		binding, ok := objects["RoleBinding/"+name]
		if !ok {
			t.Fatalf("no RoleBinding/%s in %v", name, keys(objects))
		}
		if ns, _ := get[object](t, binding, "metadata")["namespace"].(string); ns != "" && ns != "argocd" {
			t.Errorf("RoleBinding/%s is in %q, want argocd", name, ns)
		}
		if get[string](t, binding, "roleRef", "kind") != "Role" || get[string](t, binding, "roleRef", "name") != name {
			t.Errorf("RoleBinding/%s refers to %v", name, binding["roleRef"])
		}
		subjects := get[[]any](t, binding, "subjects")
		if len(subjects) != 1 || fmt.Sprint(subjects[0]) != fmt.Sprint(object{"kind": "ServiceAccount", "name": "iidp-deploy-gate", "namespace": "argocd"}) {
			t.Errorf("RoleBinding/%s binds %v, want only argocd/iidp-deploy-gate", name, subjects)
		}
	}
}

func TestDeployGateImageTagCanBePinnedButNotGuessed(t *testing.T) {
	objects := parseObjects(t, helmTemplate(t, "components/deploy-gate",
		"--set", "bootstrapRevision=main", "--set", "image.tag=dev", "--set", "image.repository=iidp-e2e.local/deploy-gate"))
	container := get[[]any](t, objects["Deployment/iidp-deploy-gate"], "spec", "template", "spec", "containers")[0].(object)
	if got := container["image"]; got != "iidp-e2e.local/deploy-gate:dev" {
		t.Errorf("image = %v, want the pinned tag", got)
	}

	requireHelm(t)
	out, err := exec.Command("helm", "template", "t", "components/deploy-gate", "--set", "bootstrapRevision=main").CombinedOutput()
	if err == nil || !strings.Contains(string(out), "not a v* release tag") {
		t.Errorf("rendering with the bootstrap on a branch and no tag: err = %v, output:\n%s\nwant a refusal naming the release tag", err, out)
	}
}

func TestArgusIsOnByDefaultAndOffRendersNothing(t *testing.T) {
	args := []string{"--values", "values.yaml", "--set", "baseDomain=app.example.test", "--set", "cloudflareZone=example.test",
		"--set", "bootstrap.repoURL=https://example.test/iidp.git", "--set", "bootstrap.targetRevision=v9.9.9"}
	argus, ok := renderApplications(t, args...)["argus"]
	if !ok {
		t.Fatal("values.yaml renders no argus Application; Argus is on by default")
	}
	for path, want := range map[string]string{
		"repoURL":        "https://example.test/iidp.git",
		"targetRevision": "v9.9.9",
		"path":           "bootstrap/components/argus",
	} {
		if got := get[string](t, argus, "spec", "source", path); got != want {
			t.Errorf("argus source %s = %q, want %q", path, got, want)
		}
	}
	if got := get[string](t, argus, "spec", "destination", "namespace"); got != "argus" {
		t.Errorf("argus namespace = %q, want argus", got)
	}
	values := get[object](t, argus, "spec", "source", "helm", "valuesObject")
	if get[string](t, values, "baseDomain") != "app.example.test" || get[string](t, values, "bootstrapRevision") != "v9.9.9" ||
		get[string](t, values, "image", "repository") != "ghcr.io/itema-as/iidp-argus" {
		t.Errorf("argus values = %v", values)
	}
	links := get[object](t, values, "links")
	if fmt.Sprint(links) != "map[argocdURL:https://argocd.app.itma.no bootstrapRepository:https://example.test/iidp.git grafanaURL: platformRepository:https://github.com/Itema-as/iidp-platform.git]" {
		t.Errorf("argus links = %v", links)
	}
	withGrafana := renderApplications(t, append(append([]string{}, args...), "--set", "grafanaURL=https://itema.grafana.net")...)["argus"]
	if got := get[string](t, withGrafana, "spec", "source", "helm", "valuesObject", "links", "grafanaURL"); got != "https://itema.grafana.net" {
		t.Errorf("argus links.grafanaURL = %q, want platform.yaml's grafanaURL", got)
	}

	for _, off := range [][]string{
		append(append([]string{}, args...), "--set", "argus.enabled=false"),
		{"--values", fixture},
	} {
		rendered := renderText(t, off...)
		if strings.Contains(rendered, "argus") {
			t.Errorf("with Argus off (%v), the bootstrap still renders it:\n%s", off, rendered)
		}
	}
}

func TestArgusComponentRendersArgus(t *testing.T) {
	objects := parseObjects(t, helmTemplate(t, "components/argus", "--namespace", "argus",
		"--set", "baseDomain=app.example.test", "--set", "bootstrapRevision=v1.2.3",
		"--set", "links.argocdURL=https://argocd.app.example.test", "--set", "links.grafanaURL=https://itema.grafana.net",
		"--set", "links.platformRepository=https://github.com/Itema-as/iidp-platform.git", "--set", "links.bootstrapRepository=https://github.com/Itema-as/iidp.git"))
	if got := strings.Join(keys(objects), " "); got != "ClusterRole/iidp-argus ClusterRoleBinding/iidp-argus Deployment/iidp-argus Ingress/iidp-argus Service/iidp-argus ServiceAccount/iidp-argus" {
		t.Errorf("rendered %s", got)
	}

	deployment := objects["Deployment/iidp-argus"]
	if got := get[int](t, deployment, "spec", "replicas"); got != 1 {
		t.Errorf("replicas = %d, want 1: every stream is served from one process's memory", got)
	}
	pod := get[object](t, deployment, "spec", "template", "spec")
	if get[string](t, pod, "serviceAccountName") != "iidp-argus" || !get[bool](t, pod, "automountServiceAccountToken") {
		t.Errorf("pod service account = %v, token %v; want its own, mounted", pod["serviceAccountName"], pod["automountServiceAccountToken"])
	}
	if _, has := pod["volumes"]; has {
		t.Errorf("the pod mounts volumes %v; Argus needs none", pod["volumes"])
	}
	container := get[[]any](t, pod, "containers")[0].(object)
	if got := container["image"]; got != "ghcr.io/itema-as/iidp-argus:1.2.3" {
		t.Errorf("image = %v, want the bootstrap release's version", got)
	}
	resources := get[object](t, container, "resources")
	if got := fmt.Sprint(resources); got != "map[limits:map[memory:96Mi] requests:map[cpu:10m memory:48Mi]]" {
		t.Errorf("resources = %s, want requests cpu 10m and memory 48Mi, a memory limit of 96Mi and no CPU limit", got)
	}
	var env []string
	for _, e := range get[[]any](t, container, "env") {
		env = append(env, fmt.Sprint(e))
	}
	if got := strings.Join(env, "\n"); got != strings.Join([]string{
		"map[name:GOMEMLIMIT value:48MiB]",
		"map[name:IIDP_ARGUS_ARGOCD_URL value:https://argocd.app.example.test]",
		"map[name:IIDP_ARGUS_GRAFANA_URL value:https://itema.grafana.net]",
		"map[name:IIDP_ARGUS_PLATFORM_REPOSITORY value:https://github.com/Itema-as/iidp-platform.git]",
		"map[name:IIDP_ARGUS_BOOTSTRAP_REPOSITORY value:https://github.com/Itema-as/iidp.git]",
		"map[name:IIDP_ARGUS_BOOTSTRAP_REVISION value:v1.2.3]",
	}, "\n") {
		t.Errorf("env =\n%s\nwant GOMEMLIMIT 48MiB and the card's links", got)
	}
	if get[string](t, container, "readinessProbe", "httpGet", "path") != "/readyz" || get[string](t, container, "livenessProbe", "httpGet", "path") != "/healthz" {
		t.Errorf("probes = %v, %v", container["readinessProbe"], container["livenessProbe"])
	}
	if !get[bool](t, container, "securityContext", "readOnlyRootFilesystem") || get[bool](t, container, "securityContext", "allowPrivilegeEscalation") {
		t.Errorf("container securityContext = %v", container["securityContext"])
	}

	// Traefik names the middleware <namespace>-<name>@kubernetescrd.
	ingress := objects["Ingress/iidp-argus"]
	rule := get[[]any](t, ingress, "spec", "rules")[0].(object)
	if got := get[string](t, rule, "host"); got != "argus.app.example.test" {
		t.Errorf("Ingress host = %q, want argus.<baseDomain>", got)
	}
	middleware := readYAML(t, "components/oauth2-proxy-login/middleware.yaml")
	login := get[string](t, middleware, "metadata", "namespace") + "-" + get[string](t, middleware, "metadata", "name") + "@kubernetescrd"
	if got := get[string](t, ingress, "metadata", "annotations", "traefik.ingress.kubernetes.io/router.middlewares"); got != login {
		t.Errorf("Ingress middlewares = %q, want the Itema login middleware %q", got, login)
	}
	tls := get[[]any](t, ingress, "spec", "tls")[0].(object)
	if _, has := tls["secretName"]; has {
		t.Errorf("the Ingress names a TLS secret; the wildcard comes from Traefik's default store")
	}
	if got := get[string](t, ingress, "metadata", "annotations", "traefik.ingress.kubernetes.io/router.entrypoints"); got != "websecure" {
		t.Errorf("entrypoints = %q", got)
	}
}

// Exactly the kinds Argus watches, no narrowing by name, and bound to no one
// but Argus.
func TestArgusReadsTheClusterWithGetListWatchOnly(t *testing.T) {
	objects := parseObjects(t, helmTemplate(t, "components/argus", "--namespace", "argus",
		"--set", "baseDomain=app.example.test", "--set", "bootstrapRevision=v1.2.3"))
	var grants []string
	for key, obj := range objects {
		kind := obj["kind"].(string)
		if kind == "Role" || kind == "RoleBinding" {
			t.Errorf("%s: Argus's reads are one ClusterRole", key)
		}
		if kind != "ClusterRole" {
			continue
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
					grants = append(grants, fmt.Sprintf("%s/%s %s", g, res, strings.Join(verbs, ",")))
				}
			}
		}
	}
	sort.Strings(grants)
	want := []string{
		"/events get,list,watch",
		"/nodes get,list,watch",
		"/pods get,list,watch",
		"apps/deployments get,list,watch",
		"argoproj.io/applications get,list,watch",
		"batch/cronjobs get,list,watch",
		"batch/jobs get,list,watch",
		"cert-manager.io/certificates get,list,watch",
		"networking.k8s.io/ingresses get,list,watch",
		"postgresql.cnpg.io/clusters get,list,watch",
	}
	if strings.Join(grants, "\n") != strings.Join(want, "\n") {
		t.Errorf("Argus may:\n%s\nwant exactly:\n%s", strings.Join(grants, "\n"), strings.Join(want, "\n"))
	}

	binding := objects["ClusterRoleBinding/iidp-argus"]
	if get[string](t, binding, "roleRef", "kind") != "ClusterRole" || get[string](t, binding, "roleRef", "name") != "iidp-argus" {
		t.Errorf("binding refers to %v", binding["roleRef"])
	}
	subjects := get[[]any](t, binding, "subjects")
	if len(subjects) != 1 || fmt.Sprint(subjects[0]) != fmt.Sprint(object{"kind": "ServiceAccount", "name": "iidp-argus", "namespace": "argus"}) {
		t.Errorf("binding binds %v, want only argus/iidp-argus", subjects)
	}
	if get[bool](t, objects["ServiceAccount/iidp-argus"], "automountServiceAccountToken") {
		t.Errorf("the ServiceAccount mounts its token everywhere; only Argus's pod should")
	}
}

func TestArgusImageTagCanBePinnedButNotGuessed(t *testing.T) {
	objects := parseObjects(t, helmTemplate(t, "components/argus",
		"--set", "bootstrapRevision=main", "--set", "image.tag=dev", "--set", "image.repository=iidp-e2e.local/argus"))
	container := get[[]any](t, objects["Deployment/iidp-argus"], "spec", "template", "spec", "containers")[0].(object)
	if got := container["image"]; got != "iidp-e2e.local/argus:dev" {
		t.Errorf("image = %v, want the pinned tag", got)
	}

	requireHelm(t)
	out, err := exec.Command("helm", "template", "t", "components/argus", "--set", "bootstrapRevision=main").CombinedOutput()
	if err == nil || !strings.Contains(string(out), "not a v* release tag") {
		t.Errorf("rendering with the bootstrap on a branch and no tag: err = %v, output:\n%s\nwant a refusal naming the release tag", err, out)
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
	// The application chart's platform.httpIssuer default.
	http01, ok := objects["ClusterIssuer/letsencrypt-http01"]
	if !ok {
		t.Fatalf("no ClusterIssuer/letsencrypt-http01 in %v", keys(objects))
	}
	solver := get[object](t, get[[]any](t, http01, "spec", "acme", "solvers")[0].(object), "http01", "ingress")
	if got := get[string](t, solver, "ingressClassName"); got != "traefik" {
		t.Errorf("letsencrypt-http01 solver ingressClassName = %q, want traefik", got)
	}
	// The guardrails refuse NodePort, cert-manager's default.
	if got := get[string](t, solver, "serviceType"); got != "ClusterIP" {
		t.Errorf("letsencrypt-http01 solver serviceType = %q, want ClusterIP", got)
	}
	redirect, ok := objects["HelmChartConfig/traefik"]
	if !ok {
		t.Fatalf("no HelmChartConfig/traefik in %v", keys(objects))
	}
	if got := get[string](t, redirect, "spec", "valuesContent"); !strings.Contains(got, "to: websecure") {
		t.Errorf("HelmChartConfig does not redirect web to websecure:\n%s", got)
	}
}

// renderApplications returns the bootstrap's Applications by name.
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

// get walks nested maps and returns the leaf as T.
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

// requireHelm skips when helm is missing, unless IIDP_REQUIRE_CHART_TOOLS
// is set.
func requireHelm(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		if os.Getenv("IIDP_REQUIRE_CHART_TOOLS") != "" {
			t.Fatal("helm not on PATH and IIDP_REQUIRE_CHART_TOOLS is set")
		}
		t.Skip("helm not on PATH")
	}
}

// TestVersionsAgreeWithCloudInit checks that infra/platform/variables.tf,
// which cloud-init installs from, names the same versions as versions.yaml.
func TestVersionsAgreeWithCloudInit(t *testing.T) {
	versions := readYAML(t, "versions.yaml")
	variables, err := os.ReadFile("../infra/platform/variables.tf")
	if err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string][]string{
		"argocd_chart_version": {"argocd", "chart"},
		"helm_version":         {"helm", "version"},
		"k3s_version":          {"k3s", "version"},
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
