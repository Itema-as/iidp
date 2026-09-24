package cli_test

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Itema-as/iidp/internal/cli"
	"github.com/Itema-as/iidp/internal/platform"
)

// bindApplication runs iidp app bind in-process against the Platform
// repository at url and the fake GitHub API gh.
func bindApplication(t *testing.T, url string, gh *fakeGitHub, name string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	deps := cli.Dependencies{TokenSource: fakeTokenSource{token: "gho_test"}, GitHubAPI: gh.srv.URL}
	full := append([]string{"app", "bind", name, "--platform-repo", url}, args...)
	var out, errOut bytes.Buffer
	code = cli.RunWith(full, strings.NewReader(""), &out, &errOut, deps)
	return out.String(), errOut.String(), code
}

// createBound makes shop through the Create path, so it is bound to
// platform.Org/shop from the start.
func createBound(t *testing.T, url string, gh *fakeGitHub) {
	t.Helper()
	_, stderr, code := createApplication(t, url, cli.Dependencies{GitHubAPI: gh.srv.URL},
		"--name", "shop", "--path", "create", "--framework", "nextjs")
	if code != 0 {
		t.Fatalf("creating shop: exit code = %d, want 0\nstderr: %s", code, stderr)
	}
}

func TestAppCreateWithoutPathWritesNoBindingAndSaysHowToBind(t *testing.T) {
	url := newPlatformRepository(t, testPlatformYAML)

	stdout, stderr, code := createApplication(t, url, cli.Dependencies{}, "--name", "shop", "--kind", "web-service")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr: %s", code, stderr)
	}
	if _, err := os.Stat(filepath.Join(cloneMain(t, url), "applications/shop/repository.yaml")); err == nil {
		t.Errorf("applications/shop/repository.yaml exists, want none: there is no Application repository to bind")
	}
	if want := "iidp app bind shop --repo " + platform.Org + "/<repository>"; !strings.Contains(stdout, want) {
		t.Errorf("stdout lacks %q:\n%s", want, stdout)
	}
}

func TestAppBindBackfillsAnApplicationWithoutIDs(t *testing.T) {
	// The live Platform's hello: made before Create and Adopt recorded ids.
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML)
	seedApplication(t, url, "--staging")
	gh := newFakeGitHub(t)
	gh.markExisting(platform.Org, "shop")

	stdout, stderr, code := bindApplication(t, url, gh, "shop", "--repo", platform.Org+"/shop")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	clone := cloneMain(t, url)
	assertBinding(t, clone, "shop", platform.Org+"/shop", gh.repoID(platform.Org, "shop"), fakeOrgID)
	if got := headSubject(t, url); got != "iidp app bind shop "+platform.Org+"/shop" {
		t.Errorf("head commit = %q", got)
	}
	touched := strings.Fields(gitRun(t, clone, "show", "--name-only", "--format=", "HEAD"))
	if strings.Join(touched, " ") != "applications/shop/repository.yaml" {
		t.Errorf("the bind commit touched %v, want only applications/shop/repository.yaml", touched)
	}
	want := fmt.Sprintf("Bound Application shop to %s/shop (repository id %d, owner id %d)", platform.Org, gh.repoID(platform.Org, "shop"), fakeOrgID)
	if !strings.Contains(stdout, want) {
		t.Errorf("stdout lacks %q:\n%s", want, stdout)
	}
}

func TestAppBindAcceptsARepositoryURL(t *testing.T) {
	url := newPlatformRepository(t, testPlatformYAML)
	seedApplication(t, url)
	gh := newFakeGitHub(t)
	gh.markExisting(platform.Org, "shop-web")

	_, stderr, code := bindApplication(t, url, gh, "shop", "--repo", "https://github.com/"+platform.Org+"/shop-web.git")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr: %s", code, stderr)
	}
	assertBinding(t, cloneMain(t, url), "shop", platform.Org+"/shop-web", gh.repoID(platform.Org, "shop-web"), fakeOrgID)
}

func TestAppBindToTheSameRepositoryChangesNothing(t *testing.T) {
	url := newPlatformRepository(t, testPlatformYAML)
	gh := newFakeGitHub(t)
	createBound(t, url, gh)

	stdout, stderr, code := bindApplication(t, url, gh, "shop", "--repo", platform.Org+"/shop")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, "already bound") {
		t.Errorf("stdout = %q, want it to say shop is already bound", stdout)
	}
	if got := headSubject(t, url); got != "iidp app create shop" {
		t.Errorf("head commit = %q, want no new commit", got)
	}
}

func TestAppBindAfterARenameOnlyRefreshesTheName(t *testing.T) {
	url := newPlatformRepository(t, testPlatformYAML)
	gh := newFakeGitHub(t)
	createBound(t, url, gh)
	gh.rename(platform.Org, "shop", "storefront")

	_, stderr, code := bindApplication(t, url, gh, "shop", "--repo", platform.Org+"/storefront")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0: the same repository by id needs no --rebind\nstderr: %s", code, stderr)
	}
	assertBinding(t, cloneMain(t, url), "shop", platform.Org+"/storefront", gh.repoID(platform.Org, "shop"), fakeOrgID)
}

func TestAppBindToAnotherRepositoryNeedsRebind(t *testing.T) {
	// The repository was deleted and recreated under the same name: GitHub
	// gives it a new id, so it is a different repository and cannot take
	// over the Application without --rebind.
	url := newPlatformRepository(t, testPlatformYAML)
	gh := newFakeGitHub(t)
	createBound(t, url, gh)
	oldID := gh.repoID(platform.Org, "shop")
	gh.recreate(platform.Org, "shop")

	stdout, stderr, code := bindApplication(t, url, gh, "shop", "--repo", platform.Org+"/shop")

	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero\nstdout: %s", stdout)
	}
	for _, want := range []string{"--rebind", fmt.Sprint(oldID)} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr)
		}
	}
	if got := headSubject(t, url); got != "iidp app create shop" {
		t.Errorf("head commit = %q, want no new commit", got)
	}
	assertBinding(t, cloneMain(t, url), "shop", platform.Org+"/shop", oldID, fakeOrgID)

	stdout, stderr, code = bindApplication(t, url, gh, "shop", "--repo", platform.Org+"/shop", "--rebind")

	if code != 0 {
		t.Fatalf("--rebind: exit code = %d, want 0\nstderr: %s", code, stderr)
	}
	newID := gh.repoID(platform.Org, "shop")
	if newID == oldID {
		t.Fatalf("the fake did not give the recreated repository a new id")
	}
	assertBinding(t, cloneMain(t, url), "shop", platform.Org+"/shop", newID, fakeOrgID)
	if !strings.Contains(stdout, "can no longer deploy it") {
		t.Errorf("stdout does not say the previous repository lost the Application:\n%s", stdout)
	}
}

func TestAppBindReplacesAHandEditedBindingThatBindsNothing(t *testing.T) {
	for name, content := range map[string]string{
		"not YAML":   "repositoryId: [unclosed\n",
		"no ids":     "repository: " + platform.Org + "/shop\n",
		"one id":     "repositoryId: 12\n",
		"empty file": "",
	} {
		t.Run(name, func(t *testing.T) {
			url := newPlatformRepository(t, testPlatformYAML)
			seedApplication(t, url)
			pushCommit(t, url, "Hand edit", map[string]string{"applications/shop/repository.yaml": content})
			gh := newFakeGitHub(t)
			gh.markExisting(platform.Org, "shop")

			_, stderr, code := bindApplication(t, url, gh, "shop", "--repo", platform.Org+"/shop")

			if code != 0 {
				t.Fatalf("exit code = %d, want 0\nstderr: %s", code, stderr)
			}
			assertBinding(t, cloneMain(t, url), "shop", platform.Org+"/shop", gh.repoID(platform.Org, "shop"), fakeOrgID)
		})
	}
}

func TestAppBindRefusesARepositoryOutsideTheOrgBeforeAnything(t *testing.T) {
	url := newPlatformRepository(t, testPlatformYAML)
	seedApplication(t, url)
	gh := newFakeGitHub(t)
	gh.markExisting("developer42", "shop")

	_, stderr, code := bindApplication(t, url, gh, "shop", "--repo", "developer42/shop")

	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero")
	}
	if !strings.Contains(stderr, "Transfer it to "+platform.Org) {
		t.Errorf("stderr = %q, want it to name the transfer", stderr)
	}
	if len(gh.requests) != 0 {
		t.Errorf("the fake GitHub API was called %d times, want 0", len(gh.requests))
	}
	if got := headSubject(t, url); got != "iidp app create shop" {
		t.Errorf("head commit = %q, want no new commit", got)
	}
}

func TestAppBindRefusesARepositoryGitHubReportsOutsideTheOrg(t *testing.T) {
	url := newPlatformRepository(t, testPlatformYAML)
	seedApplication(t, url)
	gh := newFakeGitHub(t)
	gh.markExisting(platform.Org, "shop")
	gh.transferAway(platform.Org, "shop", "developer42")

	_, stderr, code := bindApplication(t, url, gh, "shop", "--repo", platform.Org+"/shop")

	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero")
	}
	if !strings.Contains(stderr, "developer42/shop") {
		t.Errorf("stderr = %q, want it to name the owner GitHub reports", stderr)
	}
	if got := headSubject(t, url); got != "iidp app create shop" {
		t.Errorf("head commit = %q, want no new commit", got)
	}
}

func TestAppBindRefusesAnApplicationWithoutALiveEnvironment(t *testing.T) {
	url := newPlatformRepository(t, testPlatformYAML)
	gh := newFakeGitHub(t)
	gh.markExisting(platform.Org, "shop")

	_, stderr, code := bindApplication(t, url, gh, "shop", "--repo", platform.Org+"/shop")

	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero")
	}
	if !strings.Contains(stderr, "shop") || !strings.Contains(stderr, "does not exist") {
		t.Errorf("stderr = %q, want it to say shop does not exist", stderr)
	}
	assertNoApplications(t, url)
}

func TestAppBindRefusesARepositoryThatDoesNotExist(t *testing.T) {
	url := newPlatformRepository(t, testPlatformYAML)
	seedApplication(t, url)
	gh := newFakeGitHub(t)

	_, stderr, code := bindApplication(t, url, gh, "shop", "--repo", platform.Org+"/shop")

	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero")
	}
	if !strings.Contains(stderr, "does not exist") {
		t.Errorf("stderr = %q, want it to say the repository does not exist", stderr)
	}
	if got := headSubject(t, url); got != "iidp app create shop" {
		t.Errorf("head commit = %q, want no new commit", got)
	}
}

func TestAppBindRequiresRepo(t *testing.T) {
	url := newPlatformRepository(t, testPlatformYAML)
	gh := newFakeGitHub(t)

	_, stderr, code := bindApplication(t, url, gh, "shop")

	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero")
	}
	if !strings.Contains(stderr, `"repo"`) {
		t.Errorf("stderr = %q, want it to name --repo", stderr)
	}
}

func TestAppDeleteRemovesTheRepositoryBinding(t *testing.T) {
	// A deleted Application must not stay deployable through the Deploy
	// gate by its old repository; values.yaml stays for the PreDelete hook.
	url := newPlatformRepository(t, testPlatformYAML)
	gh := newFakeGitHub(t)
	createBound(t, url, gh)

	_, stderr, code := deleteApplication(t, url, "shop", "", cli.Dependencies{}, "--force")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr: %s", code, stderr)
	}
	clone := cloneMain(t, url)
	if _, err := os.Stat(filepath.Join(clone, "applications/shop/repository.yaml")); err == nil {
		t.Errorf("applications/shop/repository.yaml still exists after iidp app delete")
	}
	assertFileExists(t, filepath.Join(clone, "applications/shop/prod/values.yaml"))
	if got := headSubject(t, url); got != "iidp app delete shop" {
		t.Errorf("head commit = %q, want one delete commit", got)
	}
}

func TestCommandsWorkWithAHandEditedBinding(t *testing.T) {
	// Nothing but iidp app bind reads the binding, so a hand edit that
	// broke it (or an Application with none) never stops the other
	// commands.
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML)
	seedApplication(t, url)
	pushCommit(t, url, "Hand edit", map[string]string{"applications/shop/repository.yaml": "{not yaml\n"})

	if _, stderr, code := addCapability(t, url, "shop", cli.Dependencies{}, "--size", "medium", "--staging"); code != 0 {
		t.Fatalf("add-capability: exit code = %d, want 0\nstderr: %s", code, stderr)
	}
	if _, stderr, code := createApplication(t, url, cli.Dependencies{}, "--name", "other", "--kind", "web-service"); code != 0 {
		t.Fatalf("create another Application: exit code = %d, want 0\nstderr: %s", code, stderr)
	}
	if _, stderr, code := deleteApplication(t, url, "shop", "", cli.Dependencies{}, "--force"); code != 0 {
		t.Fatalf("delete: exit code = %d, want 0\nstderr: %s", code, stderr)
	}
}
