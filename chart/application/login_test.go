package application_test

import (
	"os/exec"
	"strings"
	"testing"
)

// The Itema login Capability: the Platform address's Ingress gets the
// Traefik ForwardAuth middleware annotation for the bootstrap's shared
// oauth2-proxy when login.enabled, and none of the other Ingresses (or an
// Environment without it) ever carry it. It is refused together with any
// custom domain: the oauth2-proxy cookie is scoped to the Platform base
// domain, so Itema login only makes sense on Platform addresses.

const loginMiddlewareAnnotation = "oauth2-proxy-itema-login-errors@kubernetescrd,oauth2-proxy-itema-login-auth@kubernetescrd"

func TestLoginAddsTheForwardAuthMiddlewareAnnotation(t *testing.T) {
	ing := mustObject(t, render(t, "login-enabled.yaml"), "Ingress/shop")
	annotations := get[map[string]any](t, ing, "metadata", "annotations")
	if got := annotations["traefik.ingress.kubernetes.io/router.middlewares"]; got != loginMiddlewareAnnotation {
		t.Errorf("router.middlewares = %v, want %s", got, loginMiddlewareAnnotation)
	}
}

func TestLoginDisabledByDefaultLeavesNoMiddlewareAnnotation(t *testing.T) {
	ing := mustObject(t, render(t, "prod-small.yaml"), "Ingress/shop")
	annotations := get[map[string]any](t, ing, "metadata", "annotations")
	if _, set := annotations["traefik.ingress.kubernetes.io/router.middlewares"]; set {
		t.Errorf("router.middlewares = %v, want unset when login is disabled", annotations["traefik.ingress.kubernetes.io/router.middlewares"])
	}
}

func TestLoginRefusedWithCustomDomains(t *testing.T) {
	out, err := helmTemplate(t, "refuse-login-with-domain.yaml")
	if err == nil {
		t.Fatalf("rendering succeeded, want a failure:\n%s", out)
	}
	want := "login.enabled needs domains to be empty"
	if !strings.Contains(out, want) {
		t.Fatalf("error does not say %q:\n%s", want, out)
	}
}

func TestLoginFixturesPassKubeconform(t *testing.T) {
	requireTool(t, "kubeconform")
	version := kubernetesVersion(t)
	for _, fixture := range []string{"login-enabled.yaml"} {
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
