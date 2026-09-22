package cli_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Itema-as/iidp/internal/cli"
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
	if _, err := os.Stat(filepath.Join(cloneMain(t, url), "applications/shop/prod/application.yaml")); err != nil {
		t.Errorf("applications/shop/prod/application.yaml should still exist: %v", err)
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
	if _, err := os.Stat(filepath.Join(cloneMain(t, url), "applications/shop/prod/application.yaml")); !os.IsNotExist(err) {
		t.Errorf("applications/shop/prod/application.yaml should be gone, stat err = %v", err)
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
	if _, err := os.Stat(filepath.Join(cloneMain(t, url), "applications/shop/prod/application.yaml")); err != nil {
		t.Errorf("applications/shop/prod/application.yaml should still exist after a mismatch: %v", err)
	}
}

func TestAppDeleteForceSkipsConfirmationEvenWithoutMatchingStdin(t *testing.T) {
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML)
	seedApplication(t, url)

	_, stderr, code := deleteApplication(t, url, "shop", "this is never read\n", cli.Dependencies{}, "--force")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr: %s", code, stderr)
	}
	if _, err := os.Stat(filepath.Join(cloneMain(t, url), "applications/shop/prod/application.yaml")); !os.IsNotExist(err) {
		t.Errorf("applications/shop/prod/application.yaml should be gone, stat err = %v", err)
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

// TestAppDeleteIsOneCommitRemovingEveryEnvironment covers both Postgres and
// non-Postgres Applications: since #39, iidp app delete never writes a
// final Backup or Application manifest itself (the chart's ArgoCD
// PreDelete hook takes the final backup instead,
// chart/application/templates/final-backup-job.yaml), so deleting always
// removes every Environment's application.yaml in exactly one commit,
// whether or not Postgres was enabled. values.yaml is deliberately left in
// place (internal/platformrepo/delete.go's own doc comment explains why:
// ArgoCD needs it to render the PreDelete hook at deletion time).
func TestAppDeleteIsOneCommitRemovingEveryEnvironment(t *testing.T) {
	cases := []struct {
		name         string
		extraCreate  []string
		wantPostgres bool
	}{
		{"without Postgres", nil, false},
		{"with Postgres", []string{"--postgres"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			url := newPlatformRepository(t, testCapabilitiesPlatformYAML)
			seedApplication(t, url, append([]string{"--staging"}, tc.extraCreate...)...)

			stdout, stderr, code := deleteApplication(t, url, "shop", "", cli.Dependencies{}, "--force")

			if code != 0 {
				t.Fatalf("exit code = %d, want 0\nstderr: %s", code, stderr)
			}
			clone := cloneMain(t, url)
			subjects := strings.Split(strings.TrimSpace(gitRun(t, clone, "log", "--format=%s")), "\n")
			if subjects[0] != "iidp app delete shop" {
				t.Errorf("newest commit = %q, want the delete commit", subjects[0])
			}
			if strings.Contains(strings.Join(subjects, "|"), "final backup") {
				t.Errorf("history = %v, want no separate final-backup commit: the ArgoCD PreDelete hook takes it, not the CLI", subjects)
			}
			for _, env := range []string{"prod", "staging"} {
				if _, err := os.Stat(filepath.Join(clone, "applications/shop", env, "application.yaml")); !os.IsNotExist(err) {
					t.Errorf("applications/shop/%s/application.yaml should be gone, stat err = %v", env, err)
				}
				// Left in place: ArgoCD needs it to render the Environment's
				// PreDelete hook at deletion time (internal/platformrepo/delete.go).
				if _, err := os.Stat(filepath.Join(clone, "applications/shop", env, "values.yaml")); err != nil {
					t.Errorf("applications/shop/%s/values.yaml should still exist, stat err = %v", env, err)
				}
			}

			if tc.wantPostgres {
				if !strings.Contains(stdout, "30") {
					t.Errorf("stdout = %q, want it to mention the 30-day retention", stdout)
				}
				if !strings.Contains(stdout, "final backup") {
					t.Errorf("stdout = %q, want it to say the Platform takes a final backup", stdout)
				}
				if strings.Contains(stdout, "nothing to back up") {
					t.Errorf("stdout = %q, want no \"nothing to back up\" when Postgres was enabled", stdout)
				}
			} else {
				if !strings.Contains(stdout, "No Environment had Postgres enabled") {
					t.Errorf("stdout = %q, want it to say there was nothing to back up", stdout)
				}
				if strings.Contains(stdout, "PreDelete hook") {
					t.Errorf("stdout = %q, want no mention of the PreDelete hook when Postgres was never enabled", stdout)
				}
			}
		})
	}
}

// TestAppDeleteThenCreateAgainIsBlockedByTheLeftoverValues documents a
// deliberate trade-off (internal/platformrepo/delete.go's own doc comment,
// docs/implementation-notes/39-final-backup-predelete-hook.md): since
// values.yaml is left behind so the final Backup PreDelete hook can still
// render at deletion time, and since checkApplicationAbsent
// (internal/platformrepo/writer.go) refuses iidp app create on any existing
// directory under applications/<name>/ (not only a live application.yaml,
// so a human's own hand-placed files are never silently clobbered, #58),
// recreating an Application right after deleting it is refused until the
// leftover values.yaml is removed by hand.
func TestAppDeleteThenCreateAgainIsBlockedByTheLeftoverValues(t *testing.T) {
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML)
	seedApplication(t, url)

	_, stderr, code := deleteApplication(t, url, "shop", "", cli.Dependencies{}, "--force")
	if code != 0 {
		t.Fatalf("delete: exit code = %d, want 0\nstderr: %s", code, stderr)
	}

	_, stderr, code = createApplication(t, url, cli.Dependencies{}, "--name", "shop", "--kind", "web-service")
	if code == 0 {
		t.Fatalf("re-create: exit code = 0, want non-zero (the leftover values.yaml should still block it)")
	}
	if !strings.Contains(stderr, "shop") || !strings.Contains(stderr, "already") {
		t.Errorf("stderr = %q, want it to say shop already has a directory", stderr)
	}
	if _, err := os.Stat(filepath.Join(cloneMain(t, url), "applications/shop/prod/application.yaml")); !os.IsNotExist(err) {
		t.Errorf("applications/shop/prod/application.yaml should still be gone, stat err = %v", err)
	}
}

// TestAppDeleteTwiceRefusesTheSecondAttemptClearly covers a code-review
// finding: deleting the same Application a second time (whether run twice
// by mistake, or on an Application some other command already deleted)
// must refuse with the same clear ErrApplicationMissing message a
// never-existed Application gets, not a low-level git error from trying to
// `git rm` an empty path list -- appDir still exists after the first
// delete (its own leftover values.yaml, left in place on purpose), but no
// Environment has a live application.yaml anymore.
func TestAppDeleteTwiceRefusesTheSecondAttemptClearly(t *testing.T) {
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML)
	seedApplication(t, url)

	_, stderr, code := deleteApplication(t, url, "shop", "", cli.Dependencies{}, "--force")
	if code != 0 {
		t.Fatalf("first delete: exit code = %d, want 0\nstderr: %s", code, stderr)
	}

	stdout, stderr, code := deleteApplication(t, url, "shop", "", cli.Dependencies{}, "--force")
	if code == 0 {
		t.Fatalf("second delete: exit code = 0, want non-zero\nstdout: %s", stdout)
	}
	if !strings.Contains(stderr, "shop") {
		t.Errorf("stderr = %q, want it to name shop", stderr)
	}
	if strings.Contains(stderr, "exit status") || strings.Contains(stderr, "pathspec") {
		t.Errorf("stderr = %q, want a clear refusal, not a raw git error", stderr)
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
