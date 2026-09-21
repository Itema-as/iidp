package application_test

import (
	"maps"
	"slices"
	"testing"
)

// A Static site is built into an nginx image by CI and deployed through the
// same chart as a Web service. nginx listens on 80 and takes no PORT, so the
// chart fixes the port and skips the injected variable; everything else
// renders exactly as for a Web service.

func TestStaticSiteServesOnPort80WithoutPORT(t *testing.T) {
	objects := render(t, "static-site.yaml")
	c := container(t, objects, "brochure")

	ports := get[[]any](t, c, "ports")
	if len(ports) != 1 {
		t.Fatalf("container has %d ports, want 1", len(ports))
	}
	if got := get[int](t, ports[0], "containerPort"); got != 80 {
		t.Errorf("containerPort = %d, want 80", got)
	}

	// Plain env still reaches the container; PORT is nginx's business.
	if vars, want := envVars(t, c), map[string]string{"SITE_ORIGIN": "https://brochure.app.itma.no"}; !maps.Equal(vars, want) {
		t.Errorf("env = %v, want %v", vars, want)
	}

	svcPorts := get[[]any](t, mustObject(t, objects, "Service/brochure"), "spec", "ports")
	if len(svcPorts) != 1 {
		t.Fatalf("Service has %d ports, want 1", len(svcPorts))
	}
	if port := get[int](t, svcPorts[0], "port"); port != 80 {
		t.Errorf("Service port = %d, want 80", port)
	}

	ing := mustObject(t, objects, "Ingress/brochure")
	paths := get[[]any](t, get[[]any](t, ing, "spec", "rules")[0], "http", "paths")
	if port := get[int](t, paths[0], "backend", "service", "port", "number"); port != 80 {
		t.Errorf("Ingress backend port = %d, want 80", port)
	}
}

func TestStaticSiteProbesRequestTheProbePath(t *testing.T) {
	cases := []struct {
		fixture, deployment, path string
	}{
		{"static-site.yaml", "brochure", "/"},
		{"static-site-probe.yaml", "brochure-staging", "/healthz.html"},
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

func TestStaticSiteRendersTheSameObjectsAsAWebService(t *testing.T) {
	objects := render(t, "static-site.yaml")
	if want := []string{"Deployment/brochure", "Ingress/brochure", "Service/brochure"}; !slices.Equal(keys(objects), want) {
		t.Fatalf("rendered %v, want exactly %v", keys(objects), want)
	}

	c := container(t, objects, "brochure")
	for _, field := range []string{"requests", "limits"} {
		if cpu := get[string](t, c, "resources", field, "cpu"); cpu != "500m" {
			t.Errorf("%s.cpu = %q, want the medium size's 500m", field, cpu)
		}
		if mem := get[string](t, c, "resources", field, "memory"); mem != "512Mi" {
			t.Errorf("%s.memory = %q, want the medium size's 512Mi", field, mem)
		}
	}

	ing := mustObject(t, objects, "Ingress/brochure")
	if host := get[string](t, get[[]any](t, ing, "spec", "rules")[0], "host"); host != "brochure.app.itma.no" {
		t.Errorf("host = %q, want brochure.app.itma.no", host)
	}
}
