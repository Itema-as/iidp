package application_test

import (
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// A custom domain directly under the base domain joins the Platform Ingress
// under the wildcard certificate; any other gets its own certificate on a
// second Ingress.

// ingressHosts returns the hosts of an Ingress's rules, in order.
func ingressHosts(t *testing.T, ing object) []string {
	t.Helper()
	var hosts []string
	for _, rule := range get[[]any](t, ing, "spec", "rules") {
		hosts = append(hosts, get[string](t, rule, "host"))
	}
	return hosts
}

// tlsEntry is one of an Ingress's tls entries: its hosts and its secretName,
// empty when the entry names none.
type tlsEntry struct {
	hosts      []string
	secretName string
}

// tlsEntries returns an Ingress's tls entries in order.
func tlsEntries(t *testing.T, ing object) []tlsEntry {
	t.Helper()
	var entries []tlsEntry
	for _, item := range get[[]any](t, ing, "spec", "tls") {
		entry, _ := item.(map[string]any)
		var e tlsEntry
		for _, h := range get[[]any](t, entry, "hosts") {
			e.hosts = append(e.hosts, h.(string))
		}
		if secret, set := entry["secretName"]; set {
			e.secretName = secret.(string)
		}
		entries = append(entries, e)
	}
	return entries
}

// assertEveryRuleRoutesTo asserts every rule of the Ingress routes / to the
// Environment's Service on its port.
func assertEveryRuleRoutesTo(t *testing.T, ing object, service string, port int) {
	t.Helper()
	for _, rule := range get[[]any](t, ing, "spec", "rules") {
		paths := get[[]any](t, rule, "http", "paths")
		if len(paths) != 1 {
			t.Fatalf("rule %s has %d paths, want 1", get[string](t, rule, "host"), len(paths))
		}
		if p := get[string](t, paths[0], "path"); p != "/" {
			t.Errorf("rule %s path = %q, want /", get[string](t, rule, "host"), p)
		}
		if svc := get[string](t, paths[0], "backend", "service", "name"); svc != service {
			t.Errorf("rule %s backend = %q, want %q", get[string](t, rule, "host"), svc, service)
		}
		if got := get[int](t, paths[0], "backend", "service", "port", "number"); got != port {
			t.Errorf("rule %s backend port = %d, want %d", get[string](t, rule, "host"), got, port)
		}
	}
}

func TestDomainUnderTheBaseDomainJoinsThePlatformIngressWithoutASecret(t *testing.T) {
	objects := render(t, "custom-domain-wildcard.yaml")
	if want := []string{"ConfigMap/iidp-domains", "Deployment/shop", "Ingress/shop", "Service/shop"}; !slices.Equal(keys(objects), want) {
		t.Fatalf("rendered %v, want exactly %v (no second Ingress for a wildcard host)", keys(objects), want)
	}
	ing := mustObject(t, objects, "Ingress/shop")

	if hosts, want := ingressHosts(t, ing), []string{"shop.app.itma.no", "butikk.app.itma.no"}; !slices.Equal(hosts, want) {
		t.Errorf("rule hosts = %v, want %v", hosts, want)
	}
	assertEveryRuleRoutesTo(t, ing, "shop", 3000)

	tls := tlsEntries(t, ing)
	if len(tls) != 1 {
		t.Fatalf("Ingress has %d tls entries, want 1 shared by every wildcard host: %v", len(tls), tls)
	}
	if want := []string{"shop.app.itma.no", "butikk.app.itma.no"}; !slices.Equal(tls[0].hosts, want) {
		t.Errorf("tls hosts = %v, want %v", tls[0].hosts, want)
	}
	if tls[0].secretName != "" {
		t.Errorf("tls secretName = %q, want none (the wildcard is Traefik's default certificate)", tls[0].secretName)
	}

	annotations := get[map[string]any](t, ing, "metadata", "annotations")
	if issuer, set := annotations["cert-manager.io/cluster-issuer"]; set {
		t.Errorf("Platform Ingress carries cert-manager.io/cluster-issuer=%v; it must not, or cert-manager would try to issue for the wildcard hosts", issuer)
	}
}

// Each foreign host gets an Ingress of its own, so a host whose certificate
// is not issued yet cannot affect another: Traefik stops loading an
// Ingress's certificates at the first Secret that does not exist.
func TestEachForeignDomainGetsAnIngressOfItsOwn(t *testing.T) {
	cases := []struct {
		fixture, name, issuer string
		port                  int
		wildcardHosts         []string
		// foreign maps each foreign host's Ingress name to its tls entry.
		foreign map[string]tlsEntry
	}{
		{
			fixture: "custom-domain-foreign.yaml", name: "shop", issuer: "letsencrypt-http01", port: 3000,
			wildcardHosts: []string{"shop.app.itma.no"},
			foreign: map[string]tlsEntry{
				"shop-shop-example-com":     {hosts: []string{"shop.example.com"}, secretName: "shop-shop-example-com-tls"},
				"shop-www-shop-example-com": {hosts: []string{"www.shop.example.com"}, secretName: "shop-www-shop-example-com-tls"},
			},
		},
		{
			fixture: "custom-domains-mixed.yaml", name: "shop-staging", issuer: "letsencrypt-staging-http01", port: 8080,
			wildcardHosts: []string{"shop-staging.app.itma.no", "butikk-staging.app.itma.no"},
			foreign: map[string]tlsEntry{
				"shop-staging-test-shop-app-itma-no": {hosts: []string{"test.shop.app.itma.no"}, secretName: "shop-staging-test-shop-app-itma-no-tls"},
				"shop-staging-shop-staging-itma-no":  {hosts: []string{"shop-staging.itma.no"}, secretName: "shop-staging-shop-staging-itma-no-tls"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			objects := render(t, tc.fixture)
			want := []string{"ConfigMap/iidp-domains", "Deployment/" + tc.name, "Ingress/" + tc.name, "Service/" + tc.name}
			for name := range tc.foreign {
				want = append(want, "Ingress/"+name)
			}
			slices.Sort(want)
			if !slices.Equal(keys(objects), want) {
				t.Fatalf("rendered %v, want exactly %v", keys(objects), want)
			}

			// The Platform Ingress is untouched by foreign hosts: no issuer
			// annotation, only the wildcard-covered hosts.
			platform := mustObject(t, objects, "Ingress/"+tc.name)
			if issuer, set := get[map[string]any](t, platform, "metadata", "annotations")["cert-manager.io/cluster-issuer"]; set {
				t.Errorf("Platform Ingress carries cert-manager.io/cluster-issuer=%v; foreign hosts must not put it there", issuer)
			}
			if hosts := ingressHosts(t, platform); !slices.Equal(hosts, tc.wildcardHosts) {
				t.Errorf("Platform Ingress hosts = %v, want %v", hosts, tc.wildcardHosts)
			}
			if tls := tlsEntries(t, platform); len(tls) != 1 || !slices.Equal(tls[0].hosts, tc.wildcardHosts) || tls[0].secretName != "" {
				t.Errorf("Platform Ingress tls = %+v, want one entry for %v with no secretName", tls, tc.wildcardHosts)
			}

			for name, entry := range tc.foreign {
				foreign := mustObject(t, objects, "Ingress/"+name)
				annotations := get[map[string]any](t, foreign, "metadata", "annotations")
				if got := annotations["cert-manager.io/cluster-issuer"]; got != tc.issuer {
					t.Errorf("%s cert-manager.io/cluster-issuer = %v, want %q", name, got, tc.issuer)
				}
				if got := annotations["traefik.ingress.kubernetes.io/router.entrypoints"]; got != "websecure" {
					t.Errorf("%s router.entrypoints = %v, want websecure", name, got)
				}
				if got := annotations["traefik.ingress.kubernetes.io/router.tls"]; got != "true" {
					t.Errorf("%s router.tls = %v, want \"true\"", name, got)
				}
				if class := get[string](t, foreign, "spec", "ingressClassName"); class != "traefik" {
					t.Errorf("%s ingressClassName = %q, want traefik", name, class)
				}
				if hosts := ingressHosts(t, foreign); !slices.Equal(hosts, entry.hosts) {
					t.Errorf("%s hosts = %v, want %v", name, hosts, entry.hosts)
				}
				assertEveryRuleRoutesTo(t, foreign, tc.name, tc.port)
				// One tls entry with the host's own secret, so cert-manager
				// issues one Certificate for this Ingress. The secret's name
				// is the one the chart has always given it, so an Environment
				// moving to this layout keeps its certificate.
				if tls := tlsEntries(t, foreign); len(tls) != 1 || !slices.Equal(tls[0].hosts, entry.hosts) || tls[0].secretName != entry.secretName {
					t.Errorf("%s tls = %+v, want %+v", name, tls, entry)
				}
				labels := get[map[string]any](t, foreign, "metadata", "labels")
				if labels["app.kubernetes.io/instance"] != tc.name || labels["iidp.itema.no/application"] != "shop" {
					t.Errorf("%s labels = %v, want the Environment's", name, labels)
				}
			}
		})
	}
}

func TestSecretsAreMountedAsEnvFromByName(t *testing.T) {
	objects := render(t, "secrets.yaml")
	c := container(t, objects, "shop")

	var refs []string
	for _, item := range get[[]any](t, c, "envFrom") {
		refs = append(refs, get[string](t, item, "secretRef", "name"))
	}
	if want := []string{"shop-stripe", "shop-smtp"}; !slices.Equal(refs, want) {
		t.Errorf("envFrom secretRefs = %v, want %v", refs, want)
	}

	// Plain env is still there beside them, and the chart renders no Secret of
	// its own.
	if vars := envVars(t, c); vars["NODE_ENV"] != "production" || vars["PORT"] != "3000" {
		t.Errorf("env = %v, want NODE_ENV and PORT beside the secrets", vars)
	}
	if want := []string{"ConfigMap/iidp-domains", "Deployment/shop", "Ingress/shop", "Service/shop"}; !slices.Equal(keys(objects), want) {
		t.Errorf("rendered %v, want exactly %v", keys(objects), want)
	}
}

func TestContainerHasNoEnvFromWithoutSecrets(t *testing.T) {
	c := container(t, render(t, "prod-small.yaml"), "shop")
	if envFrom, set := c["envFrom"]; set {
		t.Errorf("envFrom = %v, want none when secrets is empty", envFrom)
	}
}

func TestRenderingRefusesBadDomains(t *testing.T) {
	cases := []struct {
		fixture, message string
	}{
		{"refuse-domain-invalid.yaml", `domains: "https://shop.example.com/" is not a valid DNS hostname`},
		{"refuse-domain-duplicate.yaml", `domains: "shop.example.com" is listed twice`},
		{"refuse-domain-platform-address.yaml", `domains: "shop-staging.app.itma.no" is this Environment's Platform address, which is always served; remove it`},
		{"refuse-domain-too-long.yaml", `is too long for the name of its TLS secret (shop-staging-<host>-tls must be at most 253 characters)`},
		{"refuse-domain-same-name.yaml", `domains: "a-b.example.com" and "a.b-example.com" would share the Ingress shop-a-b-example-com and the TLS secret shop-a-b-example-com-tls; keep one of them`},
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

// These fixtures render objects the Web service fixtures do not (an Ingress
// per foreign host, envFrom), so they are validated too.
func TestStaticSiteDomainAndSecretFixturesPassKubeconform(t *testing.T) {
	requireTool(t, "kubeconform")
	version := kubernetesVersion(t)
	for _, fixture := range []string{"static-site.yaml", "static-site-probe.yaml", "custom-domain-wildcard.yaml", "custom-domain-foreign.yaml", "custom-domains-mixed.yaml", "secrets.yaml"} {
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
