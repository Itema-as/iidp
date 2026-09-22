//go:build e2e

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
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
// migration run, and an HTTP 200 through Traefik for both hosts.
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

	// kind has no Object Storage: create the Secret the Postgres Capability
	// needs before its Cluster and ObjectStore render, in both of the
	// fixture Application's Environment namespaces, ahead of ArgoCD's own
	// CreateNamespace=true.
	for _, ns := range []string{"shop-prod", "shop-staging"} {
		if err := cluster.CreateNamespace(ctx, ns); err != nil {
			t.Fatal(err)
		}
		if err := cluster.CreateBackupsCredentialsSecret(ctx, ns); err != nil {
			t.Fatal(err)
		}
	}

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
		// Cloud-dependent: configured, applied, but nothing to talk to.
		"platform-tls": Synced,
		"external-dns": Synced,
		"monitoring":   Synced,
	}
	if err := cluster.WaitForApplications(ctx, want, 10*time.Minute); err != nil {
		t.Fatal(err)
	}

	testFixtureApplication(ctx, t, cluster)
}

// testFixtureApplication proves the whole Platform path from the bootstrap
// ticket onward: the fixture Application shop, with a prod and a staging
// Environment and Postgres enabled, deployed through the real bootstrap and
// chart, its migration run, and an HTTP request through Traefik answered
// with 200 for both hosts. staging also has the Itema login Capability on
// (docs/implementation-notes/18-itema-login.md), so its host is asserted to
// redirect an unauthenticated request instead, while prod, unprotected,
// still answers 200.
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
			if err := cluster.CheckRedirect(ctx, env.host, 2*time.Minute); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := cluster.CheckHTTP200(ctx, env.host, 2*time.Minute); err != nil {
			t.Fatal(err)
		}
	}
}
