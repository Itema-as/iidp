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
// the Grafana Cloud shipper) only have to reach Synced.
//
// Run with:
//
//	go test -tags e2e ./test/e2e/... -run TestBootstrap -v -timeout 30m
//
// Needs kind, kubectl, helm, git and docker (or podman with
// KIND_EXPERIMENTAL_PROVIDER=podman). Set IIDP_E2E_KEEP=1 to keep the
// cluster for inspection; delete it with `kind delete cluster --name
// iidp-e2e`.
func TestBootstrap(t *testing.T) {
	ctx := context.Background()
	cluster, err := NewCluster("iidp-e2e", t.Logf)
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
	// bootstrap directory, iidp-platform.git the fixture.
	err = cluster.ServeGitRepositories(ctx,
		Repository{Name: "iidp", Files: map[string]string{"bootstrap": filepath.Join(cluster.RepoRoot, "bootstrap")}},
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
		// Cloud-dependent: configured, applied, but nothing to talk to.
		"platform-tls": Synced,
		"external-dns": Synced,
		"monitoring":   Synced,
	}
	if err := cluster.WaitForApplications(ctx, want, 10*time.Minute); err != nil {
		t.Fatal(err)
	}
}
