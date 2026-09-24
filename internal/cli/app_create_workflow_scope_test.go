package cli_test

import (
	"net/http"
	"os/exec"
	"strings"
	"testing"

	"github.com/Itema-as/iidp/internal/apprepo"
	"github.com/Itema-as/iidp/internal/cli"
	"github.com/Itema-as/iidp/internal/platform"
)

// The workflow-scope check (docs/implementation-notes/47-workflow-scope.md):
// Create and Adopt push .github/workflows/deploy.yaml with the developer's
// gh token, which GitHub refuses without the workflow scope, so both refuse
// up front instead of failing after the Application repository exists.

// scopesWithoutWorkflow is gh auth login's minimum set of scopes.
const scopesWithoutWorkflow = "gist, read:org, repo"

const refreshCommand = "gh auth refresh -s workflow"

// assertBranchAbsent fails when branch exists in the bare repository at
// bareDir: nothing was pushed.
func assertBranchAbsent(t *testing.T, bareDir, branch string) {
	t.Helper()
	cmd := exec.Command("git", "--git-dir", bareDir, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	if err := cmd.Run(); err == nil {
		t.Errorf("branch %s was pushed, want nothing pushed", branch)
	}
}

func TestAppCreatePathRefusesALoginWithoutTheWorkflowScope(t *testing.T) {
	for _, tc := range []struct {
		name   string
		scopes string
	}{
		{"minimum gh scopes", scopesWithoutWorkflow},
		{"a classic token with no scopes at all", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			platformURL := newPlatformRepository(t, testPlatformYAML)
			gh := newFakeGitHub(t)
			gh.setScopes(tc.scopes)

			stdout, stderr, code := createApplication(t, platformURL, cli.Dependencies{GitHubAPI: gh.srv.URL},
				"--name", "shop", "--path", "create", "--framework", "nextjs")

			if code == 0 {
				t.Fatalf("exit code = 0, want non-zero\nstdout: %s", stdout)
			}
			for _, want := range []string{refreshCommand, "workflow scope", ".github/workflows/deploy.yaml"} {
				if !strings.Contains(stderr, want) {
					t.Errorf("stderr lacks %q:\n%s", want, stderr)
				}
			}
			if n := gh.requestsTo(http.MethodPost, "/orgs/"+platform.Org+"/repos"); n != 0 {
				t.Errorf("POST /orgs/%s/repos called %d times, want 0: the repository must not be created", platform.Org, n)
			}
			if n := gh.requestsTo(http.MethodGet, "/repos/"+platform.Org+"/shop"); n != 0 {
				t.Errorf("GET /repos/%s/shop called %d times, want 0: the refusal comes before every other check", platform.Org, n)
			}
			if strings.Contains(stdout, "Creating Application") {
				t.Errorf("stdout announced the creation before refusing:\n%s", stdout)
			}
			assertNoApplications(t, platformURL)
		})
	}
}

func TestAppCreatePathProceedsWhenGitHubReportsNoScopes(t *testing.T) {
	// A fine-grained personal access token or a GitHub App token: GitHub
	// sends no X-OAuth-Scopes header, so whether it may push workflows
	// cannot be known up front, and the check lets it through.
	platformURL := newPlatformRepository(t, testPlatformYAML)
	gh := newFakeGitHub(t)
	gh.reportNoScopes()

	stdout, stderr, code := createApplication(t, platformURL, cli.Dependencies{GitHubAPI: gh.srv.URL},
		"--name", "shop", "--path", "create", "--framework", "nextjs")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if gh.requestsTo(http.MethodGet, "/") == 0 {
		t.Fatalf("test bug: the scopes were never read")
	}
	assertFileExists(t, cloneAppRepo(t, gh, platform.Org, "shop")+"/.github/workflows/deploy.yaml")
}

func TestAppCreateWithoutPathNeverChecksTheWorkflowScope(t *testing.T) {
	// The legacy bare path writes only the Platform repository, which has
	// no workflow the CLI touches: no scope is needed, and GitHub's API is
	// not called at all.
	platformURL := newPlatformRepository(t, testPlatformYAML)
	gh := newFakeGitHub(t)
	gh.setScopes(scopesWithoutWorkflow)

	_, stderr, code := createApplication(t, platformURL, cli.Dependencies{GitHubAPI: gh.srv.URL},
		"--name", "shop", "--kind", "web-service")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr: %s", code, stderr)
	}
	gh.mu.Lock()
	n := len(gh.requests)
	gh.mu.Unlock()
	if n != 0 {
		t.Errorf("the fake GitHub API was called %d times, want 0", n)
	}
}

func TestAppCreateWizardRefusesALoginWithoutTheWorkflowScopeBeforeTheSummary(t *testing.T) {
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML)
	gh := newFakeGitHub(t)
	gh.setScopes(scopesWithoutWorkflow)

	// Only the migration command, domain, login, size and confirmation
	// would be asked; the refusal comes before the summary asks anything.
	stdin := "\n\n\n\ny\n"

	stdout, stderr, code := createApplicationInteractive(t, url, cli.Dependencies{GitHubAPI: gh.srv.URL}, stdin,
		"--name", "shop", "--path", "create", "--framework", "nextjs", "--postgres", "--staging")

	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero\nstdout: %s", stdout)
	}
	if !strings.Contains(stderr, refreshCommand) {
		t.Errorf("stderr lacks %q:\n%s", refreshCommand, stderr)
	}
	if strings.Contains(stdout, "Summary:") {
		t.Errorf("the summary was shown before refusing:\n%s", stdout)
	}
	if n := gh.requestsTo(http.MethodPost, "/orgs/"+platform.Org+"/repos"); n != 0 {
		t.Errorf("POST /orgs/%s/repos called %d times, want 0", platform.Org, n)
	}
	assertNoApplications(t, url)
}

func TestAppAdoptRefusesALoginWithoutTheWorkflowScope(t *testing.T) {
	platformURL := newPlatformRepository(t, testPlatformYAML)
	gh := newFakeGitHub(t)
	_, bare := gh.seedAdoptRepository(t, platform.Org, "shop", "main", map[string]string{"package.json": nextJSPackageJSON})
	gh.setScopes(scopesWithoutWorkflow)

	stdout, stderr, code := createApplication(t, platformURL, cli.Dependencies{GitHubAPI: gh.srv.URL},
		"--path", "adopt", "--repo", platform.Org+"/shop")

	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero\nstdout: %s", stdout)
	}
	if !strings.Contains(stderr, refreshCommand) {
		t.Errorf("stderr lacks %q:\n%s", refreshCommand, stderr)
	}
	if len(gh.pullRequestsTo(platform.Org, "shop")) != 0 {
		t.Errorf("a pull request was opened despite the missing workflow scope")
	}
	assertBranchAbsent(t, bare, apprepo.AdoptBranch)
	assertNoApplications(t, platformURL)
}

func TestAppAdoptNeedsNoWorkflowScopeWhenTheRepositoryAlreadyHasTheWorkflow(t *testing.T) {
	// Adopt never touches an existing deploy workflow, so its pull request
	// adds only the Dockerfile, which needs no scope beyond repo.
	platformURL := newPlatformRepository(t, testPlatformYAML)
	gh := newFakeGitHub(t)
	cloneURL, _ := gh.seedAdoptRepository(t, platform.Org, "shop", "main", map[string]string{
		"package.json":                  nextJSPackageJSON,
		".github/workflows/deploy.yaml": "name: deploy\n",
	})
	gh.setScopes(scopesWithoutWorkflow)

	stdout, stderr, code := createApplication(t, platformURL, cli.Dependencies{GitHubAPI: gh.srv.URL},
		"--path", "adopt", "--repo", platform.Org+"/shop")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	assertOnlyAddedFiles(t, cloneURL, apprepo.AdoptBranch, []string{"Dockerfile", ".dockerignore"})
	if len(gh.pullRequestsTo(platform.Org, "shop")) != 1 {
		t.Errorf("pull requests opened = %d, want 1", len(gh.pullRequestsTo(platform.Org, "shop")))
	}
}

func TestAppAdoptWizardRefusesALoginWithoutTheWorkflowScopeBeforeTheSummary(t *testing.T) {
	url := newPlatformRepository(t, testPlatformYAML)
	gh := newFakeGitHub(t)
	_, bare := gh.seedAdoptRepository(t, platform.Org, "shop", "main", map[string]string{"package.json": nextJSPackageJSON})
	gh.setScopes(scopesWithoutWorkflow)

	// name, path, repository, postgres, staging, domain, login, size, confirm.
	stdin := "shop\nadopt\n" + platform.Org + "/shop\nn\nn\n\n\n\ny\n"

	stdout, stderr, code := createApplicationInteractive(t, url, cli.Dependencies{GitHubAPI: gh.srv.URL}, stdin)

	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero\nstdout: %s", stdout)
	}
	if !strings.Contains(stderr, refreshCommand) {
		t.Errorf("stderr lacks %q:\n%s", refreshCommand, stderr)
	}
	if strings.Contains(stdout, "Summary:") {
		t.Errorf("the summary was shown before refusing:\n%s", stdout)
	}
	if len(gh.pullRequestsTo(platform.Org, "shop")) != 0 {
		t.Errorf("a pull request was opened despite the missing workflow scope")
	}
	assertBranchAbsent(t, bare, apprepo.AdoptBranch)
	assertNoApplications(t, url)
}
