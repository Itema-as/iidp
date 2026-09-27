package cli_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Itema-as/iidp/internal/cli"
	"github.com/Itema-as/iidp/internal/platform"
	"github.com/Itema-as/iidp/internal/platformstate"
)

// iidp app status is tested through the CLI seam against a fake Deploy
// gate. The CLI finds the gate at https://deploy.<baseDomain>; gateRoute
// sends whatever it asks for there to the fake instead, and records the
// URL it asked for.

const statusPlatformYAML = `baseDomain: app.example.test
chartVersion: 0.3.1
`

type gateRoute struct {
	target *url.URL
	// err, when set, is what every request fails with: an unreachable gate.
	err  error
	mu   sync.Mutex
	urls []string
	auth []string
}

func (g *gateRoute) RoundTrip(r *http.Request) (*http.Response, error) {
	g.mu.Lock()
	g.urls = append(g.urls, r.URL.String())
	g.auth = append(g.auth, r.Header.Get("Authorization"))
	g.mu.Unlock()
	if g.err != nil {
		return nil, g.err
	}
	routed := r.Clone(r.Context())
	routed.URL.Scheme, routed.URL.Host, routed.Host = g.target.Scheme, g.target.Host, ""
	return http.DefaultTransport.RoundTrip(routed)
}

// newFakeStatusGate serves GET /v1/status/{app} with answer.
func newFakeStatusGate(t *testing.T, answer func(w http.ResponseWriter, application string)) *gateRoute {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/status/{application}", func(w http.ResponseWriter, r *http.Request) {
		answer(w, r.PathValue("application"))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	target, _ := url.Parse(srv.URL)
	return &gateRoute{target: target}
}

func appStatus(t *testing.T, platformRepo string, route *gateRoute, token fakeTokenSource, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	deps := cli.Dependencies{TokenSource: token, HTTPClient: &http.Client{Transport: route, Timeout: 10 * time.Second}}
	var out, errOut bytes.Buffer
	code = cli.RunWith(append([]string{"app", "status", "--platform-repo", platformRepo}, args...), strings.NewReader(""), &out, &errOut, deps)
	return out.String(), errOut.String(), code
}

func tp(s string) *time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return &t
}

// shopStatus is what the gate answers for shop: prod with a migration,
// tasks and links, staging without, a preview, and an Environment ArgoCD
// has not picked up.
func shopStatus() platformstate.Status {
	return platformstate.Status{
		Application: "shop",
		Repository:  platform.Org + "/shop",
		Environments: []platformstate.Environment{
			{
				Name: "prod", Namespace: "shop-prod",
				ArgoCD: &platformstate.ArgoCD{Application: "shop-prod", Sync: "Synced", Health: "Healthy",
					Operation: &platformstate.Operation{Phase: "Succeeded", FinishedAt: tp("2026-09-21T10:03:00Z")}},
				Image:     &platformstate.Image{Repository: "ghcr.io/itema-as/shop", Tag: "1.0.1", DeployedAt: tp("2026-09-21T10:00:00Z")},
				Pods:      platformstate.Pods{Ready: 1, Total: 1, Restarts: 1},
				Migration: &platformstate.Run{Result: "succeeded", StartedAt: tp("2026-09-21T10:01:00Z"), FinishedAt: tp("2026-09-21T10:02:00Z")},
				Tasks: []platformstate.Task{
					{Name: "cleanup", Schedule: "0 3 * * *", LastRun: &platformstate.Run{Result: "failed", FinishedAt: tp("2026-09-22T01:00:30Z")}},
					{Name: "report", Schedule: "*/5 * * * *"},
				},
				Addresses: []string{"https://shop.app.example.test", "https://www.shop.example"},
				Links:     &platformstate.Links{ArgoCD: "https://argocd.example.test/applications/argocd/shop-prod", Grafana: "https://itema.grafana.net/explore?x"},
				Condition: &platformstate.Condition{State: "Healthy"},
				Activity: &platformstate.Activity{State: "Deploying", Deploy: &platformstate.Deploy{
					Tag: "1.0.2", Commit: "c0ffee1", Promote: true, At: tp("2026-09-27T12:00:00Z"), Hop: platformstate.HopRollingOut}},
			},
			{
				Name: "staging", Namespace: "shop-staging",
				ArgoCD:    &platformstate.ArgoCD{Application: "shop-staging", Sync: "OutOfSync", Health: "Degraded", Operation: &platformstate.Operation{Phase: "Failed", Message: "one or more objects failed to apply", FinishedAt: tp("2026-09-23T10:00:00Z")}},
				Image:     &platformstate.Image{Repository: "ghcr.io/itema-as/shop", Tag: "sha-1"},
				Pods:      platformstate.Pods{Ready: 0, Total: 1, Restarts: 7},
				Tasks:     []platformstate.Task{},
				Addresses: []string{"https://shop-staging.app.example.test"},
				Condition: &platformstate.Condition{State: "Degraded", Reason: "shop-staging-a is in CrashLoopBackOff"},
				Activity: &platformstate.Activity{State: "Deploying", Stuck: true, Reason: "the migration failed",
					Deploy: &platformstate.Deploy{Tag: "sha-2", Hop: platformstate.HopApplying, Stuck: true, Reason: "the migration failed"}},
			},
			{Name: "pr-7", Namespace: "shop-pr-7", ArgoCD: &platformstate.ArgoCD{Application: "shop-pr-7", Sync: "Synced", Health: "Healthy"}, Tasks: []platformstate.Task{}, Addresses: []string{},
				Condition: &platformstate.Condition{State: "Healthy"}, Activity: &platformstate.Activity{State: "Unreleased"}},
			{Name: "later", Tasks: []platformstate.Task{}, Addresses: []string{}},
		},
	}
}

func answerShop(w http.ResponseWriter, application string) {
	if application != "shop" {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "there is no Application " + application + " on the Platform"})
		return
	}
	_ = json.NewEncoder(w).Encode(shopStatus())
}

func TestAppStatusPrintsEveryEnvironment(t *testing.T) {
	repo := newPlatformRepository(t, statusPlatformYAML)
	route := newFakeStatusGate(t, answerShop)

	stdout, stderr, code := appStatus(t, repo, route, fakeTokenSource{token: "gho_dev"}, "shop")
	if code != 0 {
		t.Fatalf("exit code = %d\nstderr: %s", code, stderr)
	}
	// The gate named by platform.yaml's baseDomain, with the gh token.
	if strings.Join(route.urls, " ") != "https://deploy.app.example.test/v1/status/shop" || strings.Join(route.auth, " ") != "Bearer gho_dev" {
		t.Errorf("requests = %v with %v, want the gate at deploy.<baseDomain> with the gh token", route.urls, route.auth)
	}
	want := `shop (Itema-as/shop)

prod
  Condition: Healthy
  Activity:  Deploying 1.0.2 (Promote), rolling out
  Status:    Synced, Healthy, last sync Succeeded 2026-09-21 10:03 UTC
  Image:     ghcr.io/itema-as/shop:1.0.1, deployed 2026-09-21 10:00 UTC
  Pods:      1/1 ready, 1 restart
  Migration: last run succeeded 2026-09-21 10:02 UTC
  Tasks:     cleanup (0 3 * * *): last run failed 2026-09-22 01:00 UTC
             report (*/5 * * * *): no run yet
  Addresses: https://shop.app.example.test
             https://www.shop.example
  ArgoCD:    https://argocd.example.test/applications/argocd/shop-prod
  Logs:      https://itema.grafana.net/explore?x

staging
  Condition: Degraded: shop-staging-a is in CrashLoopBackOff
  Activity:  Deploying sha-2, stuck at Applying: the migration failed
  Status:    OutOfSync, Degraded, last sync Failed 2026-09-23 10:00 UTC: one or more objects failed to apply
  Image:     ghcr.io/itema-as/shop:sha-1
  Pods:      0/1 ready, 7 restarts
  Addresses: https://shop-staging.app.example.test

pr-7
  Condition: Healthy
  Activity:  Unreleased
  Status:    Synced, Healthy
  Image:     none yet: nothing has been deployed

later
  Status:    not on the Platform yet: ArgoCD picks a new Environment up within a few minutes
`
	if stdout != want {
		t.Errorf("stdout:\n%s\nwant:\n%s", stdout, want)
	}
}

func TestAppStatusJSONIsTheGatesAnswer(t *testing.T) {
	repo := newPlatformRepository(t, statusPlatformYAML)
	route := newFakeStatusGate(t, answerShop)

	stdout, stderr, code := appStatus(t, repo, route, fakeTokenSource{token: "gho_dev"}, "shop", "--json")
	if code != 0 {
		t.Fatalf("exit code = %d\nstderr: %s", code, stderr)
	}
	var got platformstate.Status
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout)
	}
	want, _ := json.Marshal(shopStatus())
	again, _ := json.Marshal(got)
	if string(want) != string(again) {
		t.Errorf("--json = %s\nwant %s", again, want)
	}
	// The documented keys, spelled as README.md spells them.
	for _, key := range []string{`"application": "shop"`, `"repository"`, `"environments"`, `"argocd"`, `"sync": "Synced"`, `"health"`, `"operation"`, `"image"`, `"deployedAt": "2026-09-21T10:00:00Z"`, `"pods"`, `"ready"`, `"restarts"`, `"migration"`, `"tasks"`, `"lastRun"`, `"addresses"`, `"links"`, `"grafana"`,
		`"condition": {`, `"state": "Degraded"`, `"reason": "shop-staging-a is in CrashLoopBackOff"`, `"activity": {`, `"stuck": true`,
		`"deploy": {`, `"hop": "RollingOut"`, `"promote": true`, `"tag": "sha-2"`, `"activity": null`} {
		if !strings.Contains(stdout, key) {
			t.Errorf("--json lacks %s", key)
		}
	}
}

// Each Activity in words, and an Environment from a Deploy gate older than
// Condition and Activity, which sends neither.
func TestAppStatusPrintsConditionAndActivity(t *testing.T) {
	envs := []platformstate.Environment{
		{Name: "prod", Condition: &platformstate.Condition{State: "Healthy"},
			Activity: &platformstate.Activity{State: "Updating", Stuck: true, Reason: "OutOfSync for over 5 minutes with no sync started"}},
		{Name: "staging", Condition: &platformstate.Condition{State: "Unknown", Reason: "the state of shop-a is unknown"},
			Activity: &platformstate.Activity{State: "Arriving", Deploy: &platformstate.Deploy{Tag: "sha-1", Hop: platformstate.HopWaitingForArgoCD}}},
		{Name: "pr-3", Condition: &platformstate.Condition{State: "Healthy"}, Activity: &platformstate.Activity{State: "Leaving", Stuck: true, Reason: "the final backup failed"}},
		{Name: "pr-4", Condition: &platformstate.Condition{State: "Healthy"},
			Activity: &platformstate.Activity{State: "Deploying", Stuck: true, Reason: "the image ghcr.io/itema-as/shop:sha-9 cannot be pulled (ImagePullBackOff)",
				Deploy: &platformstate.Deploy{Tag: "sha-9", Preview: true, Hop: platformstate.HopRollingOut, Stuck: true}}},
		{Name: "pr-5"},
	}
	for i := range envs {
		envs[i].ArgoCD = &platformstate.ArgoCD{Application: "shop-" + envs[i].Name, Sync: "Synced", Health: "Healthy"}
		envs[i].Tasks, envs[i].Addresses = []platformstate.Task{}, []string{}
	}
	repo := newPlatformRepository(t, statusPlatformYAML)
	route := newFakeStatusGate(t, func(w http.ResponseWriter, _ string) {
		_ = json.NewEncoder(w).Encode(platformstate.Status{Application: "shop", Repository: platform.Org + "/shop", Environments: envs})
	})
	stdout, stderr, code := appStatus(t, repo, route, fakeTokenSource{token: "gho_dev"}, "shop")
	if code != 0 {
		t.Fatalf("exit code = %d\nstderr: %s", code, stderr)
	}
	want := `shop (Itema-as/shop)

prod
  Condition: Healthy
  Activity:  Updating, stuck: OutOfSync for over 5 minutes with no sync started
  Status:    Synced, Healthy
  Image:     none yet: nothing has been deployed

staging
  Condition: Unknown: the state of shop-a is unknown
  Activity:  Arriving sha-1, waiting for ArgoCD
  Status:    Synced, Healthy
  Image:     none yet: nothing has been deployed

pr-3
  Condition: Healthy
  Activity:  Leaving, stuck: the final backup failed
  Status:    Synced, Healthy
  Image:     none yet: nothing has been deployed

pr-4
  Condition: Healthy
  Activity:  Deploying sha-9, stuck at Rolling out: the image ghcr.io/itema-as/shop:sha-9 cannot be pulled (ImagePullBackOff)
  Status:    Synced, Healthy
  Image:     none yet: nothing has been deployed

pr-5
  Status:    Synced, Healthy
  Image:     none yet: nothing has been deployed
`
	if stdout != want {
		t.Errorf("stdout:\n%s\nwant:\n%s", stdout, want)
	}
}

func TestAppStatusErrors(t *testing.T) {
	refuse := func(status int, msg string) func(http.ResponseWriter, string) {
		return func(w http.ResponseWriter, _ string) {
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
		}
	}
	for _, tc := range []struct {
		name   string
		answer func(http.ResponseWriter, string)
		err    error
		app    string
		want   string
	}{
		{"no access", refuse(http.StatusForbidden, "refused: you cannot read shop's Application repository (Itema-as/shop, repository id 1)"), nil, "shop",
			"no access to shop's status: refused: you cannot read shop's Application repository"},
		{"no binding", refuse(http.StatusForbidden, "refused: shop is not bound to an Application repository"), nil, "shop",
			"no access to shop's status: refused: shop is not bound"},
		{"an unknown Application", answerShop, nil, "nosuchapp", "unknown Application nosuchapp: there is no Application nosuchapp on the Platform"},
		{"an unreachable gate", nil, errors.New("dial tcp: connection refused"), "shop",
			"the Deploy gate at https://deploy.app.example.test cannot be reached"},
		{"a gate that cannot read the cluster", refuse(http.StatusServiceUnavailable, "the Deploy gate cannot read the cluster right now"), nil, "shop",
			"the Deploy gate at https://deploy.app.example.test is unavailable: HTTP 503: the Deploy gate cannot read the cluster right now"},
		{"a gate from before app status", func(w http.ResponseWriter, _ string) { http.NotFound(w, nil) }, nil, "shop",
			"does not answer status calls (HTTP 404); it may be older than iidp app status"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := newPlatformRepository(t, statusPlatformYAML)
			route := &gateRoute{err: tc.err}
			if tc.answer != nil {
				route = newFakeStatusGate(t, tc.answer)
			}
			stdout, stderr, code := appStatus(t, repo, route, fakeTokenSource{token: "gho_dev"}, tc.app)
			if code == 0 || !strings.Contains(stderr, tc.want) {
				t.Errorf("exit code = %d, stderr = %q, want a failure saying %q", code, stderr, tc.want)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want nothing", stdout)
			}
		})
	}
}

func TestAppStatusNeedsAGitHubLoginAndAValidName(t *testing.T) {
	repo := newPlatformRepository(t, statusPlatformYAML)
	route := newFakeStatusGate(t, answerShop)

	_, stderr, code := appStatus(t, repo, route, fakeTokenSource{err: errors.New("gh auth token: not logged in")}, "shop")
	if code == 0 || !strings.Contains(stderr, "not logged in to GitHub") {
		t.Errorf("without a login: exit %d, stderr %q", code, stderr)
	}
	_, stderr, code = appStatus(t, repo, route, fakeTokenSource{token: "gho_dev"}, "Shop")
	if code == 0 || !strings.Contains(stderr, "invalid Application name") {
		t.Errorf("an invalid name: exit %d, stderr %q", code, stderr)
	}
	if len(route.urls) != 0 {
		t.Errorf("the gate was called %v, want not at all", route.urls)
	}
}

func TestAppStatusFallsBackToItemasGateWithoutPlatformYAML(t *testing.T) {
	route := newFakeStatusGate(t, answerShop)
	missing := "file://" + t.TempDir() + "/no-such-platform.git"

	_, stderr, code := appStatus(t, missing, route, fakeTokenSource{token: "gho_dev"}, "shop")
	if code != 0 {
		t.Fatalf("exit code = %d\nstderr: %s", code, stderr)
	}
	if strings.Join(route.urls, " ") != platform.DefaultDeployGateURL+"/v1/status/shop" {
		t.Errorf("requests = %v, want %s", route.urls, platform.DefaultDeployGateURL)
	}
	if !strings.Contains(stderr, "Could not read platform.yaml, so asking the Deploy gate at "+platform.DefaultDeployGateURL) {
		t.Errorf("stderr = %q, want it to say which gate it fell back to", stderr)
	}
}
