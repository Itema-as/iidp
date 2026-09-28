package argus

import (
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/Itema-as/iidp/internal/platformstate"
)

func TestNewPlatformGivesWebAddresses(t *testing.T) {
	p := NewPlatform(" https://argocd.app.itma.no/ ", "https://itema.grafana.net/", "https://github.com/Itema-as/iidp-platform.git", "https://github.com/Itema-as/iidp.git", "v1.2.3")
	want := Platform{
		ArgoCDURL: "https://argocd.app.itma.no", GrafanaURL: "https://itema.grafana.net",
		PlatformRepository: "https://github.com/Itema-as/iidp-platform", BootstrapRepository: "https://github.com/Itema-as/iidp", BootstrapRevision: "v1.2.3",
	}
	if p != want {
		t.Errorf("NewPlatform = %+v, want %+v", p, want)
	}
}

// The snapshot carries where the card links out to, and each Environment
// its ArgoCD and Grafana links, as iidp app status gives them.
func TestSnapshotCarriesThePlatformsLinks(t *testing.T) {
	s, _ := newStore(t)
	put(t, s, servingShop()...)
	s.Seed()
	if env := s.Snapshot().Applications[0].Environments[0]; env.Links != nil {
		t.Errorf("links = %+v without a Platform, want none", env.Links)
	}

	p := NewPlatform("https://argocd.example.test", "https://itema.grafana.net", "https://github.com/Itema-as/iidp-platform.git", "", "")
	s.SetPlatform(p)
	s.Recompute()
	snap := s.Snapshot()
	if snap.Platform != p {
		t.Errorf("snapshot's platform = %+v, want %+v", snap.Platform, p)
	}
	env := snap.Applications[0].Environments[0]
	if env.Links == nil || env.Links.ArgoCD != "https://argocd.example.test/applications/argocd/shop-prod" || !strings.Contains(env.Links.Grafana, "shop-prod") {
		t.Errorf("links = %+v", env.Links)
	}
}

const commit2 = "2222222222222222222222222222222222222222"

// shopDeployed is shop prod having served 1.0.1, which the Deploy gate
// accepted as commit2 twelve minutes before t0 and ArgoCD synced ten
// minutes before t0.
func shopDeployed() []obj {
	app := argoApp("shop", "prod")
	status := app["status"].(obj)
	status["sync"] = obj{"status": "Synced", "revisions": []any{"0.4.0", commit2}}
	status["history"] = append(status["history"].([]any), obj{"id": 2, "revisions": []any{"0.4.0", commit2}, "deployedAt": at(-10 * time.Minute)})
	status["operationState"] = obj{
		"phase": "Succeeded", "startedAt": at(-11 * time.Minute), "finishedAt": at(-10 * time.Minute),
		"operation":  obj{"sync": obj{"revisions": []any{"0.4.0", commit2}}},
		"syncResult": obj{"revisions": []any{"0.4.0", commit2}},
	}
	return []obj{
		app,
		envDeployment("shop", "prod", "1.0.1"),
		pod("shop-prod", "shop-b", "shop", "1.0.1", true, -9*time.Minute),
		gateEvent("shop", "prod", platformstate.ReasonDeployAccepted, "deploy", "1.0.1", commit2, "Deploy shop prod 1.0.1 accepted", -12*time.Minute),
	}
}

// Argus knows when an image was deployed from the Deploy that brought it:
// ArgoCD's history entry for the Deploy gate's commit. It keeps that once
// the gate's Event has expired, for as long as the tag runs, and does not
// guess for a tag it never saw deployed.
func TestDeployedAtComesFromArgoCDsHistory(t *testing.T) {
	s, _ := newStore(t)
	put(t, s, shopDeployed()...)
	s.Seed()
	deployedAt := func() *time.Time {
		t.Helper()
		env := s.Snapshot().Applications[0].Environments[0]
		if env.Image == nil {
			t.Fatal("no image")
		}
		return env.Image.DeployedAt
	}
	if at := deployedAt(); at == nil || !at.Equal(t0.Add(-10*time.Minute)) {
		t.Fatalf("deployedAt = %v, want ArgoCD's sync of commit2, %s", at, t0.Add(-10*time.Minute))
	}

	// The gate's Event expires after an hour.
	s.Delete("events", "argocd/shop-prod.1.0.1")
	s.Recompute()
	if at := deployedAt(); at == nil || !at.Equal(t0.Add(-10*time.Minute)) {
		t.Errorf("deployedAt = %v once the Event expired, want it kept", at)
	}

	// A later tag rolled out without an Event Argus saw.
	put(t, s, envDeployment("shop", "prod", "1.0.2"), pod("shop-prod", "shop-b", "shop", "1.0.2", true, -time.Minute))
	s.Recompute()
	if at := deployedAt(); at != nil {
		t.Errorf("deployedAt = %v for a tag Argus never saw deployed, want none", at)
	}
}

// The web directory is served with a policy that lets the page load only
// from Argus, and its import map run by its hash.
func TestTheWebDirectoryHasAStrictContentSecurityPolicy(t *testing.T) {
	importMap := `{ "imports": { "three": "./vendor/three/three.module.min.js" } }`
	web := fstest.MapFS{"index.html": {Data: []byte(`<!doctype html><title>Argus</title><script type="importmap">` + importMap + `</script>`)}}
	s, _ := newStore(t)
	srv := serveWeb(t, s, web)
	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	csp := resp.Header.Get("Content-Security-Policy")
	sum := sha256.Sum256([]byte(importMap))
	for _, want := range []string{
		"default-src 'self'",
		"script-src 'self' 'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'",
		"connect-src 'self'", "font-src 'self'", "style-src 'self'", "img-src 'self'", "object-src 'none'",
	} {
		if !strings.Contains(csp, want) {
			t.Errorf("Content-Security-Policy = %q, want %q in it", csp, want)
		}
	}
	if strings.Contains(csp, "http") || strings.Contains(csp, "unsafe") || strings.Contains(csp, "*") {
		t.Errorf("Content-Security-Policy = %q allows more than Argus itself", csp)
	}
}
