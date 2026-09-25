package application_test

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The Itema login Capability: the Platform address's Ingress gets the
// Traefik ForwardAuth middleware annotation for the bootstrap's shared
// oauth2-proxy when login.enabled, and none of the other Ingresses (or an
// Environment without it) ever carry it. It is refused together with any
// custom domain: the oauth2-proxy cookie is scoped to the Platform base
// domain, so Itema login only makes sense on Platform addresses.

const loginMiddlewareAnnotation = "oauth2-proxy-itema-login-auth@kubernetescrd"

func TestLoginAddsTheForwardAuthMiddlewareAnnotation(t *testing.T) {
	ing := mustObject(t, render(t, "login-enabled.yaml"), "Ingress/shop")
	annotations := get[map[string]any](t, ing, "metadata", "annotations")
	if got := annotations["traefik.ingress.kubernetes.io/router.middlewares"]; got != loginMiddlewareAnnotation {
		t.Errorf("router.middlewares = %v, want %s", got, loginMiddlewareAnnotation)
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
