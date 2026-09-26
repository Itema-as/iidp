package application_test

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"reflect"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The Itema login Capability: every Ingress of an Environment with
// login.enabled gets the Traefik ForwardAuth middleware annotation for the
// bootstrap's shared oauth2-proxy, the Platform address's and the HTTP-01
// one's alike, and an Environment without it never carries it. A custom
// domain outside platform.loginCookieDomain (baseDomain when empty) is
// refused: the oauth2-proxy cookie would never reach it
// (docs/implementation-notes/76-login-in-zone-domains.md).

const loginMiddlewareAnnotation = "oauth2-proxy-itema-login-auth@kubernetescrd"

func TestLoginAddsTheForwardAuthMiddlewareAnnotation(t *testing.T) {
	ing := mustObject(t, render(t, "login-enabled.yaml"), "Ingress/shop")
	annotations := get[map[string]any](t, ing, "metadata", "annotations")
	if got := annotations["traefik.ingress.kubernetes.io/router.middlewares"]; got != loginMiddlewareAnnotation {
		t.Errorf("router.middlewares = %v, want %s", got, loginMiddlewareAnnotation)
	}
}

// Custom domains inside the login cookie domain are protected on whichever
// Ingress they land: the wildcard-covered one on the Platform Ingress, the
// rest (x.itma.no, a deeper name under the base domain, the zone apex) on
// the HTTP-01 one, which keeps its issuer annotation.
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

// Without platform.loginCookieDomain the cookie domain is the base domain,
// as it is on a Platform without a cloudflareZone: a host under it is
// allowed, one only inside the zone is not.
func TestLoginCookieDomainDefaultsToTheBaseDomain(t *testing.T) {
	objects := render(t, "login-custom-domains.yaml", "--set", "platform.loginCookieDomain=", "--set", "domains={butikk.app.itma.no,test.shop.app.itma.no}")
	for _, name := range []string{"Ingress/shop", "Ingress/shop-http01"} {
		annotations := get[map[string]any](t, mustObject(t, objects, name), "metadata", "annotations")
		if got := annotations["traefik.ingress.kubernetes.io/router.middlewares"]; got != loginMiddlewareAnnotation {
			t.Errorf("%s router.middlewares = %v, want %s", name, got, loginMiddlewareAnnotation)
		}
	}
	out, err := helmTemplate(t, "login-custom-domains.yaml", "--set", "platform.loginCookieDomain=")
	if err == nil {
		t.Fatalf("rendering succeeded, want a failure:\n%s", out)
	}
	want := `login.enabled needs every custom domain inside platform.loginCookieDomain "app.itma.no", the domain the Itema login cookie is set for; outside it: x.itma.no, itma.no`
	if !strings.Contains(out, want) {
		t.Fatalf("error does not say %q:\n%s", want, out)
	}
}

// Every middleware the annotation names is one the bootstrap defines in
// the oauth2-proxy namespace. Traefik refuses a router whose middleware
// does not exist, so a name out of step with the bootstrap would take the
// Application off the air rather than leave it unprotected.
func TestLoginMiddlewaresExistInTheBootstrap(t *testing.T) {
	data, err := os.ReadFile("../../bootstrap/components/oauth2-proxy-login/middleware.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defined := map[string]bool{}
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
	for _, name := range strings.Split(loginMiddlewareAnnotation, ",") {
		if !defined[name] {
			t.Errorf("the annotation names %s, which bootstrap/components/oauth2-proxy-login/middleware.yaml does not define", name)
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

// The HTTP-01 Ingress carries the middleware only with login.enabled.
func TestLoginDisabledLeavesTheHTTP01IngressWithoutMiddleware(t *testing.T) {
	ing := mustObject(t, render(t, "custom-domains-mixed.yaml"), "Ingress/shop-staging-http01")
	annotations := get[map[string]any](t, ing, "metadata", "annotations")
	if _, set := annotations["traefik.ingress.kubernetes.io/router.middlewares"]; set {
		t.Errorf("router.middlewares = %v, want unset when login is disabled", annotations["traefik.ingress.kubernetes.io/router.middlewares"])
	}
}

func TestLoginRefusedOutsideTheCookieDomain(t *testing.T) {
	for _, tc := range []struct {
		fixture, message string
	}{
		{"refuse-login-domain-outside-cookie-domain.yaml", `login.enabled needs every custom domain inside platform.loginCookieDomain "itma.no", the domain the Itema login cookie is set for; outside it: shop.example.com, notitma.no`},
		{"refuse-login-platform-address-outside-cookie-domain.yaml", `login.enabled needs the Platform address "shop.app.itma.no" inside platform.loginCookieDomain "example.com"`},
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
}

func TestLoginFixturesPassKubeconform(t *testing.T) {
	requireTool(t, "kubeconform")
	version := kubernetesVersion(t)
	for _, fixture := range []string{"login-enabled.yaml", "login-custom-domains.yaml"} {
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

// Sign-in groups (#92). Without any, login renders exactly what it did
// before: no Middleware of the Environment's own, and the shared one named
// on every Ingress. With groups, the Environment gets its own ForwardAuth
// Middleware asking the same oauth2-proxy to check allowed_groups, and
// every Ingress that carries login names it instead
// (docs/implementation-notes/92-sign-in-groups.md).

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
	// Every Ingress that carries login names it: the Platform address's
	// (with the wildcard-covered custom domain) and the HTTP-01 one.
	for _, name := range []string{"Ingress/shop-staging", "Ingress/shop-staging-http01"} {
		annotations := get[map[string]any](t, mustObject(t, objects, name), "metadata", "annotations")
		if got := annotations["traefik.ingress.kubernetes.io/router.middlewares"]; got != signInGroupsAnnotation {
			t.Errorf("%s router.middlewares = %v, want %s", name, got, signInGroupsAnnotation)
		}
	}
}

// The Environment's Middleware is the bootstrap's shared itema-login-auth
// with allowed_groups added to its address, and nothing else changed: the
// same oauth2-proxy root address, so an unauthenticated browser is still
// redirected to Entra ID, and the same headers passed on.
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

	address := get[string](t, got, "address")
	base, query, found := strings.Cut(address, "?")
	if !found || base != get[string](t, want, "address") {
		t.Errorf("address = %s, want the shared address %s with a query", address, get[string](t, want, "address"))
	}
	if !strings.HasPrefix(query, "allowed_groups=") || strings.Contains(query, "&") {
		t.Errorf("address query = %q, want allowed_groups only", query)
	}
	for key, value := range want {
		if key == "address" {
			continue
		}
		if !reflect.DeepEqual(got[key], value) {
			t.Errorf("forwardAuth.%s = %v, want the shared middleware's %v", key, got[key], value)
		}
	}
	for key := range got {
		if _, ok := want[key]; !ok {
			t.Errorf("forwardAuth.%s is set, the shared middleware has no such field", key)
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

// The Middleware is a Traefik CRD, so kubeconform needs its schema, from
// the same catalogue the Postgres objects' come from.
func TestLoginGroupsFixturePassesKubeconformWithTheCRDSchemas(t *testing.T) {
	requireTool(t, "kubeconform")
	version := kubernetesVersion(t)
	manifests, err := helmTemplate(t, "login-groups.yaml", "--namespace", signInGroupsNamespace)
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
	// Six since #90 added the iidp-domains ConfigMap.
	if !strings.Contains(string(out), "Valid: 6") {
		t.Errorf("kubeconform did not validate all six objects, the Middleware included: %s", out)
	}
	t.Logf("kubeconform: %s", strings.TrimSpace(string(out)))
}
