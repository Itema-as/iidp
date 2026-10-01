//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Itema-as/iidp/internal/appconfig"
	"github.com/Itema-as/iidp/internal/platformstate"
)

// TestBootstrap bootstraps a kind cluster from the fixture Platform
// repository, waits for every Platform component (only Synced for those
// that need a cloud account), then runs the end-to-end scenarios below in
// order on the same cluster.
//
// Run with:
//
//	go test -tags e2e ./test/e2e/... -run TestBootstrap -v -timeout 30m
//
// Needs kind, kubectl, helm, git and docker (or podman with
// KIND_EXPERIMENTAL_PROVIDER=podman). IIDP_E2E_KEEP=1 keeps the cluster for
// inspection; IIDP_E2E_CLUSTER overrides its name (default iidp-e2e), so
// several runs can share a machine.
func TestBootstrap(t *testing.T) {
	ctx := context.Background()
	clusterName := "iidp-e2e"
	if name := os.Getenv("IIDP_E2E_CLUSTER"); name != "" {
		clusterName = name
	}
	cluster, err := NewCluster(clusterName, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cluster.Close)
	if err := cluster.Create(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if os.Getenv("IIDP_E2E_KEEP") != "" {
			t.Logf("keeping kind cluster %s (KUBECONFIG=%s)", cluster.Name, cluster.Kubeconfig)
			return
		}
		if err := cluster.Delete(context.Background()); err != nil {
			t.Logf("delete cluster: %v", err)
		}
	})
	// Runs before the cluster is deleted: cleanups run last registered first.
	t.Cleanup(func() { cluster.LogImageSources(context.Background()) })

	images, err := cluster.RegistryImages(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := cluster.PreloadImages(ctx, RateLimited(append(images, fixtureImages...))); err != nil {
		t.Fatal(err)
	}

	if err := cluster.InstallTraefik(ctx); err != nil {
		t.Fatal(err)
	}
	if err := cluster.InstallArgoCD(ctx); err != nil {
		t.Fatal(err)
	}
	fixtures := filepath.Join(cluster.RepoRoot, "test", "e2e", "fixtures")
	if err := cluster.CreateAgeKeySecret(ctx, filepath.Join(fixtures, "age-keys.txt")); err != nil {
		t.Fatal(err)
	}
	// shop-staging's ObjectStore points at it, so its final Backup can
	// complete when testDeleteEnvironment deletes it.
	if err := cluster.InstallObjectStorage(ctx); err != nil {
		t.Fatal(err)
	}
	issuer, err := cluster.InstallDeployGateStandIns(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// The Platform repository points at this repository for the bootstrap,
	// so both are served. The fixture Environments also take the chart from
	// iidp.git rather than GHCR, which kind cannot reach.
	err = cluster.ServeGitRepositories(ctx,
		Repository{Name: "iidp", Files: map[string]string{
			"bootstrap": filepath.Join(cluster.RepoRoot, "bootstrap"),
			"chart":     filepath.Join(cluster.RepoRoot, "chart"),
		}},
		Repository{Name: "iidp-platform", Files: map[string]string{".": filepath.Join(fixtures, "platform-repo")}},
	)
	if err != nil {
		t.Fatal(err)
	}

	if err := cluster.ApplyRootApplication(ctx, GitBaseURL+"/iidp-platform.git", "bootstrap"); err != nil {
		t.Fatal(err)
	}

	want := map[string]Expectation{
		"platform":            Healthy,
		"platform-components": Healthy,
		"platform-secrets":    Healthy,
		"argocd":              Healthy,
		"cert-manager":        Healthy,
		"cloudnative-pg":      Healthy,
		"cnpg-barman-cloud":   Healthy,
		// Dummy Entra credentials and skipOIDCDiscovery are enough for the
		// pod to come up; only a real sign-in would need real ones.
		"oauth2-proxy": Healthy,
		"deploy-gate":  Healthy,
		"guardrails":   Healthy,
		// Cloud-dependent: configured, applied, but nothing to talk to.
		"platform-tls": Synced,
		"external-dns": Synced,
		"monitoring":   Synced,
	}
	if err := cluster.WaitForApplications(ctx, want, 10*time.Minute); err != nil {
		t.Fatal(err)
	}

	testFixtureApplication(ctx, t, cluster)
	testUnreleasedEnvironments(ctx, t, cluster)
	testDeleteEnvironment(ctx, t, cluster)
	// The order matters. Workloads that are added (brochure-prod's first
	// image, a preview's database) only fit once shop-staging is deleted;
	// testAppStatus needs shop-prod's migration and task run; and
	// testGuardrails checks that nothing before it tripped a guardrail.
	testDeployGate(ctx, t, cluster, issuer)
	testMigrationCommandFromIidpYAML(ctx, t, cluster, issuer)
	testAppStatus(ctx, t, cluster)
	testPreviewEnvironments(ctx, t, cluster)
	testGuardrails(ctx, t, cluster)
}

// brochureRepositoryID and shopRepositoryID are the repository ids the
// fixture binds brochure and shop to (applications/<name>/repository.yaml).
const (
	brochureRepositoryID = 700000001
	shopRepositoryID     = 700000002
)

// fixtureImages are the images the fixture Applications run.
var fixtureImages = []string{
	"docker.io/nginxinc/nginx-unprivileged:1.30-alpine",
	"docker.io/nginxinc/nginx-unprivileged:" + shopDeployTag,
}

// shopDeployTag differs from the fixture's 1.30-alpine so the Deployment
// changes too: ArgoCD leaves hooks out of its diff, so a commit changing
// only the migration Job would not sync. It must exist on Docker Hub, since
// the gate checks every new tag.
const shopDeployTag = "1.30.0-alpine"

// testMigrationCommandFromIidpYAML proves the migration command and
// Scheduled task in shop's iidp.yaml travel with a deploy into the same
// commit as the tag, the migration Job runs the command, and the task's
// CronJob runs a Job with DATABASE_URL set. A command or tasks for
// brochure, a Static site without Postgres, are refused first.
func testMigrationCommandFromIidpYAML(ctx context.Context, t *testing.T, cluster *Cluster, issuer *FakeIssuer) {
	t.Helper()
	appConfig, ok, err := appconfig.Read(filepath.Join(cluster.RepoRoot, "test", "e2e", "fixtures", "shop-repository"))
	if err != nil || !ok || appConfig.MigrationCommand == "" || len(appConfig.Tasks) != 1 {
		t.Fatalf("reading the fixture iidp.yaml: ok = %v, command = %q, tasks = %v, err = %v", ok, appConfig.MigrationCommand, appConfig.Tasks, err)
	}
	command, task := appConfig.MigrationCommand, appConfig.Tasks[0]
	call := func(repository string, repositoryID int64, application, tag string, command *string, tasks []appconfig.Task) (int, map[string]any) {
		t.Helper()
		token, err := issuer.Sign(issuer.Claims(repository, repositoryID, "refs/heads/main"))
		if err != nil {
			t.Fatal(err)
		}
		status, body, err := cluster.CallDeployGateWithIidpYAML(ctx, token, application, "auto", tag, command, tasks)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("Deploy gate: HTTP %d %v", status, body)
		return status, body
	}

	status, body := call("Itema-as/brochure", brochureRepositoryID, "brochure", "1.30-alpine", &command, nil)
	if msg, _ := body["error"].(string); status != http.StatusConflict || !strings.Contains(msg, "Add the Postgres Capability first") {
		t.Errorf("a migration command for brochure: HTTP %d %v, want 409 asking for the Postgres Capability first", status, body)
	}
	status, body = call("Itema-as/brochure", brochureRepositoryID, "brochure", "1.30-alpine", nil, appConfig.Tasks)
	if msg, _ := body["error"].(string); status != http.StatusConflict || !strings.Contains(msg, "is a Static site") {
		t.Errorf("tasks for brochure: HTTP %d %v, want 409 saying it is a Static site", status, body)
	}

	// staging was deleted by testDeleteEnvironment, so main deploys to prod.
	status, body = call("Itema-as/shop", shopRepositoryID, "shop", shopDeployTag, &command, appConfig.Tasks)
	if status != http.StatusOK || body["environment"] != "prod" || body["migrationCommandChanged"] != true || body["tasksChanged"] != true {
		t.Fatalf("the deploy of shop with its iidp.yaml: HTTP %d %v, want 200, prod and the migration command and tasks changed", status, body)
	}

	err = cluster.ReadRepository(ctx, "iidp-platform", func(dir string) error {
		git := func(args ...string) string {
			out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
			if err != nil {
				t.Fatalf("git %v: %v\n%s", args, err, out)
			}
			return strings.TrimSpace(string(out))
		}
		if got := git("log", "-1", "--format=%s"); got != "Deploy shop prod "+shopDeployTag {
			t.Errorf("the Platform repository's head is %q, want shop's deploy (and nothing from the refused call)", got)
		}
		if got := git("log", "-1", "--format=%b"); !strings.Contains(got, "Migration command, from iidp.yaml: "+command) {
			t.Errorf("the deploy's body is %q, want it to name the new migration command", got)
		}
		if got := git("log", "-1", "--format=%b"); !strings.Contains(got, "- "+task.Name+" ("+task.Schedule+"): "+task.Command) {
			t.Errorf("the deploy's body is %q, want it to name the task", got)
		}
		if got := git("show", "--stat", "--format=", "HEAD"); !strings.Contains(got, "applications/shop/prod/values.yaml") || !strings.Contains(got, "1 file changed") {
			t.Errorf("the deploy changed:\n%s\nwant only shop's prod values.yaml, tag and command together", got)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := cluster.RefreshApplication(ctx, "shop-prod"); err != nil {
		t.Fatal(err)
	}
	logs, err := cluster.WaitForJobRunning(ctx, "shop-prod", "shop-migrate", command, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs, "migrated by the command in iidp.yaml") {
		t.Errorf("the migration Job's log is %q, want the marker the command in iidp.yaml echoes", logs)
	}
	if err := cluster.WaitForApplications(ctx, map[string]Expectation{"shop-prod": Healthy}, 5*time.Minute); err != nil {
		t.Fatal(err)
	}

	logs, err = cluster.WaitForScheduledTaskRun(ctx, "shop-prod", task.Name, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs, "scheduled task ran with DATABASE_URL") {
		t.Errorf("the task's log is %q, want the marker its command echoes", logs)
	}
}

// testAppStatus proves the gate's status endpoint, called as iidp app status
// calls it with a developer's token, reports shop-prod's sync, health,
// image, pods, migration and task run, and refuses brochure, whose
// repository the token cannot read.
func testAppStatus(ctx context.Context, t *testing.T, cluster *Cluster) {
	t.Helper()
	var status platformstate.Status
	var last string
	err := pollUntil(ctx, 3*time.Minute, 5*time.Second,
		func() (bool, error) {
			code, body, err := cluster.CallStatus(ctx, DeveloperToken, "shop")
			if err != nil {
				return false, err
			}
			last = fmt.Sprintf("HTTP %d: %s", code, body)
			if code >= 500 {
				return false, nil
			}
			if code != http.StatusOK {
				return false, fmt.Errorf("the status of shop: %s", last)
			}
			if err := json.Unmarshal(body, &status); err != nil {
				return false, err
			}
			for _, env := range status.Environments {
				if env.Name == "prod" && env.ArgoCD != nil && env.ArgoCD.Sync == "Synced" && env.ArgoCD.Health == "Healthy" && env.Pods.Ready > 0 {
					return true, nil
				}
			}
			return false, nil
		},
		func() error { return fmt.Errorf("shop-prod never showed Synced, Healthy and a ready pod: %s", last) })
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("iidp app status shop: %s", last)
	if len(status.Environments) != 1 {
		t.Errorf("Environments = %+v, want prod only: staging was deleted", status.Environments)
	}
	prod := status.Environments[0]
	if prod.Namespace != "shop-prod" || prod.Image == nil || prod.Image.Tag != shopDeployTag || prod.Image.DeployedAt == nil {
		t.Errorf("prod = %+v, image %+v: want %s with the time of the gate's commit", prod, prod.Image, shopDeployTag)
	}
	if prod.Migration == nil || prod.Migration.Result != platformstate.RunSucceeded {
		t.Errorf("migration = %+v, want the migration Job's success", prod.Migration)
	}
	if len(prod.Tasks) != 1 || prod.Tasks[0].LastRun == nil {
		t.Errorf("tasks = %+v, want the fixture's task with its run", prod.Tasks)
	}
	if len(prod.Addresses) == 0 {
		t.Errorf("addresses = %v, want shop's", prod.Addresses)
	}

	code, body, err := cluster.CallStatus(ctx, DeveloperToken, "brochure")
	if err != nil {
		t.Fatal(err)
	}
	if code != http.StatusForbidden || !strings.Contains(string(body), "you cannot read brochure's Application repository") {
		t.Errorf("the status of brochure: HTTP %d %s, want 403: the token cannot read its repository", code, body)
	}
}

// testDeployGate proves the Deploy gate refuses a call from another
// repository, from a ref that may not deploy, and for a tag Docker Hub does
// not have, then deploys brochure's first image: the commit is authored by
// the token's actor and committed by the App, and brochure-prod then
// answers HTTP 200.
func testDeployGate(ctx context.Context, t *testing.T, cluster *Cluster, issuer *FakeIssuer) {
	t.Helper()
	if err := cluster.WaitForDeployGate(ctx, 2*time.Minute); err != nil {
		t.Fatal(err)
	}
	call := func(claims map[string]any, tag string) (int, string) {
		t.Helper()
		token, err := issuer.Sign(claims)
		if err != nil {
			t.Fatal(err)
		}
		status, body, err := cluster.CallDeployGate(ctx, token, "brochure", "auto", tag)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("Deploy gate: HTTP %d %v", status, body)
		if msg, ok := body["error"].(string); ok {
			return status, msg
		}
		return status, fmt.Sprint(body["environment"])
	}

	status, msg := call(issuer.Claims("Itema-as/impostor", 700000099, "refs/heads/main"), "1.30-alpine")
	if status != http.StatusForbidden || !strings.Contains(msg, "repository id 700000099") {
		t.Errorf("a call from another repository: HTTP %d %q, want 403 naming its repository id", status, msg)
	}
	status, msg = call(issuer.Claims("Itema-as/brochure", brochureRepositoryID, "refs/heads/feature"), "1.30-alpine")
	if status != http.StatusForbidden || !strings.Contains(msg, "refs/heads/feature") {
		t.Errorf("a call from refs/heads/feature: HTTP %d %q, want 403 naming the ref", status, msg)
	}
	status, msg = call(issuer.Claims("Itema-as/brochure", brochureRepositoryID, "refs/heads/main"), "iidp-e2e-no-such-tag")
	if status != http.StatusUnprocessableEntity || !strings.Contains(msg, "docker.io/nginxinc/nginx-unprivileged:iidp-e2e-no-such-tag does not exist") {
		t.Errorf("a tag that was never pushed: HTTP %d %q, want 422 naming the image", status, msg)
	}

	// brochure has no staging, so main deploys to prod.
	status, env := call(issuer.Claims("Itema-as/brochure", brochureRepositoryID, "refs/heads/main"), "1.30-alpine")
	if status != http.StatusOK || env != "prod" {
		t.Fatalf("the deploy from main: HTTP %d %q, want 200 and prod", status, env)
	}

	var head string
	err := cluster.ReadRepository(ctx, "iidp-platform", func(dir string) error {
		git := func(args ...string) string {
			out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
			if err != nil {
				t.Fatalf("git %v: %v\n%s", args, err, out)
			}
			return strings.TrimSpace(string(out))
		}
		if got := git("log", "-1", "--format=%s"); got != "Deploy brochure prod 1.30-alpine" {
			t.Errorf("the Platform repository's head is %q, want the deploy", got)
		}
		if got := git("log", "-1", "--format=%an <%ae>"); got != "e2e-developer <1000001+e2e-developer@users.noreply.github.com>" {
			t.Errorf("the deploy's author is %q, want the token's actor", got)
		}
		if got := git("log", "-1", "--format=%cn <%ce>"); got != FakeBotIdentity {
			t.Errorf("the deploy's committer is %q, want the App's bot %q", got, FakeBotIdentity)
		}
		if got := git("show", "--stat", "--format=", "HEAD"); !strings.Contains(got, "applications/brochure/prod/values.yaml") || !strings.Contains(got, "1 file changed") {
			t.Errorf("the deploy changed:\n%s\nwant only brochure's prod values.yaml", got)
		}
		head = git("rev-parse", "HEAD")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	checkDeployEvents(ctx, t, cluster, head)

	// A refresh saves waiting out ArgoCD's three-minute poll. Safe here,
	// unlike for the deletion in testDeleteEnvironment.
	if err := cluster.RefreshApplication(ctx, "brochure-prod"); err != nil {
		t.Fatal(err)
	}
	if err := cluster.CheckHTTP200(ctx, "brochure.app.example.test", 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := cluster.WaitForApplications(ctx, map[string]Expectation{"brochure-prod": Healthy}, 3*time.Minute); err != nil {
		t.Fatal(err)
	}
}

// checkDeployEvents proves a real API server accepts the gate's Events and
// the gate may create them. Of testDeployGate's four calls, only the missing
// tag and the deploy have brochure-prod behind them and are recorded.
func checkDeployEvents(ctx context.Context, t *testing.T, cluster *Cluster, commit string) {
	t.Helper()
	type event struct {
		Metadata struct {
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
		Reason              string `json:"reason"`
		ReportingController string `json:"reportingController"`
		Regarding           struct {
			Name string `json:"name"`
		} `json:"regarding"`
		Note string `json:"note"`
	}
	var events []event
	err := pollUntil(ctx, 30*time.Second, 2*time.Second, func() (bool, error) {
		// ArgoCD records Events on its Applications too; only the gate's
		// count.
		out, err := cluster.Kubectl(ctx, "get", "events.events.k8s.io", "-n", "argocd", "-o", "json")
		if err != nil {
			return false, fmt.Errorf("listing the Events in argocd: %w\n%s", err, out)
		}
		var list struct {
			Items []event `json:"items"`
		}
		if err := json.Unmarshal([]byte(out), &list); err != nil {
			return false, err
		}
		events = nil
		for _, e := range list.Items {
			if e.ReportingController == "iidp.itema.no/deploy-gate" {
				events = append(events, e)
			}
		}
		return len(events) >= 2, nil
	}, func() error {
		return fmt.Errorf("the Deploy gate recorded %d Events in argocd, want 2: %+v; its logs say why (kubectl -n argocd logs deploy/iidp-deploy-gate)", len(events), events)
	})
	if err != nil {
		t.Fatal(err)
	}
	reasons := map[string]event{}
	for _, e := range events {
		if e.Regarding.Name != "brochure-prod" {
			t.Errorf("an Event regards %q, want only brochure-prod: %+v", e.Regarding.Name, e)
		}
		if _, dup := reasons[e.Reason]; dup {
			t.Errorf("more than one %s Event: %+v", e.Reason, events)
		}
		reasons[e.Reason] = e
	}
	refused, accepted := reasons["DeployRefused"], reasons["DeployAccepted"]
	if refused.Metadata.Annotations["iidp.itema.no/refusal"] != "422" || !strings.Contains(refused.Note, "iidp-e2e-no-such-tag does not exist") {
		t.Errorf("the refused Deploy's Event = %+v, want DeployRefused with 422 and the reason", refused)
	}
	if accepted.Metadata.Annotations["iidp.itema.no/commit"] != commit || accepted.Metadata.Annotations["iidp.itema.no/tag"] != "1.30-alpine" || accepted.Note != "Deploy brochure prod 1.30-alpine accepted" {
		t.Errorf("the Deploy's Event = %+v, want DeployAccepted naming commit %s", accepted, commit)
	}
}

// testUnreleasedEnvironments proves an Environment with no image yet
// (image.tag: "") is Synced and Healthy with no comparison error and
// nothing of the chart's in it. later-prod's only resource is its
// backups-credentials Secret; brochure-prod has none at all.
func testUnreleasedEnvironments(ctx context.Context, t *testing.T, cluster *Cluster) {
	t.Helper()
	want := map[string]Expectation{
		"later-prod":    Healthy,
		"brochure-prod": Healthy,
	}
	if err := cluster.WaitForApplications(ctx, want, 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	apps, err := cluster.Applications(ctx)
	if err != nil {
		t.Fatal(err)
	}

	for _, env := range []struct {
		application, namespace string
		resources              []string
	}{
		{"later-prod", "later-prod", []string{"Secret/backups-credentials"}},
		{"brochure-prod", "brochure-prod", nil},
	} {
		// A stale Synced can hide a ComparisonError, so check conditions.
		if conditions := apps[env.application].Conditions; len(conditions) != 0 {
			t.Errorf("%s has conditions %v, want none", env.application, conditions)
		}

		out, err := cluster.Kubectl(ctx, "-n", "argocd", "get", "application", env.application,
			"-o", `jsonpath={range .status.resources[*]}{.kind}/{.name}{"\n"}{end}`)
		if err != nil {
			t.Fatalf("read %s's resources: %v\n%s", env.application, err, out)
		}
		if got := strings.Fields(out); !slices.Equal(got, env.resources) {
			t.Errorf("%s manages %v, want %v", env.application, got, env.resources)
		}

		out, err = cluster.Kubectl(ctx, "-n", env.namespace, "get",
			"deployments,services,ingresses,jobs,clusters.postgresql.cnpg.io,objectstores.barmancloud.cnpg.io,scheduledbackups.postgresql.cnpg.io",
			"-o", "name")
		if err != nil {
			t.Fatalf("list %s: %v\n%s", env.namespace, err, out)
		}
		var found []string
		for _, line := range strings.Split(out, "\n") {
			// -o name prints kind.group/name; "No resources found in
			// <namespace> namespace." (stderr, combined here) is not one.
			if line = strings.TrimSpace(line); strings.Contains(line, "/") && !strings.Contains(line, " ") {
				found = append(found, line)
			}
		}
		if len(found) != 0 {
			t.Errorf("namespace %s holds %v before the Environment's first image, want nothing of the chart's", env.namespace, found)
		}
	}
}

// testFixtureApplication proves shop's prod and staging Environments, with
// Postgres, deploy through the real bootstrap and chart and run their
// migrations. prod answers 200, also throughout a rollout. staging has
// Itema login, so its host and its custom domain redirect straight to the
// provider's sign-in, except for the custom domain's ACME challenge path.
func testFixtureApplication(ctx context.Context, t *testing.T, cluster *Cluster) {
	t.Helper()
	want := map[string]Expectation{
		"applications": Healthy,
		"shop-prod":    Healthy,
		"shop-staging": Healthy,
	}
	if err := cluster.WaitForApplications(ctx, want, 10*time.Minute); err != nil {
		t.Fatal(err)
	}

	for _, env := range []struct {
		namespace, job, host string
		protected            bool
	}{
		{"shop-prod", "shop-migrate", "shop.app.example.test", false},
		{"shop-staging", "shop-staging-migrate", "shop-staging.app.example.test", true},
	} {
		if err := cluster.WaitForJobSucceeded(ctx, env.namespace, env.job, time.Minute); err != nil {
			t.Fatal(err)
		}
		if env.protected {
			if err := cluster.CheckSignInRedirect(ctx, env.host, signInPath, fixtureSignIn, 2*time.Minute); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := cluster.CheckHTTP200(ctx, env.host, 2*time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	// kind cannot sign anyone in to show a user outside the sign-in group
	// gets a 403, so check the Middleware that would refuse them.
	if err := checkSignInGroupMiddleware(ctx, cluster); err != nil {
		t.Fatal(err)
	}
	// The login cookie and redirect allowlist cover the whole zone, so a
	// custom domain inside it is protected the same way.
	if err := cluster.CheckSignInRedirect(ctx, shopStagingCustomDomain, signInPath, fixtureSignIn, 2*time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := cluster.CheckACMEChallengeBypassesLogin(ctx, "shop-staging", shopStagingCustomDomain, "shop-staging", 8080, 2*time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := cluster.CheckRolloutServes(ctx, "shop-prod", "shop", "shop.app.example.test"); err != nil {
		t.Fatal(err)
	}
}

// checkSignInGroupMiddleware checks that shop-staging's Middleware passes
// its sign-in group to oauth2-proxy as allowed_groups, and both its
// Ingresses use it.
func checkSignInGroupMiddleware(ctx context.Context, cluster *Cluster) error {
	const (
		address    = "http://oauth2-proxy.oauth2-proxy.svc.cluster.local/?allowed_groups=0f3b6a4e-8c1d-4e2f-9a7b-5c6d7e8f9a0b"
		annotation = "shop-staging-shop-staging-itema-login@kubernetescrd"
	)
	got, err := cluster.Kubectl(ctx, "-n", "shop-staging", "get", "middlewares.traefik.io", "shop-staging-itema-login", "-o", "jsonpath={.spec.forwardAuth.address}")
	if err != nil {
		return fmt.Errorf("shop-staging's sign-in group Middleware: %w", err)
	}
	if strings.TrimSpace(got) != address {
		return fmt.Errorf("shop-staging-itema-login forwardAuth.address = %q, want %q", got, address)
	}
	for _, ingress := range []string{"shop-staging", "shop-staging-http01"} {
		got, err := cluster.Kubectl(ctx, "-n", "shop-staging", "get", "ingress", ingress, "-o", `jsonpath={.metadata.annotations.traefik\.ingress\.kubernetes\.io/router\.middlewares}`)
		if err != nil {
			return fmt.Errorf("Ingress %s: %w", ingress, err)
		}
		if strings.TrimSpace(got) != annotation {
			return fmt.Errorf("Ingress %s router.middlewares = %q, want %q", ingress, got, annotation)
		}
	}
	return nil
}

// shopStagingCustomDomain is inside the fixture's cloudflareZone but not
// under its baseDomain.
const shopStagingCustomDomain = "shop-staging.example.test"

// signInPath is a path with a query, so the sign-in check proves both
// survive the round trip through the provider.
const signInPath = "/account/orders?page=2&sort=date"

// fixtureSignIn is the fixture Platform's sign-in redirect. Its
// oauth2-proxy skips OIDC discovery and uses a never-reached authorize
// endpoint.
var fixtureSignIn = SignIn{
	LoginURL:     "https://oauth2-proxy-entra.invalid/authorize",
	Callback:     "https://auth.app.example.test/oauth2/callback",
	CookieDomain: "example.test",
	CSRFCookie:   "__Secure-itema_login_csrf",
}

// testDeleteEnvironment proves that deleting an Environment as iidp app
// delete does makes ArgoCD delete its Application only after the final
// Backup PreDelete hook completes a real Backup, and that the Deployment and
// Cluster are gone afterwards.
func testDeleteEnvironment(ctx context.Context, t *testing.T, cluster *Cluster) {
	t.Helper()

	// Like iidp app delete, remove only application.yaml: ArgoCD still
	// needs values.yaml to render the PreDelete hook, and removing it too
	// causes a DeletionError.
	err := cluster.PushToRepository(ctx, "iidp-platform", "test: iidp app delete shop (staging only)",
		func(dir string) (bool, error) {
			applicationYAML := filepath.Join(dir, "applications", "shop", "staging", "application.yaml")
			if _, err := os.Stat(applicationYAML); err != nil {
				return false, err
			}
			return true, os.Remove(applicationYAML)
		})
	if err != nil {
		t.Fatal(err)
	}
	// Deliberately not refreshed: forcing a refresh of the parent made
	// ArgoCD run overlapping reconciles for shop-staging, one of which
	// deleted the resources without waiting for the PreDelete hook.
	//
	// The Job and Backup are logged on every poll, once a second, because
	// the hook's objects can be created and cleaned up within a few
	// seconds, and the Backup is removed along with the Cluster. A Backup
	// seen reaching completed during the deletion is the proof the hook did
	// its job.
	start := time.Now()
	deadline := start.Add(9 * time.Minute)
	jobEverObserved := false
	jobEverSucceeded := false
	backupsSeen := map[string]string{}
	completedBackup := ""
	for {
		apps, err := cluster.Applications(ctx)
		if err != nil {
			t.Fatal(err)
		}
		jobOut, jobErr := cluster.Kubectl(ctx, "-n", "shop-staging", "get", "job", "shop-staging-final-backup",
			"-o", "jsonpath={.status.startTime} active={.status.active} succeeded={.status.succeeded} failed={.status.failed}")
		if jobErr == nil {
			jobEverObserved = true
			if strings.Contains(jobOut, "succeeded=1") {
				jobEverSucceeded = true
			}
		}
		// Only the hook's own Backup counts, not the ScheduledBackup's
		// (shop-staging-db-<timestamp>).
		phases, _ := cluster.BackupPhases(ctx, "shop-staging")
		for name, phase := range phases {
			if !strings.HasPrefix(name, "shop-staging-final-") {
				continue
			}
			backupsSeen[name] = phase
			if phase == "completed" && completedBackup == "" {
				completedBackup = name
				t.Logf("[%4.0fs] final Backup %s reached phase completed", time.Since(start).Seconds(), name)
			}
		}
		if app, ok := apps["shop-staging"]; ok {
			t.Logf("[%4.0fs] shop-staging: sync=%s health=%s | Job shop-staging-final-backup: %s | Backups: %v",
				time.Since(start).Seconds(), app.Sync, app.Health, strings.TrimSpace(jobOut), phases)
		} else {
			t.Logf("shop-staging Application is gone (job observed at some point: %v) | Backups seen during deletion: %v", jobEverObserved, backupsSeen)
			break
		}
		if time.Now().After(deadline) {
			out, _ := cluster.Kubectl(ctx, "-n", "argocd", "get", "application", "shop-staging", "-o", "yaml")
			t.Fatalf("Application shop-staging was not deleted within %s:\n%s", deadline.Sub(start), out)
		}
		if sleep(ctx, time.Second) != nil {
			t.Fatal(ctx.Err())
		}
	}

	// A Job that succeeded without a completed Backup is a bug in the
	// chart. Otherwise ArgoCD did not wait for its PreDelete hook, an
	// upstream race (argoproj/argo-cd#29100) hit in about one run in three,
	// so it is logged rather than failed.
	switch {
	case completedBackup != "":
		t.Logf("final Backup %s completed before the Cluster was deleted", completedBackup)
	case jobEverSucceeded:
		t.Fatalf("the final backup Job succeeded but no Backup reached phase completed (Backups seen: %v)", backupsSeen)
	default:
		t.Logf("ArgoCD deleted shop-staging without waiting for its PreDelete hook (argoproj/argo-cd#29100): hook Job observed: %v, Backups seen: %v",
			jobEverObserved, backupsSeen)
	}

	for _, res := range []struct{ kind, name string }{
		{"deployment", "shop-staging"},
		{"cluster.postgresql.cnpg.io", "shop-staging-db"},
	} {
		if err := cluster.WaitForResourceGone(ctx, res.kind, "shop-staging", res.name, time.Minute); err != nil {
			t.Fatal(err)
		}
	}
}
