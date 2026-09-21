package cli_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Itema-as/iidp/internal/cli"
	"github.com/Itema-as/iidp/internal/platform"
)

// deleteApplication runs iidp app delete in-process against the Platform
// repository at url, with stdin standing in for what a developer would
// type at the confirmation prompt.
func deleteApplication(t *testing.T, url, name, stdin string, deps cli.Dependencies, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	if deps.TokenSource == nil {
		deps.TokenSource = fakeTokenSource{token: "gho_test"}
	}
	full := append([]string{"app", "delete", name, "--platform-repo", url}, args...)
	var out, errOut bytes.Buffer
	code = cli.RunWith(full, strings.NewReader(stdin), &out, &errOut, deps)
	return out.String(), errOut.String(), code
}

func TestAppDeleteNonInteractiveWithoutForceRefuses(t *testing.T) {
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML)
	seedApplication(t, url)

	// strings.Reader is never a terminal, so without --interactive or
	// --force this must refuse outright, without even looking at stdin.
	_, stderr, code := deleteApplication(t, url, "shop", "shop\n", cli.Dependencies{})

	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero")
	}
	if !strings.Contains(stderr, "--force") {
		t.Errorf("stderr = %q, want it to name --force", stderr)
	}
	if got := headSubject(t, url); got != "iidp app create shop" {
		t.Errorf("Platform repository head is %q, want unchanged (only the seeding create, no delete commit)", got)
	}
	if _, err := os.Stat(filepath.Join(cloneMain(t, url), "applications/shop")); err != nil {
		t.Errorf("applications/shop should still exist: %v", err)
	}
}

func TestAppDeleteInteractiveConfirmationMatchSucceeds(t *testing.T) {
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML)
	seedApplication(t, url)

	stdout, stderr, code := deleteApplication(t, url, "shop", "shop\n", cli.Dependencies{}, "--interactive")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, "Type the Application name") {
		t.Errorf("stdout = %q, want the confirmation prompt", stdout)
	}
	if _, err := os.Stat(filepath.Join(cloneMain(t, url), "applications/shop")); !os.IsNotExist(err) {
		t.Errorf("applications/shop should be gone, stat err = %v", err)
	}
}

func TestAppDeleteInteractiveConfirmationMismatchRefuses(t *testing.T) {
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML)
	seedApplication(t, url)

	_, stderr, code := deleteApplication(t, url, "shop", "not-shop\n", cli.Dependencies{}, "--interactive")

	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero")
	}
	if !strings.Contains(stderr, "not-shop") || !strings.Contains(stderr, "shop") {
		t.Errorf("stderr = %q, want it to show both the typed and the real name", stderr)
	}
	if _, err := os.Stat(filepath.Join(cloneMain(t, url), "applications/shop")); err != nil {
		t.Errorf("applications/shop should still exist after a mismatch: %v", err)
	}
}

func TestAppDeleteForceSkipsConfirmationEvenWithoutMatchingStdin(t *testing.T) {
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML)
	seedApplication(t, url)

	_, stderr, code := deleteApplication(t, url, "shop", "this is never read\n", cli.Dependencies{}, "--force")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr: %s", code, stderr)
	}
	if _, err := os.Stat(filepath.Join(cloneMain(t, url), "applications/shop")); !os.IsNotExist(err) {
		t.Errorf("applications/shop should be gone, stat err = %v", err)
	}
}

func TestAppDeleteRefusesUnknownApplication(t *testing.T) {
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML)

	_, stderr, code := deleteApplication(t, url, "ghost", "", cli.Dependencies{}, "--force")

	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero")
	}
	if !strings.Contains(stderr, "ghost") {
		t.Errorf("stderr = %q, want it to name the missing Application", stderr)
	}
}

func TestAppDeleteWithoutPostgresIsOneCommitAndNoBackupFiles(t *testing.T) {
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML)
	seedApplication(t, url, "--staging")

	stdout, stderr, code := deleteApplication(t, url, "shop", "", cli.Dependencies{}, "--force")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr: %s", code, stderr)
	}
	clone := cloneMain(t, url)
	subjects := strings.Split(strings.TrimSpace(gitRun(t, clone, "log", "--format=%s")), "\n")
	// Seed, prod, staging create commits, then exactly one delete commit.
	if subjects[0] != "iidp app delete shop" {
		t.Errorf("newest commit = %q, want the delete commit", subjects[0])
	}
	if strings.Contains(strings.Join(subjects, "|"), "final backup") {
		t.Errorf("history = %v, want no final-backup commit: Postgres was never enabled", subjects)
	}
	for _, path := range []string{"applications/shop/prod", "applications/shop/staging"} {
		if _, err := os.Stat(filepath.Join(clone, path)); !os.IsNotExist(err) {
			t.Errorf("%s should be gone, stat err = %v", path, err)
		}
	}
	if _, err := os.Stat(filepath.Join(clone, "applications/shop")); !os.IsNotExist(err) {
		t.Errorf("applications/shop should be entirely gone (no final-backup directory), stat err = %v", err)
	}
	if strings.Contains(stdout, "Final Postgres backups") {
		t.Errorf("stdout = %q, want no mention of a final backup", stdout)
	}
}

func TestAppDeleteWithPostgresCommitsFinalBackupThenRemoves(t *testing.T) {
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML)
	seedApplication(t, url, "--staging", "--postgres")

	stdout, stderr, code := deleteApplication(t, url, "shop", "", cli.Dependencies{}, "--force")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr: %s", code, stderr)
	}
	clone := cloneMain(t, url)
	subjects := strings.Split(strings.TrimSpace(gitRun(t, clone, "log", "--format=%s")), "\n")
	if len(subjects) < 2 || subjects[0] != "iidp app delete shop" || subjects[1] != "iidp app delete shop: final backup" {
		t.Fatalf("history = %v, want [\"iidp app delete shop\", \"iidp app delete shop: final backup\", ...]", subjects)
	}

	// Both commits must have been pushed together: the push only happens
	// once, so if the second commit exists the first was already on the
	// remote too (checked above by history order); nothing more to prove
	// through this seam.

	for _, path := range []string{"applications/shop/prod", "applications/shop/staging"} {
		if _, err := os.Stat(filepath.Join(clone, path)); !os.IsNotExist(err) {
			t.Errorf("%s should be gone, stat err = %v", path, err)
		}
	}

	for _, tc := range []struct {
		env           string
		wantCluster   string
		wantNamespace string
		wantAppName   string
	}{
		{"prod", "shop-db", "shop-prod", "shop-final-backup-prod"},
		{"staging", "shop-staging-db", "shop-staging", "shop-final-backup-staging"},
	} {
		finalDir := filepath.Join(clone, "applications/shop/final-backup-"+tc.env)
		backup := readYAML(t, filepath.Join(finalDir, "backup.yaml"))
		for _, check := range []struct {
			path []any
			want any
		}{
			{[]any{"apiVersion"}, "postgresql.cnpg.io/v1"},
			{[]any{"kind"}, "Backup"},
			{[]any{"spec", "cluster", "name"}, tc.wantCluster},
			{[]any{"spec", "method"}, "plugin"},
			{[]any{"spec", "pluginConfiguration", "name"}, "barman-cloud.cloudnative-pg.io"},
		} {
			if got := lookup(t, backup, check.path...); got != check.want {
				t.Errorf("%s backup.yaml %v = %v, want %v", tc.env, check.path, got, check.want)
			}
		}
		if got, ok := lookup(t, backup, "metadata", "annotations", "iidp.itema.no/retain-until").(string); !ok || len(got) != len("2026-01-02") {
			t.Errorf("%s backup.yaml retain-until annotation = %v, want a YYYY-MM-DD date", tc.env, lookup(t, backup, "metadata", "annotations", "iidp.itema.no/retain-until"))
		}

		app := readYAML(t, filepath.Join(finalDir, "application.yaml"))
		for _, check := range []struct {
			path []any
			want any
		}{
			{[]any{"apiVersion"}, "argoproj.io/v1alpha1"},
			{[]any{"kind"}, "Application"},
			{[]any{"metadata", "name"}, tc.wantAppName},
			{[]any{"spec", "source", "repoURL"}, platform.RepositoryURL},
			{[]any{"spec", "source", "path"}, "applications/shop/final-backup-" + tc.env},
			{[]any{"spec", "source", "directory", "include"}, "backup.yaml"},
			{[]any{"spec", "destination", "namespace"}, tc.wantNamespace},
			{[]any{"spec", "syncPolicy", "automated", "prune"}, false},
		} {
			if got := lookup(t, app, check.path...); got != check.want {
				t.Errorf("%s final-backup application.yaml %v = %v, want %v", tc.env, check.path, got, check.want)
			}
		}
		if _, hasFinalizers := app["metadata"].(map[string]any)["finalizers"]; hasFinalizers {
			t.Errorf("%s final-backup application.yaml has finalizers, want none", tc.env)
		}
	}

	if !strings.Contains(stdout, "shop-db") || !strings.Contains(stdout, "shop-staging-db") {
		t.Errorf("stdout = %q, want it to name both Clusters", stdout)
	}
	if !strings.Contains(stdout, "30") {
		t.Errorf("stdout = %q, want it to mention the 30-day retention", stdout)
	}
}

func TestAppDeleteWithPostgresOnlyOnProdBacksUpOnlyProd(t *testing.T) {
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML)
	seedApplication(t, url, "--postgres")

	_, stderr, code := deleteApplication(t, url, "shop", "", cli.Dependencies{}, "--force")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr: %s", code, stderr)
	}
	clone := cloneMain(t, url)
	if _, err := os.Stat(filepath.Join(clone, "applications/shop/final-backup-prod/backup.yaml")); err != nil {
		t.Errorf("final-backup-prod/backup.yaml missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(clone, "applications/shop/final-backup-staging")); !os.IsNotExist(err) {
		t.Errorf("final-backup-staging should not exist: there was no staging Environment, stat err = %v", err)
	}
}

func TestAppDeleteNeverTouchesApplicationRepository(t *testing.T) {
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML)
	seedApplication(t, url, "--postgres")
	deps := cli.Dependencies{
		// Any GitHub API call the delete path made would try to reach this
		// (deliberately invalid) base URL and fail; a passing run proves
		// delete never talks to GitHub's API at all (the Application
		// repository is only ever reached through it).
		GitHubAPI: "http://127.0.0.1:1/unreachable",
	}

	_, stderr, code := deleteApplication(t, url, "shop", "", deps, "--force")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr: %s", code, stderr)
	}
}
