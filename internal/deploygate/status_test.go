package deploygate_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Itema-as/iidp/internal/platformstate"
)

// GET /v1/status/<app> is tested through the gate's HTTP boundary, like a
// deploy, against the fake GitHub (GET /repositories/{id} with the
// developer's token), a local Platform repository, and fakeCluster, an
// in-memory stand-in for the Kubernetes API's list calls.

const (
	readerToken   = "gho_reader"
	strangerToken = "gho_stranger"
	noPullToken   = "gho_nopull"
)

// fakeCluster answers List from objects kept per collection path, filtered
// by the label selector the way the API server does for k=v selectors.
type fakeCluster struct {
	mu      sync.Mutex
	objects map[string][]map[string]any
	lists   []string
}

func newFakeCluster() *fakeCluster {
	return &fakeCluster{objects: map[string][]map[string]any{}}
}

func (c *fakeCluster) add(path string, objs ...map[string]any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.objects[path] = append(c.objects[path], objs...)
}

func (c *fakeCluster) List(_ context.Context, path, selector string, into any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lists = append(c.lists, path+"?"+selector)
	var items []map[string]any
	for _, obj := range c.objects[path] {
		labels, _ := obj["metadata"].(map[string]any)["labels"].(map[string]any)
		ok := true
		for _, term := range strings.Split(selector, ",") {
			if k, v, found := strings.Cut(term, "="); found && labels[k] != v {
				ok = false
			}
		}
		if ok {
			items = append(items, obj)
		}
	}
	data, err := json.Marshal(items)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, into)
}

func (c *fakeCluster) listCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.lists)
}

func meta(name, namespace string, created time.Time, labels map[string]any) map[string]any {
	return map[string]any{"name": name, "namespace": namespace, "labels": labels, "creationTimestamp": created.Format(time.RFC3339)}
}

func argoApp(name, application, environment, namespace, sync, health string, urls ...string) map[string]any {
	labels := map[string]any{"iidp.itema.no/application": application}
	if environment != "" {
		labels["iidp.itema.no/environment"] = environment
	}
	return map[string]any{
		"metadata": meta(name, "argocd", at(1), labels),
		"spec":     map[string]any{"destination": map[string]any{"namespace": namespace}},
		"status": map[string]any{
			"sync":   map[string]any{"status": sync},
			"health": map[string]any{"status": health},
			"operationState": map[string]any{"phase": "Succeeded", "message": "successfully synced (all tasks run)",
				"startedAt": at(20).Format(time.RFC3339), "finishedAt": at(21).Format(time.RFC3339)},
			"summary": map[string]any{"externalURLs": urls},
		},
	}
}

func deploymentObj(namespace, application, instance, image string) map[string]any {
	selector := map[string]any{"app.kubernetes.io/name": application, "app.kubernetes.io/instance": instance}
	return map[string]any{
		"metadata": meta(instance, namespace, at(2), map[string]any{"iidp.itema.no/application": application,
			"app.kubernetes.io/name": application, "app.kubernetes.io/instance": instance}),
		"spec": map[string]any{
			"selector": map[string]any{"matchLabels": selector},
			"template": map[string]any{"spec": map[string]any{"containers": []any{map[string]any{"name": application, "image": image}}}},
		},
	}
}

func podObj(name, namespace string, labels map[string]any, phase string, ready bool, restarts int) map[string]any {
	readyStatus := "False"
	if ready {
		readyStatus = "True"
	}
	return map[string]any{
		"metadata": meta(name, namespace, at(3), labels),
		"status": map[string]any{
			"phase":             phase,
			"conditions":        []any{map[string]any{"type": "Ready", "status": readyStatus}},
			"containerStatuses": []any{map[string]any{"name": "app", "ready": ready, "restartCount": restarts}},
		},
	}
}

// jobObj is a finished Job: condition is Complete or Failed; "" is still
// running.
func jobObj(name, namespace string, labels map[string]any, created int, condition string) map[string]any {
	status := map[string]any{"startTime": at(created).Format(time.RFC3339)}
	if condition != "" {
		status["conditions"] = []any{map[string]any{"type": condition, "status": "True", "lastTransitionTime": at(created + 1).Format(time.RFC3339)}}
		if condition == "Complete" {
			status["completionTime"] = at(created + 1).Format(time.RFC3339)
		}
	}
	return map[string]any{"metadata": meta(name, namespace, at(created), labels), "status": status}
}

// at is a fixed time, n minutes into the test's day.
func at(n int) time.Time {
	return time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC).Add(time.Duration(n) * time.Minute)
}

// shopCluster is shop on the cluster: prod with Postgres (a migration) and
// a Scheduled task, staging with neither, a Preview Environment, and
// another Application's objects that must not show.
func shopCluster(prodTag, stagingTag string) *fakeCluster {
	c := newFakeCluster()
	apps := "/apis/argoproj.io/v1alpha1/namespaces/argocd/applications"
	c.add(apps,
		argoApp("shop-staging", "shop", "staging", "shop-staging", "Synced", "Healthy", "https://shop-staging.app.example.test/"),
		argoApp("shop-prod", "shop", "prod", "shop-prod", "Synced", "Healthy",
			"https://shop.app.example.test/", "http://shop.app.example.test/", "https://www.shop.example/"),
		// A Preview Environment, whose ArgoCD Application an
		// ApplicationSet generates: found by the same label, named from
		// its namespace.
		argoApp("shop-pr-7", "shop", "", "shop-pr-7", "OutOfSync", "Progressing", "https://shop-pr-7.app.example.test"),
		argoApp("other-prod", "other", "prod", "other-prod", "Synced", "Degraded"),
	)

	app := map[string]any{"iidp.itema.no/application": "shop"}
	web := func(instance string) map[string]any {
		return map[string]any{"iidp.itema.no/application": "shop", "app.kubernetes.io/name": "shop", "app.kubernetes.io/instance": instance}
	}
	with := func(base map[string]any, kv ...string) map[string]any {
		out := map[string]any{}
		for k, v := range base {
			out[k] = v
		}
		for i := 0; i+1 < len(kv); i += 2 {
			out[kv[i]] = kv[i+1]
		}
		return out
	}

	// prod: one ready pod that restarted twice, a pod from an old
	// ReplicaSet still starting, and a finished migration pod and a task
	// pod, which do not count among the Application's pods.
	c.add("/apis/apps/v1/namespaces/shop-prod/deployments", deploymentObj("shop-prod", "shop", "shop", "ghcr.io/itema-as/shop:"+prodTag))
	c.add("/api/v1/namespaces/shop-prod/pods",
		podObj("shop-abc", "shop-prod", web("shop"), "Running", true, 2),
		podObj("shop-def", "shop-prod", web("shop"), "Pending", false, 0),
		podObj("shop-migrate-x", "shop-prod", with(app, "app.kubernetes.io/component", "migration"), "Succeeded", false, 0),
		podObj("shop-report-1-x", "shop-prod", with(app, "app.kubernetes.io/component", "scheduled-task", "iidp.itema.no/task", "report",
			"batch.kubernetes.io/job-name", "shop-report-1"), "Running", true, 0),
	)
	c.add("/apis/batch/v1/namespaces/shop-prod/jobs",
		jobObj("shop-migrate", "shop-prod", with(app, "app.kubernetes.io/component", "migration"), 10, "Complete"),
		jobObj("shop-cleanup-1", "shop-prod", with(app, "app.kubernetes.io/component", "scheduled-task", "iidp.itema.no/task", "cleanup"), 30, "Complete"),
		jobObj("shop-cleanup-2", "shop-prod", with(app, "app.kubernetes.io/component", "scheduled-task", "iidp.itema.no/task", "cleanup"), 40, "Failed"),
		jobObj("shop-report-1", "shop-prod", with(app, "app.kubernetes.io/component", "scheduled-task", "iidp.itema.no/task", "report"), 45, ""),
	)
	c.add("/apis/batch/v1/namespaces/shop-prod/cronjobs",
		map[string]any{
			"metadata": meta("shop-cleanup", "shop-prod", at(2), with(app, "app.kubernetes.io/component", "scheduled-task", "iidp.itema.no/task", "cleanup")),
			"spec":     map[string]any{"schedule": "0 3 * * *"},
			"status":   map[string]any{"lastScheduleTime": at(40).Format(time.RFC3339)},
		},
		map[string]any{
			"metadata": meta("shop-report", "shop-prod", at(2), with(app, "app.kubernetes.io/component", "scheduled-task", "iidp.itema.no/task", "report")),
			"spec":     map[string]any{"schedule": "*/5 * * * *"},
			"status":   map[string]any{"lastScheduleTime": at(45).Format(time.RFC3339)},
		},
	)

	c.add("/apis/apps/v1/namespaces/shop-staging/deployments", deploymentObj("shop-staging", "shop", "shop-staging", "ghcr.io/itema-as/shop:"+stagingTag))
	c.add("/api/v1/namespaces/shop-staging/pods", podObj("shop-staging-a", "shop-staging", web("shop-staging"), "Running", true, 0))

	c.add("/apis/apps/v1/namespaces/shop-pr-7/deployments", deploymentObj("shop-pr-7", "shop", "shop-pr-7", "ghcr.io/itema-as/shop:0badc0ffee"))

	// Another Application's objects in shop's namespace are not shop's.
	c.add("/apis/batch/v1/namespaces/shop-prod/jobs", jobObj("other-migrate", "shop-prod",
		map[string]any{"iidp.itema.no/application": "other", "app.kubernetes.io/component": "migration"}, 50, "Failed"))
	return c
}

func valuesWith(environment, tag, size string) string {
	return fmt.Sprintf("application:\n  name: shop\nenvironment: %s\nimage:\n  repository: ghcr.io/itema-as/shop\n  tag: %q\nsize: %s\n", environment, tag, size)
}

// pushAt pushes files to the Platform repository as one commit made at when.
func pushAt(t *testing.T, url string, when time.Time, message string, files map[string]string) {
	t.Helper()
	t.Setenv("GIT_COMMITTER_DATE", when.Format(time.RFC3339))
	t.Setenv("GIT_AUTHOR_DATE", when.Format(time.RFC3339))
	pushCommit(t, url, message, files)
}

// shopStatusEnv is a gate with shop bound and deployed: prod was deployed
// 1.0.1 on the 21st (and changed size after), staging was deployed sha-1
// on the 23rd and sha-2 on the 24th, which the cluster has not caught up
// with yet.
func shopStatusEnv(t *testing.T) (*env, *fakeCluster) {
	t.Helper()
	e := newEnv(t)
	addApplication(t, e.platform, "shop", true, binding(shopRepoID, orgID))
	prod, staging := "applications/shop/prod/values.yaml", "applications/shop/staging/values.yaml"
	pushAt(t, e.platform, time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC), "Deploy shop prod 1.0.0", map[string]string{prod: valuesWith("prod", "1.0.0", "small")})
	pushAt(t, e.platform, time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC), "Deploy shop prod 1.0.1", map[string]string{prod: valuesWith("prod", "1.0.1", "small")})
	pushAt(t, e.platform, time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC), "iidp app add-capability shop size", map[string]string{prod: valuesWith("prod", "1.0.1", "medium")})
	pushAt(t, e.platform, time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC), "Deploy shop staging sha-1", map[string]string{staging: valuesWith("staging", "sha-1", "small")})
	pushAt(t, e.platform, time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC), "Deploy shop staging sha-2", map[string]string{staging: valuesWith("staging", "sha-2", "small")})
	pushAt(t, e.platform, time.Date(2026, 9, 24, 11, 0, 0, 0, time.UTC), "Link ArgoCD and Grafana", map[string]string{"platform.yaml": "baseDomain: app.example.test\nchartVersion: 0.3.1\nargocdURL: https://argocd.example.test/\ngrafanaURL: https://itema.grafana.net\n"})

	cluster := shopCluster("1.0.1", "sha-1")
	e.gate.Cluster = cluster
	e.github.readers[readerToken] = map[int64]bool{shopRepoID: true}
	e.github.readers[strangerToken] = map[int64]bool{}
	e.github.readers[noPullToken] = map[int64]bool{shopRepoID: false}
	return e, cluster
}

func (e *env) status(token, application string) (int, []byte) {
	e.t.Helper()
	req, _ := http.NewRequest(http.MethodGet, e.srv.URL+"/v1/status/"+application, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body
}

func TestStatusShowsEveryEnvironmentToAReaderOfTheRepository(t *testing.T) {
	e, cluster := shopStatusEnv(t)

	code, body := e.status(readerToken, "shop")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", code, body)
	}
	var got platformstate.Status
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.Application != "shop" || got.Repository != "Itema-as/shop-renamed" {
		t.Errorf("application, repository = %q, %q, want shop and GitHub's current name", got.Application, got.Repository)
	}
	var names []string
	for _, env := range got.Environments {
		names = append(names, env.Name)
	}
	if strings.Join(names, " ") != "prod staging pr-7" {
		t.Fatalf("Environments = %v, want prod, staging and the preview pr-7, and not the other Application's", names)
	}

	prod, staging, preview := got.Environments[0], got.Environments[1], got.Environments[2]
	if prod.Namespace != "shop-prod" || prod.ArgoCD == nil || prod.ArgoCD.Application != "shop-prod" || prod.ArgoCD.Sync != "Synced" || prod.ArgoCD.Health != "Healthy" {
		t.Errorf("prod ArgoCD = %+v in %q", prod.ArgoCD, prod.Namespace)
	}
	if op := prod.ArgoCD.Operation; op == nil || op.Phase != "Succeeded" || op.FinishedAt == nil || !op.FinishedAt.Equal(at(21)) {
		t.Errorf("prod's last operation = %+v, want Succeeded at %s", op, at(21))
	}
	if img := prod.Image; img == nil || img.Repository != "ghcr.io/itema-as/shop" || img.Tag != "1.0.1" || img.DeployedAt == nil ||
		!img.DeployedAt.Equal(time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("prod image = %+v, want 1.0.1 deployed with the commit that set it on the 21st", img)
	}
	if prod.Pods != (platformstate.Pods{Ready: 1, Total: 2, Restarts: 2}) {
		t.Errorf("prod pods = %+v, want 1 of 2 ready, 2 restarts, and no Job pods counted", prod.Pods)
	}
	if m := prod.Migration; m == nil || m.Result != "succeeded" || m.FinishedAt == nil || !m.FinishedAt.Equal(at(11)) {
		t.Errorf("prod migration = %+v, want shop's own, succeeded", m)
	}
	if len(prod.Tasks) != 2 {
		t.Fatalf("prod tasks = %+v, want cleanup and report", prod.Tasks)
	}
	cleanup, report := prod.Tasks[0], prod.Tasks[1]
	if cleanup.Name != "cleanup" || cleanup.Schedule != "0 3 * * *" || cleanup.LastScheduleTime == nil || !cleanup.LastScheduleTime.Equal(at(40)) ||
		cleanup.LastRun == nil || cleanup.LastRun.Result != "failed" || !cleanup.LastRun.FinishedAt.Equal(at(41)) {
		t.Errorf("cleanup = %+v (last run %+v), want its newest run, failed", cleanup, cleanup.LastRun)
	}
	if report.Name != "report" || report.LastRun == nil || report.LastRun.Result != "running" || report.LastRun.FinishedAt != nil {
		t.Errorf("report = %+v (last run %+v), want a run still going", report, report.LastRun)
	}
	if strings.Join(prod.Addresses, " ") != "https://shop.app.example.test https://www.shop.example" {
		t.Errorf("prod addresses = %v, want each host once, https", prod.Addresses)
	}
	if prod.Links == nil || prod.Links.ArgoCD != "https://argocd.example.test/applications/argocd/shop-prod" ||
		!strings.HasPrefix(prod.Links.Grafana, "https://itema.grafana.net/explore?") || !strings.Contains(prod.Links.Grafana, "shop-prod") {
		t.Errorf("prod links = %+v", prod.Links)
	}

	// The cluster still runs sha-1: when that was deployed, not sha-2.
	if img := staging.Image; img == nil || img.Tag != "sha-1" || img.DeployedAt == nil || !img.DeployedAt.Equal(time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("staging image = %+v, want sha-1 deployed on the 23rd", img)
	}
	if preview.Namespace != "shop-pr-7" || preview.ArgoCD.Sync != "OutOfSync" || preview.Image == nil || preview.Image.DeployedAt != nil {
		t.Errorf("preview = %+v, image %+v: want its own state and no deploy time, since it has no Platform repository history", preview, preview.Image)
	}

	// The shape of an Environment without Postgres or tasks: the keys are
	// there, empty, so a script can rely on them.
	var raw struct {
		Environments []map[string]json.RawMessage `json:"environments"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{"migration": "null", "tasks": "[]", "name": `"staging"`} {
		if got := string(raw.Environments[1][key]); got != want {
			t.Errorf("staging %s = %s, want %s", key, got, want)
		}
	}
	for _, key := range []string{"name", "namespace", "argocd", "image", "pods", "migration", "tasks", "addresses", "links"} {
		if _, ok := raw.Environments[0][key]; !ok {
			t.Errorf("prod has no %q key: %s", key, body)
		}
	}

	// GitHub was asked with the developer's own token, and the App was
	// never used.
	if strings.Join(e.github.repositoryTokens, ",") != readerToken {
		t.Errorf("GET /repositories tokens = %v, want the developer's only", e.github.repositoryTokens)
	}
	if n := e.github.requestCount("token") + e.github.requestCount("app"); n != 0 {
		t.Errorf("the App was used %d times for a status call", n)
	}
	if cluster.listCount() == 0 {
		t.Errorf("the cluster was not read")
	}
}

func TestStatusRefusesWithoutAccess(t *testing.T) {
	for _, tc := range []struct {
		name, token, application string
		want                     int
		says                     string
	}{
		{"no token", "", "shop", http.StatusUnauthorized, "no GitHub token"},
		{"a token GitHub refuses", "gho_revoked", "shop", http.StatusUnauthorized, "run gh auth login"},
		{"a user who cannot see the private repository (GitHub's 404)", strangerToken, "shop", http.StatusForbidden, "you cannot read shop's Application repository (Itema-as/shop, repository id 812345678)"},
		{"a user without pull permission", noPullToken, "shop", http.StatusForbidden, "you cannot read"},
		{"an unknown Application", readerToken, "nosuchapp", http.StatusNotFound, "there is no Application nosuchapp on the Platform"},
		{"an invalid name", readerToken, "Shop", http.StatusBadRequest, "invalid Application name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, cluster := shopStatusEnv(t)
			code, body := e.status(tc.token, tc.application)
			var refusal struct{ Error string }
			_ = json.Unmarshal(body, &refusal)
			if code != tc.want || !strings.Contains(refusal.Error, tc.says) {
				t.Errorf("status = %d %q, want %d saying %q", code, refusal.Error, tc.want, tc.says)
			}
			if n := cluster.listCount(); n != 0 {
				t.Errorf("the cluster was read %d times for a refused call", n)
			}
		})
	}
}

func TestStatusRefusesAnApplicationWithoutABinding(t *testing.T) {
	for name, repositoryYAML := range map[string]string{
		"no binding":       "",
		"a broken binding": "repositoryId: \"812345678\"\n",
		"another org":      binding(shopRepoID, 42),
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			addApplication(t, e.platform, "shop", false, repositoryYAML)
			cluster := shopCluster("1.0.1", "sha-1")
			e.gate.Cluster = cluster
			e.github.readers[readerToken] = map[int64]bool{shopRepoID: true}

			code, body := e.status(readerToken, "shop")
			if code != http.StatusForbidden || !strings.Contains(string(body), "iidp app bind shop") {
				t.Errorf("status = %d %s, want 403 saying how to bind it", code, body)
			}
			if n := e.github.requestCount("repositories"); n != 0 {
				t.Errorf("GitHub was asked %d times about an unbound Application", n)
			}
			if n := cluster.listCount(); n != 0 {
				t.Errorf("the cluster was read %d times", n)
			}
		})
	}
}

func TestStatusShowsAnEnvironmentArgoCDHasNotPickedUpYet(t *testing.T) {
	e := newEnv(t)
	addApplication(t, e.platform, "shop", true, binding(shopRepoID, orgID))
	cluster := newFakeCluster()
	cluster.add("/apis/argoproj.io/v1alpha1/namespaces/argocd/applications", argoApp("shop-prod", "shop", "prod", "shop-prod", "Synced", "Healthy"))
	e.gate.Cluster = cluster
	e.github.readers[readerToken] = map[int64]bool{shopRepoID: true}

	code, body := e.status(readerToken, "shop")
	if code != http.StatusOK {
		t.Fatalf("status = %d: %s", code, body)
	}
	var got platformstate.Status
	_ = json.Unmarshal(body, &got)
	if len(got.Environments) != 2 || got.Environments[1].Name != "staging" || got.Environments[1].ArgoCD != nil {
		t.Fatalf("Environments = %+v, want staging listed with no ArgoCD state", got.Environments)
	}
	// prod renders nothing before its first image: no image, no pods.
	if prod := got.Environments[0]; prod.Image != nil || prod.Pods.Total != 0 || prod.Migration != nil {
		t.Errorf("unreleased prod = %+v, want no image, pods or migration", prod)
	}
}

func TestStatusIsUnavailableWithoutClusterAccess(t *testing.T) {
	e, _ := shopStatusEnv(t)
	e.gate.Cluster = nil
	code, body := e.status(readerToken, "shop")
	if code != http.StatusServiceUnavailable || !strings.Contains(string(body), "cannot read the cluster") {
		t.Errorf("status = %d %s, want 503", code, body)
	}
}
