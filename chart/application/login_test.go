package application_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"reflect"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const loginMiddlewareAnnotation = "oauth2-proxy-itema-login-auth@kubernetescrd"

func TestLoginAddsTheForwardAuthMiddlewareAnnotation(t *testing.T) {
	ing := mustObject(t, render(t, "login-enabled.yaml"), "Ingress/shop")
	annotations := get[map[string]any](t, ing, "metadata", "annotations")
	if got := annotations["traefik.ingress.kubernetes.io/router.middlewares"]; got != loginMiddlewareAnnotation {
		t.Errorf("router.middlewares = %v, want %s", got, loginMiddlewareAnnotation)
	}
}

// Custom domains inside the login cookie domain are protected on whichever
// Ingress they land, and the HTTP-01 one keeps its issuer annotation.
func TestLoginProtectsCustomDomainsInsideTheCookieDomain(t *testing.T) {
	objects := render(t, "login-custom-domains.yaml")
	for _, tc := range []struct {
		ingress string
		hosts   []string
	}{
		{"Ingress/shop", []string{"shop.app.itma.no", "butikk.app.itma.no"}},
		{"Ingress/shop-http01", []string{"x.itma.no", "test.shop.app.itma.no", "itma.no"}},
	} {
		ing := mustObject(t, objects, tc.ingress)
		if hosts := ingressHosts(t, ing); !slices.Equal(hosts, tc.hosts) {
			t.Errorf("%s hosts = %v, want %v", tc.ingress, hosts, tc.hosts)
		}
		annotations := get[map[string]any](t, ing, "metadata", "annotations")
		if got := annotations["traefik.ingress.kubernetes.io/router.middlewares"]; got != loginMiddlewareAnnotation {
			t.Errorf("%s router.middlewares = %v, want %s", tc.ingress, got, loginMiddlewareAnnotation)
		}
	}
	http01 := get[map[string]any](t, mustObject(t, objects, "Ingress/shop-http01"), "metadata", "annotations")
	if got := http01["cert-manager.io/cluster-issuer"]; got != "letsencrypt-http01" {
		t.Errorf("shop-http01 cert-manager.io/cluster-issuer = %v, want letsencrypt-http01", got)
	}
}

// Without platform.loginCookieDomain the cookie domain is the base domain.
func TestLoginCookieDomainDefaultsToTheBaseDomain(t *testing.T) {
	objects := render(t, "login-custom-domains.yaml", "--set", "platform.loginCookieDomain=", "--set", "domains={butikk.app.itma.no,test.shop.app.itma.no}")
	for _, name := range []string{"Ingress/shop", "Ingress/shop-http01"} {
		annotations := get[map[string]any](t, mustObject(t, objects, name), "metadata", "annotations")
		if got := annotations["traefik.ingress.kubernetes.io/router.middlewares"]; got != loginMiddlewareAnnotation {
			t.Errorf("%s router.middlewares = %v, want %s", name, got, loginMiddlewareAnnotation)
		}
	}
	// The zone's other hosts are outside app.itma.no, so behind the
	// host-only login.
	objects = render(t, "login-custom-domains.yaml", "--set", "platform.loginCookieDomain=")
	if hosts := ingressHosts(t, mustObject(t, objects, "Ingress/shop-host-login")); !slices.Equal(hosts, []string{"x.itma.no", "itma.no"}) {
		t.Errorf("shop-host-login hosts = %v, want [x.itma.no itma.no]", hosts)
	}
}

// Traefik refuses a router whose middleware does not exist, so a name out of
// step with the bootstrap would take the Application off the air.
func TestLoginMiddlewaresExistInTheBootstrap(t *testing.T) {
	defined := map[string]bool{}
	for _, file := range []string{"middleware.yaml", "middleware-host.yaml"} {
		data, err := os.ReadFile("../../bootstrap/components/oauth2-proxy-login/" + file)
		if err != nil {
			t.Fatal(err)
		}
		dec := yaml.NewDecoder(bytes.NewReader(data))
		for {
			var obj object
			if err := dec.Decode(&obj); errors.Is(err, io.EOF) {
				break
			} else if err != nil {
				t.Fatal(err)
			}
			defined[get[string](t, obj, "metadata", "namespace")+"-"+get[string](t, obj, "metadata", "name")+"@kubernetescrd"] = true
		}
	}
	for _, name := range []string{loginMiddlewareAnnotation, hostLoginMiddlewareAnnotation} {
		if !defined[name] {
			t.Errorf("the chart names %s, which bootstrap/components/oauth2-proxy-login does not define", name)
		}
	}
}

func TestLoginDisabledByDefaultLeavesNoMiddlewareAnnotation(t *testing.T) {
	ing := mustObject(t, render(t, "prod-small.yaml"), "Ingress/shop")
	annotations := get[map[string]any](t, ing, "metadata", "annotations")
	if _, set := annotations["traefik.ingress.kubernetes.io/router.middlewares"]; set {
		t.Errorf("router.middlewares = %v, want unset when login is disabled", annotations["traefik.ingress.kubernetes.io/router.middlewares"])
	}
}

func TestLoginDisabledLeavesTheHTTP01IngressWithoutMiddleware(t *testing.T) {
	ing := mustObject(t, render(t, "custom-domains-mixed.yaml"), "Ingress/shop-staging-http01")
	annotations := get[map[string]any](t, ing, "metadata", "annotations")
	if _, set := annotations["traefik.ingress.kubernetes.io/router.middlewares"]; set {
		t.Errorf("router.middlewares = %v, want unset when login is disabled", annotations["traefik.ingress.kubernetes.io/router.middlewares"])
	}
}

// The Platform address must be inside the cookie domain: a wrong hand-set
// value would otherwise lock it out too.
func TestLoginRefusesAPlatformAddressOutsideTheCookieDomain(t *testing.T) {
	out, err := helmTemplate(t, "refuse-login-platform-address-outside-cookie-domain.yaml")
	if err == nil {
		t.Fatalf("rendering succeeded, want a failure:\n%s", out)
	}
	want := `login.enabled needs the Platform address "shop.app.itma.no" inside platform.loginCookieDomain "example.com"`
	if !strings.Contains(out, want) {
		t.Fatalf("error does not say %q:\n%s", want, out)
	}
}

const (
	hostLoginMiddlewareAnnotation = "oauth2-proxy-itema-login-host-auth@kubernetescrd"
	hostLoginNamespace            = "shop"
	// The host-only proxy's address with the fixture's groups.
	hostLoginGroupsAddress = "http://oauth2-proxy-host.oauth2-proxy.svc.cluster.local/?allowed_groups=0f3b6a4e-8c1d-4e2f-9a7b-5c6d7e8f9a0b"
)

// middlewaresOf returns an Ingress's router.middlewares annotation, or "".
func middlewaresOf(t *testing.T, ing object) string {
	t.Helper()
	annotations := get[map[string]any](t, ing, "metadata", "annotations")
	value, _ := annotations["traefik.ingress.kubernetes.io/router.middlewares"].(string)
	return value
}

// Custom domains outside the cookie domain go behind the host-only proxy,
// on an Ingress of their own; every other host keeps the shared one.
func TestLoginOutsideTheCookieDomainUsesTheHostOnlyProxy(t *testing.T) {
	objects := render(t, "login-domains-outside-cookie-domain.yaml", "--namespace", hostLoginNamespace)

	for _, tc := range []struct {
		ingress, middleware string
		hosts               []string
	}{
		{"Ingress/shop", loginMiddlewareAnnotation, []string{"shop.app.itma.no", "butikk.app.itma.no"}},
		{"Ingress/shop-http01", loginMiddlewareAnnotation, []string{"x.itma.no", "shop.example.com", "notitma.no"}},
		{"Ingress/shop-host-login", hostLoginMiddlewareAnnotation, []string{"shop.example.com", "notitma.no"}},
	} {
		ing := mustObject(t, objects, tc.ingress)
		if hosts := ingressHosts(t, ing); !slices.Equal(hosts, tc.hosts) {
			t.Errorf("%s hosts = %v, want %v", tc.ingress, hosts, tc.hosts)
		}
		if got := middlewaresOf(t, ing); got != tc.middleware {
			t.Errorf("%s router.middlewares = %q, want %q", tc.ingress, got, tc.middleware)
		}
	}

	// The HTTP-01 Ingress still holds every certificate, so turning login
	// on or off never moves one between Ingresses. The hosts outside the
	// cookie domain are listed there without paths: their routes are on
	// shop-host-login, and Traefik finds their certificates by SNI.
	http01 := mustObject(t, objects, "Ingress/shop-http01")
	if got := get[string](t, http01, "metadata", "annotations", "cert-manager.io/cluster-issuer"); got != "letsencrypt-http01" {
		t.Errorf("shop-http01 cert-manager.io/cluster-issuer = %q, want letsencrypt-http01", got)
	}
	wantTLS := []tlsEntry{
		{[]string{"x.itma.no"}, "shop-x-itma-no-tls"},
		{[]string{"shop.example.com"}, "shop-shop-example-com-tls"},
		{[]string{"notitma.no"}, "shop-notitma-no-tls"},
	}
	if got := tlsEntries(t, http01); !reflect.DeepEqual(got, wantTLS) {
		t.Errorf("shop-http01 tls = %v, want %v", got, wantTLS)
	}
	for _, rule := range get[[]any](t, http01, "spec", "rules") {
		host := get[string](t, rule, "host")
		_, routed := rule.(map[string]any)["http"]
		if want := host == "x.itma.no"; routed != want {
			t.Errorf("shop-http01 rule %s has paths = %v, want %v", host, routed, want)
		}
	}

	hostLogin := mustObject(t, objects, "Ingress/shop-host-login")
	assertEveryRuleRoutesTo(t, hostLogin, "shop", 3000)
	annotations := get[map[string]any](t, hostLogin, "metadata", "annotations")
	if _, set := annotations["cert-manager.io/cluster-issuer"]; set {
		t.Errorf("shop-host-login has an issuer annotation; its certificates come from shop-http01")
	}
	if got := annotations["traefik.ingress.kubernetes.io/router.entrypoints"]; got != "websecure" {
		t.Errorf("shop-host-login router.entrypoints = %v, want websecure", got)
	}
	for _, entry := range tlsEntries(t, hostLogin) {
		if entry.secretName != "" {
			t.Errorf("shop-host-login names TLS secret %s; its certificates come from shop-http01", entry.secretName)
		}
	}
}

// The sign-in callback on a host outside the cookie domain goes to the
// host-only proxy, not through ForwardAuth, from an Ingress in the proxy's
// own namespace: Traefik routes an Ingress only to Services in its
// namespace.
func TestLoginOutsideTheCookieDomainRoutesTheCallbackToTheHostOnlyProxy(t *testing.T) {
	objects := render(t, "login-domains-outside-cookie-domain.yaml", "--namespace", hostLoginNamespace)
	ing := mustObject(t, objects, "Ingress/shop-shop-oauth2")
	if got := get[string](t, ing, "metadata", "namespace"); got != "oauth2-proxy" {
		t.Errorf("namespace = %q, want oauth2-proxy", got)
	}
	if hosts := ingressHosts(t, ing); !slices.Equal(hosts, []string{"shop.example.com", "notitma.no"}) {
		t.Errorf("hosts = %v, want the hosts outside the cookie domain", hosts)
	}
	annotations := get[map[string]any](t, ing, "metadata", "annotations")
	if got := annotations["traefik.ingress.kubernetes.io/router.middlewares"]; got != nil {
		t.Errorf("router.middlewares = %v, want none: the callback must not go through ForwardAuth", got)
	}
	if got := annotations["traefik.ingress.kubernetes.io/router.entrypoints"]; got != "websecure" {
		t.Errorf("router.entrypoints = %v, want websecure", got)
	}
	for _, rule := range get[[]any](t, ing, "spec", "rules") {
		paths := get[[]any](t, rule, "http", "paths")
		if len(paths) != 1 {
			t.Fatalf("rule %s has %d paths, want 1", get[string](t, rule, "host"), len(paths))
		}
		// Longer than the Application's PathPrefix(`/`), so Traefik
		// prefers it for /oauth2/.
		if got := get[string](t, paths[0], "path"); got != "/oauth2/" {
			t.Errorf("path = %q, want /oauth2/", got)
		}
		if got := get[string](t, paths[0], "pathType"); got != "Prefix" {
			t.Errorf("pathType = %q, want Prefix", got)
		}
		if got := get[string](t, paths[0], "backend", "service", "name"); got != "oauth2-proxy-host" {
			t.Errorf("backend = %q, want oauth2-proxy-host", got)
		}
		if got := get[string](t, paths[0], "backend", "service", "port", "name"); got != "http" {
			t.Errorf("backend port = %q, want http", got)
		}
	}
	for _, entry := range tlsEntries(t, ing) {
		if entry.secretName != "" {
			t.Errorf("names TLS secret %s, which is not in the oauth2-proxy namespace", entry.secretName)
		}
	}

	// The name carries the Environment's namespace, so two Environments
	// never write the same object.
	other := render(t, "login-domains-outside-cookie-domain.yaml", "--namespace", "other")
	mustObject(t, other, "Ingress/other-shop-oauth2")
}

// With every host inside the cookie domain, or without login, nothing of
// the host-only proxy renders.
func TestHostOnlyLoginRendersOnlyForHostsOutsideTheCookieDomain(t *testing.T) {
	for _, tc := range []struct {
		fixture string
		args    []string
	}{
		{"login-custom-domains.yaml", nil},
		{"login-groups.yaml", nil},
		{"login-domains-outside-cookie-domain.yaml", []string{"--set", "login.enabled=false"}},
	} {
		t.Run(tc.fixture+" "+strings.Join(tc.args, " "), func(t *testing.T) {
			for key, obj := range render(t, tc.fixture, tc.args...) {
				if strings.HasSuffix(key, "-host-login") || strings.HasSuffix(key, "-oauth2") || strings.HasSuffix(key, "-itema-login-host") {
					t.Errorf("rendered %s", key)
				}
				if strings.HasPrefix(key, "Ingress/") && strings.Contains(middlewaresOf(t, obj), "host") {
					t.Errorf("%s names %s", key, middlewaresOf(t, obj))
				}
			}
		})
	}
	// Without login the hosts outside the cookie domain are served like
	// any other custom domain.
	objects := render(t, "login-domains-outside-cookie-domain.yaml", "--set", "login.enabled=false")
	assertEveryRuleRoutesTo(t, mustObject(t, objects, "Ingress/shop-http01"), "shop", 3000)
}

// With sign-in groups, the hosts outside the cookie domain get the
// Environment's own copy of the host-only Middleware.
func TestLoginGroupsOutsideTheCookieDomainUseTheirOwnHostOnlyMiddleware(t *testing.T) {
	objects := render(t, "login-domains-outside-cookie-domain.yaml", "--namespace", hostLoginNamespace,
		"--set-json", `login.groups=["0F3B6A4E-8C1D-4E2F-9A7B-5C6D7E8F9A0B"]`)
	mw := mustObject(t, objects, "Middleware/shop-itema-login-host")
	if got := get[string](t, mw, "spec", "forwardAuth", "address"); got != hostLoginGroupsAddress {
		t.Errorf("forwardAuth.address = %s, want %s", got, hostLoginGroupsAddress)
	}
	if got := get[string](t, mw, "metadata", "annotations", "argocd.argoproj.io/sync-wave"); got != "-1" {
		t.Errorf("sync-wave = %s, want -1, before the Ingress that names it", got)
	}
	for name, want := range map[string]string{
		"Ingress/shop":            "shop-shop-itema-login@kubernetescrd",
		"Ingress/shop-http01":     "shop-shop-itema-login@kubernetescrd",
		"Ingress/shop-host-login": "shop-shop-itema-login-host@kubernetescrd",
	} {
		if got := middlewaresOf(t, mustObject(t, objects, name)); got != want {
			t.Errorf("%s router.middlewares = %q, want %q", name, got, want)
		}
	}

	// The copy is the bootstrap's itema-login-host-auth with allowed_groups
	// added to its address and nothing else changed.
	data, err := os.ReadFile("../../bootstrap/components/oauth2-proxy-login/middleware-host.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var shared object
	if err := yaml.Unmarshal(data, &shared); err != nil {
		t.Fatal(err)
	}
	assertForwardAuthWithAllowedGroups(t, get[map[string]any](t, mw, "spec", "forwardAuth"), get[map[string]any](t, shared, "spec", "forwardAuth"))
}

func TestLoginFixturesPassKubeconform(t *testing.T) {
	requireTool(t, "kubeconform")
	version := kubernetesVersion(t)
	for _, fixture := range []string{"login-enabled.yaml", "login-custom-domains.yaml", "login-domains-outside-cookie-domain.yaml"} {
		t.Run(fixture, func(t *testing.T) {
			manifests, err := helmTemplate(t, fixture)
			if err != nil {
				t.Fatalf("helm template %s: %v\n%s", fixture, err, manifests)
			}
			cmd := exec.Command("kubeconform", "-strict", "-summary", "-kubernetes-version", version)
			cmd.Stdin = strings.NewReader(manifests)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("kubeconform: %v\n%s", err, out)
			}
			t.Logf("kubeconform: %s", strings.TrimSpace(string(out)))
		})
	}
}

const (
	signInGroupsNamespace  = "shop-staging"
	signInGroupsMiddleware = "shop-staging-itema-login"
	signInGroupsAnnotation = "shop-staging-shop-staging-itema-login@kubernetescrd"
	// The shared middleware's address with the fixture's groups, the
	// second lowercased.
	signInGroupsAddress = "http://oauth2-proxy.oauth2-proxy.svc.cluster.local/?allowed_groups=0f3b6a4e-8c1d-4e2f-9a7b-5c6d7e8f9a0b,6e1d2c3b-4a5f-4b6c-8d7e-9f0a1b2c3d4e"
)

func TestLoginWithoutGroupsRendersNoMiddlewareOfItsOwn(t *testing.T) {
	for _, tc := range []struct {
		fixture string
		args    []string
	}{
		{"login-enabled.yaml", nil},
		{"login-custom-domains.yaml", nil},
		{"login-enabled.yaml", []string{"--set-json", "login.groups=[]"}},
		{"login-enabled.yaml", []string{"--set-json", "login.groups=null"}},
	} {
		t.Run(tc.fixture+" "+strings.Join(tc.args, " "), func(t *testing.T) {
			objects := render(t, tc.fixture, tc.args...)
			for key, obj := range objects {
				if strings.HasPrefix(key, "Middleware/") {
					t.Errorf("rendered %s, want no Middleware without sign-in groups", key)
				}
				if !strings.HasPrefix(key, "Ingress/") {
					continue
				}
				annotations := get[map[string]any](t, obj, "metadata", "annotations")
				if got := annotations["traefik.ingress.kubernetes.io/router.middlewares"]; got != loginMiddlewareAnnotation {
					t.Errorf("%s router.middlewares = %v, want %s", key, got, loginMiddlewareAnnotation)
				}
			}
		})
	}
}

func TestLoginGroupsRenderTheEnvironmentsOwnMiddleware(t *testing.T) {
	objects := render(t, "login-groups.yaml", "--namespace", signInGroupsNamespace)
	mw := mustObject(t, objects, "Middleware/"+signInGroupsMiddleware)
	if got := get[string](t, mw, "apiVersion"); got != "traefik.io/v1alpha1" {
		t.Errorf("Middleware apiVersion = %s, want traefik.io/v1alpha1", got)
	}
	if got := get[string](t, mw, "spec", "forwardAuth", "address"); got != signInGroupsAddress {
		t.Errorf("forwardAuth.address = %s, want %s", got, signInGroupsAddress)
	}
	if got := get[string](t, mw, "metadata", "annotations", "argocd.argoproj.io/sync-wave"); got != "-1" {
		t.Errorf("sync-wave = %s, want -1, before the Ingresses that name it", got)
	}
	// Every Ingress that carries login names it, the HTTP-01 one included.
	for _, name := range []string{"Ingress/shop-staging", "Ingress/shop-staging-http01"} {
		annotations := get[map[string]any](t, mustObject(t, objects, name), "metadata", "annotations")
		if got := annotations["traefik.ingress.kubernetes.io/router.middlewares"]; got != signInGroupsAnnotation {
			t.Errorf("%s router.middlewares = %v, want %s", name, got, signInGroupsAnnotation)
		}
	}
}

// The Environment's Middleware is the shared one with allowed_groups added
// to its address and nothing else changed.
func TestLoginGroupsMiddlewareIsTheSharedOneWithAllowedGroups(t *testing.T) {
	data, err := os.ReadFile("../../bootstrap/components/oauth2-proxy-login/middleware.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var shared object
	if err := yaml.Unmarshal(data, &shared); err != nil {
		t.Fatal(err)
	}
	want := get[map[string]any](t, shared, "spec", "forwardAuth")
	got := get[map[string]any](t, mustObject(t, render(t, "login-groups.yaml", "--namespace", signInGroupsNamespace), "Middleware/"+signInGroupsMiddleware), "spec", "forwardAuth")

	assertForwardAuthWithAllowedGroups(t, got, want)
}

// assertForwardAuthWithAllowedGroups asserts an Environment's forwardAuth is
// the bootstrap's want with ?allowed_groups= on the address and nothing else
// changed.
func assertForwardAuthWithAllowedGroups(t *testing.T, got, want map[string]any) {
	t.Helper()
	address := get[string](t, got, "address")
	base, query, found := strings.Cut(address, "?")
	if !found || base != get[string](t, want, "address") {
		t.Errorf("address = %s, want the bootstrap Middleware's address %s with a query", address, get[string](t, want, "address"))
	}
	if !strings.HasPrefix(query, "allowed_groups=") || strings.Contains(query, "&") {
		t.Errorf("address query = %q, want allowed_groups only", query)
	}
	for key, value := range want {
		if key == "address" {
			continue
		}
		if !reflect.DeepEqual(got[key], value) {
			t.Errorf("forwardAuth.%s = %v, want the bootstrap Middleware's %v", key, got[key], value)
		}
	}
	for key := range got {
		if _, ok := want[key]; !ok {
			t.Errorf("forwardAuth.%s is set, the bootstrap Middleware has no such field", key)
		}
	}
}

func TestLoginGroupsRefused(t *testing.T) {
	for _, tc := range []struct {
		fixture, message string
	}{
		{"refuse-login-groups-without-login.yaml", "login.groups needs login.enabled: true"},
		{"refuse-login-group-not-a-guid.yaml", `login.groups: "0f3b6a4e-8c1d-4e2f-9a7b-5c6d7e8f9a0b&allowed_emails=x" is not an Entra group object id`},
		{"refuse-login-group-duplicate.yaml", `login.groups: "0f3b6a4e-8c1d-4e2f-9a7b-5c6d7e8f9a0b" is listed twice`},
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
	out, err := helmTemplate(t, "login-enabled.yaml", "--set", "login.groups=abc")
	if err == nil || !strings.Contains(out, "login.groups must be a list") {
		t.Errorf("login.groups as a string: err = %v, want the list refusal:\n%s", err, out)
	}
}

// The Middlewares are Traefik CRDs, so kubeconform needs their schema.
func TestLoginGroupsFixturesPassKubeconformWithTheCRDSchemas(t *testing.T) {
	requireTool(t, "kubeconform")
	version := kubernetesVersion(t)
	for _, tc := range []struct {
		fixture string
		args    []string
		valid   int
	}{
		{"login-groups.yaml", nil, 6},
		// Both Middlewares and the four Ingresses.
		{"login-domains-outside-cookie-domain.yaml", []string{"--set-json", `login.groups=["0f3b6a4e-8c1d-4e2f-9a7b-5c6d7e8f9a0b"]`}, 9},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			manifests, err := helmTemplate(t, tc.fixture, append([]string{"--namespace", signInGroupsNamespace}, tc.args...)...)
			if err != nil {
				t.Fatalf("helm template: %v\n%s", err, manifests)
			}
			cmd := exec.Command("kubeconform", "-strict", "-summary", "-kubernetes-version", version,
				"-schema-location", "default", "-schema-location", crdSchemas)
			cmd.Stdin = strings.NewReader(manifests)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("kubeconform: %v\n%s", err, out)
			}
			if want := fmt.Sprintf("Valid: %d,", tc.valid); !strings.Contains(string(out), want) {
				t.Errorf("kubeconform did not validate all %d objects, the Middlewares included: %s", tc.valid, out)
			}
			t.Logf("kubeconform: %s", strings.TrimSpace(string(out)))
		})
	}
}
