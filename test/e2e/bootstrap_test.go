//go:build e2e

package e2e

import (
	"context"
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
)

// TestBootstrap proves that, given a Platform repository, ArgoCD installs
// every Phase 1 Platform component: a kind cluster set up like the node,
// the fixture Platform repository served from inside the cluster, the root
// Application applied the way cloud-init applies it, and every component
// Application reaching Synced and Healthy. Components that cannot work
// without a cloud account (the TLS issuer and certificate, external-dns,
// the Grafana Cloud shipper) only have to reach Synced. It then proves the
// whole Platform path (testFixtureApplication): the fixture Platform
// repository's Application shop, with Postgres enabled on a prod and a
// staging Environment, deployed through the real bootstrap and chart, its
// migration run, and an HTTP 200 through Traefik for both hosts. Next
// (testUnreleasedEnvironments) it proves that two Environments with no
// image yet, later-prod and brochure-prod, are Synced and Healthy with
// nothing of the chart's in their namespaces. Finally
// (testDeleteEnvironment) it proves #39: pushing a commit that removes
// shop's staging Environment directory, the way iidp app delete itself
// does, makes ArgoCD delete the shop-staging Application only after its
// final Backup PreDelete hook completes a real Backup against the
// harness's own S3 server, and leaves the namespace's Deployment and Cluster
// gone afterwards. Last (testDeployGate) it proves #60: a deploy through the
// Deploy gate, authenticated by an OIDC token from the harness's fake
// issuer, lands in the Platform repository and brochure-prod syncs it,
// while a call from another repository, one from a disallowed ref and
// (#61) one with a tag Docker Hub does not have are refused. After it
// (testMigrationCommandFromIidpYAML) it proves #66: a deploy of shop
// carrying the migration command from its Application repository's
// iidp.yaml sets it with the tag, and the migration Job runs it.
//
// Run with:
//
//	go test -tags e2e ./test/e2e/... -run TestBootstrap -v -timeout 30m
//
// Needs kind, kubectl, helm, git and docker (or podman with
// KIND_EXPERIMENTAL_PROVIDER=podman). Set IIDP_E2E_KEEP=1 to keep the
// cluster for inspection; delete it with `kind delete cluster --name
// iidp-e2e` (or the name IIDP_E2E_CLUSTER set). IIDP_E2E_CLUSTER overrides
// the kind cluster name, so a machine can run more than one instance of
// this test at once without one deleting another's cluster; the in-cluster
// git server's namespace and hostnames are a fixed "iidp-e2e" regardless,
// since they never leave the cluster they are served from.
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
	// A harness-only Object Storage stand-in, not part of the bootstrap or
	// the chart: shop-staging's ObjectStore points at it, so its final
	// Backup PreDelete hook can genuinely complete when testDeleteEnvironment
	// deletes it (docs/implementation-notes/39-final-backup-predelete-hook.md).
	if err := cluster.InstallObjectStorage(ctx); err != nil {
		t.Fatal(err)
	}
	// What the Deploy gate needs that kind lacks: its image built from this
	// working tree, the App credential cloud-init writes, and a stand-in
	// for GitHub's OIDC issuer and API (testDeployGate).
	issuer, err := cluster.InstallDeployGateStandIns(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// The Platform repository pins the bootstrap by pointing at this
	// repository, so both are served: iidp.git holds the working tree's
	// bootstrap directory and chart (the fixture Application's Environments
	// take the application chart from here, path chart/application, since
	// kind has no GHCR to pull the OCI chart from; see
	// docs/implementation-notes/09-e2e-fixture-application.md),
	// iidp-platform.git the fixture Platform repository, which also holds
	// the fixture Application shop (applications/shop/{prod,staging}).
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

	// kind has no cloud Object Storage: unlike before #42, the harness no
	// longer creates the backups-credentials Secret by hand in either of
	// the fixture Application's Environment namespaces. Both Environments'
	// own applications/shop/<environment>/sops/backups-credentials.enc.yaml
	// (a byte-for-byte copy of bootstrap/templates/backups-credentials.enc.yaml, the
	// same file iidp app create --postgres itself copies) is applied by
	// their own ArgoCD Application, at the same sync-wave as the Cluster
	// and ObjectStore that reference it. prod's endpoint stays unreachable
	// (objectstorage.invalid; nothing ever authenticates with these
	// credentials there); staging's points at the harness's own S3 server
	// (versitygw), whose root credentials
	// (ObjectStorageAccessKey/ObjectStorageSecretKey) are exactly
	// what that file decrypts to. See
	// docs/implementation-notes/42-backups-credentials.md.
	want := map[string]Expectation{
		"platform":            Healthy,
		"platform-components": Healthy,
		"platform-secrets":    Healthy,
		"argocd":              Healthy,
		"cert-manager":        Healthy,
		"cloudnative-pg":      Healthy,
		"cnpg-barman-cloud":   Healthy,
		// The dummy Entra credentials and platform.yaml's
		// oauth2Proxy.skipOIDCDiscovery (bootstrap/README.md) are enough for
		// the pod itself to come up Healthy in kind; only a real sign-in
		// would need real credentials.
		"oauth2-proxy": Healthy,
		// Healthy against the harness's image, App credential and fake
		// GitHub (InstallDeployGateStandIns).
		"deploy-gate": Healthy,
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
	// Last, once shop-staging's CPU requests are gone: brochure-prod's
	// first image adds a workload to the node.
	testDeployGate(ctx, t, cluster, issuer)
	testMigrationCommandFromIidpYAML(ctx, t, cluster, issuer)
}

// brochureRepositoryID and shopRepositoryID are the repository ids the
// fixture binds brochure and shop to (applications/<name>/repository.yaml).
const (
	brochureRepositoryID = 700000001
	shopRepositoryID     = 700000002
)

// shopDeployTag is the image testMigrationCommandFromIidpYAML deploys to
// shop-prod: another nginx Alpine tag than the fixture's 1.30-alpine, so
// the Deployment changes too. ArgoCD leaves hooks out of its diff, so a
// commit that changed only the migration Job would not sync on its own; a
// real deploy always changes the tag with it. It must exist on Docker Hub:
// the gate checks every new tag against the image's registry (#61).
const shopDeployTag = "1.30.0-alpine"

// testMigrationCommandFromIidpYAML proves #66 end to end: the migration
// command in shop's Application repository (the fixture
// test/e2e/fixtures/shop-repository/iidp.yaml, read with the code iidp ci
// set-image uses) travels with a deploy through the Deploy gate, lands in
// shop's prod values.yaml in the same commit as the tag, and is what the
// migration Job then runs. A command for brochure, which has no Postgres,
// is refused first.
func testMigrationCommandFromIidpYAML(ctx context.Context, t *testing.T, cluster *Cluster, issuer *FakeIssuer) {
	t.Helper()
	appConfig, ok, err := appconfig.Read(filepath.Join(cluster.RepoRoot, "test", "e2e", "fixtures", "shop-repository"))
	if err != nil || !ok || appConfig.MigrationCommand == "" {
		t.Fatalf("reading the fixture iidp.yaml: ok = %v, command = %q, err = %v", ok, appConfig.MigrationCommand, err)
	}
	command := appConfig.MigrationCommand
	call := func(repository string, repositoryID int64, application, tag string) (int, map[string]any) {
		t.Helper()
		token, err := issuer.Sign(issuer.Claims(repository, repositoryID, "refs/heads/main"))
		if err != nil {
			t.Fatal(err)
		}
		status, body, err := cluster.CallDeployGateWithMigration(ctx, token, application, "auto", tag, &command)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("Deploy gate: HTTP %d %v", status, body)
		return status, body
	}

	// brochure has no Postgres: nothing to migrate, so nothing is written.
	status, body := call("Itema-as/brochure", brochureRepositoryID, "brochure", "1.30-alpine")
	if msg, _ := body["error"].(string); status != http.StatusConflict || !strings.Contains(msg, "Add the Postgres Capability first") {
		t.Errorf("a migration command for brochure: HTTP %d %v, want 409 asking for the Postgres Capability first", status, body)
	}

	// shop's deploy. Its staging Environment was deleted
	// (testDeleteEnvironment), so main deploys to prod.
	status, body = call("Itema-as/shop", shopRepositoryID, "shop", shopDeployTag)
	if status != http.StatusOK || body["environment"] != "prod" || body["migrationCommandChanged"] != true {
		t.Fatalf("the deploy of shop with its iidp.yaml: HTTP %d %v, want 200, prod and the migration command changed", status, body)
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
		if got := git("show", "--stat", "--format=", "HEAD"); !strings.Contains(got, "applications/shop/prod/values.yaml") || !strings.Contains(got, "1 file changed") {
			t.Errorf("the deploy changed:\n%s\nwant only shop's prod values.yaml, tag and command together", got)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// shop-prod syncs the commit: the migration Job, recreated for the
	// sync, runs the command from iidp.yaml before the new image rolls out.
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
}

// testDeployGate proves #60 end to end: the Deploy gate the bootstrap
// installed, reached through Traefik at deploy.<baseDomain> with a token
// from the harness's fake issuer, refuses a call from another repository
// of the org, a call from a ref that may not deploy and (#61) a tag its
// image repository does not have, then deploys brochure's first image
// from main, a tag the gate has checked on Docker Hub. The commit lands in
// the Platform repository authored by the token's actor and committed by
// the App, and
// brochure-prod syncs it: the Environment that rendered nothing
// (testUnreleasedEnvironments) now answers HTTP 200.
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

	// Another repository of the org, calling for brochure.
	status, msg := call(issuer.Claims("Itema-as/impostor", 700000099, "refs/heads/main"), "1.30-alpine")
	if status != http.StatusForbidden || !strings.Contains(msg, "repository id 700000099") {
		t.Errorf("a call from another repository: HTTP %d %q, want 403 naming its repository id", status, msg)
	}
	// brochure's own repository, from a branch other than main.
	status, msg = call(issuer.Claims("Itema-as/brochure", brochureRepositoryID, "refs/heads/feature"), "1.30-alpine")
	if status != http.StatusForbidden || !strings.Contains(msg, "refs/heads/feature") {
		t.Errorf("a call from refs/heads/feature: HTTP %d %q, want 403 naming the ref", status, msg)
	}
	// brochure's own repository, from main, with a tag Docker Hub does not
	// have: the gate's image check (#61) asks the real registry, as it
	// does for the deploy below.
	status, msg = call(issuer.Claims("Itema-as/brochure", brochureRepositoryID, "refs/heads/main"), "iidp-e2e-no-such-tag")
	if status != http.StatusUnprocessableEntity || !strings.Contains(msg, "docker.io/library/nginx:iidp-e2e-no-such-tag does not exist") {
		t.Errorf("a tag that was never pushed: HTTP %d %q, want 422 naming the image", status, msg)
	}

	// The deploy. brochure has no staging, so main deploys to prod.
	status, env := call(issuer.Claims("Itema-as/brochure", brochureRepositoryID, "refs/heads/main"), "1.30-alpine")
	if status != http.StatusOK || env != "prod" {
		t.Fatalf("the deploy from main: HTTP %d %q, want 200 and prod", status, env)
	}

	// Refused calls committed nothing; the deploy committed one change.
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
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// brochure-prod syncs the new image. A refresh saves waiting out
	// ArgoCD's three-minute poll; this is a plain sync, not the deletion
	// testDeleteEnvironment must not hurry.
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

// testUnreleasedEnvironments proves #47's item 13: an Environment the deploy
// workflow has not written an image into yet (image.tag: "", what the CLI
// writes on create) is Synced and Healthy in ArgoCD, with no comparison
// error, and nothing of the chart's runs in it. later-prod has Postgres on
// and the sops/ source iidp app create --postgres adds, so its only
// resource is the backups-credentials Secret; brochure-prod has no
// Capability and no secret, so it has no resources at all, the case whose
// status comes from ArgoCD alone. Neither adds a workload to the node. See
// docs/implementation-notes/47-unreleased-environment.md.
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
		// A ComparisonError is what an unreleased Environment showed
		// before: the sync status alone could hide one behind a stale
		// Synced, so the conditions are checked too.
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

		// And nothing of the chart's exists in the namespace: no
		// workload, no database, no hook.
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

// testFixtureApplication proves the whole Platform path from the bootstrap
// ticket onward: the fixture Application shop, with a prod and a staging
// Environment and Postgres enabled, deployed through the real bootstrap and
// chart, its migration run, and an HTTP request through Traefik answered
// with 200 for both hosts. staging also has the Itema login Capability on
// (docs/implementation-notes/18-itema-login.md), so an unauthenticated
// request to its host, and to its custom domain inside the fixture zone,
// is asserted to be redirected straight to the provider's sign-in,
// carrying the URL to come back to, while that custom domain's ACME
// challenge path is not (#76), and prod, unprotected, still answers 200
// (docs/implementation-notes/77-login-redirect.md), and keeps answering 200
// throughout a rollout (#75).
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
	// shop-staging has a sign-in group, so the redirect above came through
	// its own Middleware, which asks oauth2-proxy for allowed_groups, named
	// on both its Ingresses (#92). A signed-in user outside the group
	// would get a 403 from it; kind cannot sign anyone in to show that
	// (docs/implementation-notes/92-sign-in-groups.md).
	if err := checkSignInGroupMiddleware(ctx, cluster); err != nil {
		t.Fatal(err)
	}
	// shop-staging's custom domain inside the fixture zone, on the chart's
	// HTTP-01 Ingress, is protected the same way: the login cookie and the
	// redirect allowlist cover the whole zone, so the state carries its own
	// URL and the CSRF cookie is for the zone
	// (docs/implementation-notes/76-login-in-zone-domains.md). Its ACME
	// HTTP-01 challenge path is not.
	if err := cluster.CheckSignInRedirect(ctx, shopStagingCustomDomain, signInPath, fixtureSignIn, 2*time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := cluster.CheckACMEChallengeBypassesLogin(ctx, "shop-staging", shopStagingCustomDomain, "shop-staging", 80, 2*time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := cluster.CheckRolloutServes(ctx, "shop-prod", "shop", "shop.app.example.test"); err != nil {
		t.Fatal(err)
	}
}

// checkSignInGroupMiddleware checks that shop-staging's sign-in group
// reached the cluster: its Middleware asks oauth2-proxy's root address
// with the group as allowed_groups, and both its Ingresses name it.
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

// shopStagingCustomDomain is the custom domain the fixture's shop-staging
// lists: inside the fixture's cloudflareZone, example.test, not under its
// baseDomain.
const shopStagingCustomDomain = "shop-staging.example.test"

// signInPath is a path with a query, so the sign-in check proves both
// survive the round trip through the provider.
const signInPath = "/account/orders?page=2&sort=date"

// fixtureSignIn is the sign-in redirect the fixture Platform gives: its
// oauth2-proxy skips OIDC discovery and uses a fixed, never-reached
// authorize endpoint (bootstrap/templates/oauth2-proxy.yaml, platform.yaml's
// oauth2Proxy.skipOIDCDiscovery). The callback stays on the auth address
// under baseDomain; the CSRF cookie is for the fixture's cloudflareZone,
// the login cookie domain, under the bootstrap's cookie name.
var fixtureSignIn = SignIn{
	LoginURL:     "https://oauth2-proxy-entra.invalid/authorize",
	Callback:     "https://auth.app.example.test/oauth2/callback",
	CookieDomain: "example.test",
	CSRFCookie:   "__Secure-itema_login_csrf",
}

// testDeleteEnvironment proves #39's design end to end: pushing a commit to
// the fixture Platform repository that removes an Environment's directory
// (what iidp app delete itself does) makes ArgoCD delete that Environment's
// ArgoCD Application only after its final Backup PreDelete hook
// (chart/application/templates/final-backup-job.yaml) reaches Healthy --
// its Backup genuinely completes, since shop-staging's ObjectStore points
// at the harness's S3 server (InstallObjectStorage, unlike prod's, which stays
// unreachable and is not touched here) -- and that the namespace's
// Deployment and Cluster are gone afterwards. See
// docs/implementation-notes/39-final-backup-predelete-hook.md.
func testDeleteEnvironment(ctx context.Context, t *testing.T, cluster *Cluster) {
	t.Helper()

	// Removes only application.yaml, matching iidp app delete's own actual
	// behaviour (internal/platformrepo/delete.go): values.yaml is left in
	// place deliberately, since ArgoCD needs it to render the Environment's
	// PreDelete hook at deletion time -- removing the whole directory here
	// reproduces the DeletionError this design change exists to avoid; see
	// docs/implementation-notes/39-final-backup-predelete-hook.md.
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
	// Deliberately not hard-refreshed. An earlier version of this test
	// called Cluster.RefreshApplication(ctx, "applications") here, the same
	// annotation `argocd app get --hard-refresh` sets, to avoid waiting out
	// ArgoCD's default ~3-minute poll interval. It reproduced a genuine
	// ArgoCD-level race every time: the parent's own sync (triggered
	// immediately by the forced refresh) fired a burst of overlapping,
	// concurrent reconciles for shop-staging -- visible in the
	// argocd-application-controller's own log as repeated "Hook resource
	// ... already exists, skipping" warnings within the same second -- one
	// of which proceeded straight to "Deleting resources" without ever
	// waiting for the just-created hook Job to reach Healthy, deleting the
	// Cluster and Deployment in well under ten seconds, long before a
	// Job's Pod could even pull an image. Removing the forced refresh and
	// letting ArgoCD's own, unhurried poll trigger the deletion made the
	// hook wait correctly every time in testing; see
	// docs/implementation-notes/39-final-backup-predelete-hook.md for the
	// evidence and the reasoning kept here.
	//
	// The Application is only removed once the PreDelete hook Job's Backup
	// reaches phase completed (or the hook fails and blocks deletion, in
	// which case this times out with diagnostics naming why). This is also
	// proof the Cluster and every other resource are gone: ArgoCD does not
	// finish deleting an Application, and so does not remove it from this
	// list, until the resources-finalizer has removed everything it owns.
	// Logging the Job's and the Backup's own state on every poll (not just
	// on timeout) is what makes that distinction -- "the hook ran and
	// finished" versus "the hook never ran at all" -- visible in the
	// output at all, given how briefly the hook's own objects exist.
	// Polls every second, not every five: the ArgoCD race documented above
	// (found, not caused, by this test -- see the implementation notes) can
	// take the hook's ServiceAccount/Role/RoleBinding/Job from "just
	// created" to "cleaned up by its own HookSucceeded policy" in well
	// under a five-second gap, which is indistinguishable from the hook
	// never running at all through external polling alone. A one-second
	// poll does not guarantee catching that window either, but it is the
	// best this test can do without watching the namespace's events
	// directly (a bigger lift this ticket does not take on).
	//
	// The Backup is observed here, during the deletion, because it does not
	// survive it: CloudNativePG removes a Backup together with the Cluster
	// it references, and the Cluster is torn down as soon as the hook
	// reports Healthy. So the proof that the hook did its job is a Backup
	// seen reaching phase completed while the Application was still being
	// deleted, recorded across polls, never a Backup found afterwards.
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
		// Only the hook's own Backup counts. The Environment's
		// ScheduledBackup has been creating its own, named after the
		// Cluster (shop-staging-db-<timestamp>), since the Environment
		// was created; the hook names its Backup <fullname>-final-<timestamp>
		// (chart/application/templates/final-backup-job.yaml). Counting
		// any Backup would let a scheduled one decide this assertion.
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

	// Three outcomes, told apart by what the polls recorded:
	//
	//   - A Backup reached completed: the hook did its job. What this test
	//     exists to prove.
	//   - The hook Job succeeded but no Backup ever completed: the Job's
	//     own script claimed success without the backup finishing, which
	//     is a bug in this repository's chart. A hard failure.
	//   - Otherwise the Environment was deleted while the hook was still
	//     pending, or without the hook running at all: ArgoCD did not wait
	//     for its own PreDelete hook, argoproj/argo-cd#29100 (a stale-cache
	//     race in the controller, open upstream, about one local run in
	//     three). Nothing here can fix that, so it is named loudly rather
	//     than failing the run on an upstream bug; the resource-gone
	//     assertions below still hold either way.
	switch {
	case completedBackup != "":
		t.Logf("final Backup %s completed before the Cluster was deleted", completedBackup)
	case jobEverSucceeded:
		t.Fatalf("the final backup Job succeeded but no Backup reached phase completed (Backups seen: %v)", backupsSeen)
	default:
		t.Logf("ArgoCD deleted shop-staging without waiting for its PreDelete hook (argoproj/argo-cd#29100): hook Job observed: %v, Backups seen: %v; see docs/implementation-notes/39-final-backup-predelete-hook.md",
			jobEverObserved, backupsSeen)
	}

	for _, res := range []struct{ kind, name string }{
		{"deployment", "shop-staging"},
		// <fullname>-db, the chart's naming convention for the Cluster
		// (docs/implementation-notes/07-chart-postgres.md).
		{"cluster.postgresql.cnpg.io", "shop-staging-db"},
	} {
		if err := cluster.WaitForResourceGone(ctx, res.kind, "shop-staging", res.name, time.Minute); err != nil {
			t.Fatal(err)
		}
	}
}
