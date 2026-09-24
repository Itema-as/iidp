package cli_test

import (
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/Itema-as/iidp/internal/apprepo"
	"github.com/Itema-as/iidp/internal/cli"
	"github.com/Itema-as/iidp/internal/platform"
)

// Fixture package.json bodies for framework detection
// (docs/implementation-notes/15-cli-adopt-path.md): Next.js keeps next as a
// plain dependency, Vite React keeps vite as a devDependency with react as
// a dependency (the shape create-vite itself produces), and a plain one
// carries neither.
const (
	nextJSPackageJSON    = `{"name":"shop","dependencies":{"next":"16.0.0","react":"19.0.0","react-dom":"19.0.0"}}`
	viteReactPackageJSON = `{"name":"shop","dependencies":{"react":"19.0.0","react-dom":"19.0.0"},"devDependencies":{"vite":"8.0.0"}}`
	plainPackageJSON     = `{"name":"shop","dependencies":{"express":"4.0.0"}}`
	adoptedDockerfile    = "FROM node:24-slim\nEXPOSE 3000\nCMD [\"node\", \"server.js\"]\n"
	adoptedPrismaSchema  = "// schema\n"
)

// cloneBranch clones cloneURL's branch into a fresh directory.
func cloneBranch(t *testing.T, url, branch string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "clone")
	gitRun(t, t.TempDir(), "clone", "--quiet", "--branch", branch, url, dir)
	return dir
}

// assertOnlyAddedFiles clones cloneURL, fetches branch, and asserts that
// the diff between the default branch and branch touches only want, all as
// additions (status A) — the ticket's "the pull request contains only the
// added files" assertion, checked by diffing the pushed branch against the
// default branch.
func assertOnlyAddedFiles(t *testing.T, cloneURL, branch string, want []string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "diff")
	gitRun(t, t.TempDir(), "clone", "--quiet", cloneURL, dir)
	gitRun(t, dir, "fetch", "--quiet", "origin", branch)
	out := strings.TrimSpace(gitRun(t, dir, "diff", "--name-status", "HEAD", "FETCH_HEAD"))

	var got []string
	if out != "" {
		for _, line := range strings.Split(out, "\n") {
			parts := strings.SplitN(line, "\t", 2)
			if len(parts) != 2 {
				t.Fatalf("unexpected diff line %q", line)
			}
			if parts[0] != "A" {
				t.Errorf("file %q has diff status %q, want A (added, never modified or deleted)", parts[1], parts[0])
			}
			got = append(got, parts[1])
		}
	}
	sort.Strings(got)
	wantSorted := append([]string(nil), want...)
	sort.Strings(wantSorted)
	if strings.Join(got, ",") != strings.Join(wantSorted, ",") {
		t.Errorf("files added on %s vs the default branch = %v, want %v", branch, got, wantSorted)
	}
}

// pushExistingBranch simulates a pre-existing AdoptBranch on cloneURL, so
// tests can assert that Adopt refuses rather than push over it.
func pushExistingBranch(t *testing.T, cloneURL, branch string) {
	t.Helper()
	dir := cloneMain(t, cloneURL)
	gitRun(t, dir, "checkout", "-b", branch)
	gitRun(t, dir, "commit", "--allow-empty", "-m", "pre-existing branch")
	gitRun(t, dir, "push", "origin", "HEAD:refs/heads/"+branch)
}

func TestAppAdoptWithoutDockerfileDetectsNextJSAndGeneratesIt(t *testing.T) {
	platformURL := newPlatformRepository(t, testPlatformYAML)
	gh := newFakeGitHub(t)
	gh.seedAdoptRepository(t, platform.Org, "shop", "main", map[string]string{
		"package.json": nextJSPackageJSON,
		"README.md":    "# shop\n",
	})

	stdout, stderr, code := createApplication(t, platformURL, cli.Dependencies{GitHubAPI: gh.srv.URL},
		"--path", "adopt", "--repo", platform.Org+"/shop")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}

	cloneURL := gh.cloneURL(platform.Org, "shop")
	wantFiles := []string{"Dockerfile", ".dockerignore", ".github/workflows/deploy.yaml"}
	assertOnlyAddedFiles(t, cloneURL, apprepo.AdoptBranch, wantFiles)

	branchClone := cloneBranch(t, cloneURL, apprepo.AdoptBranch)
	if got := strings.TrimSpace(gitRun(t, branchClone, "log", "-1", "--format=%s")); !strings.Contains(got, "adopt") {
		t.Errorf("commit subject = %q, want it to mention adopt", got)
	}
	dockerfile, err := os.ReadFile(filepath.Join(branchClone, "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(dockerfile), "PORT") {
		t.Errorf("Dockerfile lacks PORT (Next.js template expected):\n%s", dockerfile)
	}
	workflow, err := os.ReadFile(filepath.Join(branchClone, ".github", "workflows", "deploy.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(workflow), "ghcr.io/"+strings.ToLower(platform.Org)+"/shop") {
		t.Errorf("deploy.yaml lacks the expected image reference:\n%s", workflow)
	}

	prs := gh.pullRequestsTo(platform.Org, "shop")
	if len(prs) != 1 {
		t.Fatalf("pull requests opened = %d, want 1", len(prs))
	}
	pr := prs[0]
	if pr.Base != "main" || pr.Head != apprepo.AdoptBranch {
		t.Errorf("pull request base/head = %q/%q, want main/%s", pr.Base, pr.Head, apprepo.AdoptBranch)
	}
	for _, want := range wantFiles {
		if !strings.Contains(pr.Body, want) {
			t.Errorf("pull request body lacks %q:\n%s", want, pr.Body)
		}
	}
	if !strings.Contains(pr.Body, "first merged run") {
		t.Errorf("pull request body lacks what happens on merge:\n%s", pr.Body)
	}
	if !strings.Contains(stdout, "pull/1") {
		t.Errorf("stdout lacks the pull request URL:\n%s", stdout)
	}

	values := readYAML(t, filepath.Join(cloneMain(t, platformURL), "applications/shop/prod/values.yaml"))
	if got := lookup(t, values, "kind"); got != "web-service" {
		t.Errorf("values.yaml kind = %v, want web-service (derived from Next.js)", got)
	}
	if got := lookup(t, values, "image", "repository"); got != "ghcr.io/"+strings.ToLower(platform.Org)+"/shop" {
		t.Errorf("values.yaml image.repository = %v", got)
	}
	if !strings.Contains(stdout, "https://shop.app.itma.no") {
		t.Errorf("stdout lacks the Application's address:\n%s", stdout)
	}
}

func TestAppAdoptWithoutDockerfileDetectsViteReactAndGeneratesStaticSite(t *testing.T) {
	platformURL := newPlatformRepository(t, testPlatformYAML)
	gh := newFakeGitHub(t)
	gh.seedAdoptRepository(t, platform.Org, "storefront", "main", map[string]string{
		"package.json": viteReactPackageJSON,
	})

	stdout, stderr, code := createApplication(t, platformURL, cli.Dependencies{GitHubAPI: gh.srv.URL},
		"--path", "adopt", "--repo", platform.Org+"/storefront")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	cloneURL := gh.cloneURL(platform.Org, "storefront")
	branchClone := cloneBranch(t, cloneURL, apprepo.AdoptBranch)
	if _, err := os.Stat(filepath.Join(branchClone, "index.html")); err == nil {
		t.Errorf("index.html exists: Adopt must add only the Dockerfile and .dockerignore, not the whole framework template")
	}
	dockerfile, err := os.ReadFile(filepath.Join(branchClone, "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(dockerfile), "nginx") {
		t.Errorf("Dockerfile lacks nginx (Vite React template expected):\n%s", dockerfile)
	}

	values := readYAML(t, filepath.Join(cloneMain(t, platformURL), "applications/storefront/prod/values.yaml"))
	if got := lookup(t, values, "kind"); got != "static-site" {
		t.Errorf("values.yaml kind = %v, want static-site (derived from Vite React)", got)
	}
}

func TestAppAdoptNoKnownFrameworkRequiresKindAndProducesTheOtherStub(t *testing.T) {
	platformURL := newPlatformRepository(t, testPlatformYAML)
	gh := newFakeGitHub(t)
	gh.seedAdoptRepository(t, platform.Org, "legacy-api", "main", map[string]string{
		"package.json": plainPackageJSON,
	})

	t.Run("without --kind, refused", func(t *testing.T) {
		_, stderr, code := createApplication(t, platformURL, cli.Dependencies{GitHubAPI: gh.srv.URL},
			"--path", "adopt", "--repo", platform.Org+"/legacy-api")
		if code == 0 {
			t.Fatalf("exit code = 0, want non-zero")
		}
		if !strings.Contains(stderr, "kind") {
			t.Errorf("stderr = %q, want it to mention kind", stderr)
		}
		if len(gh.pullRequestsTo(platform.Org, "legacy-api")) != 0 {
			t.Errorf("a pull request was opened despite the missing --kind")
		}
	})

	t.Run("with --kind, the Other stub is added", func(t *testing.T) {
		_, stderr, code := createApplication(t, platformURL, cli.Dependencies{GitHubAPI: gh.srv.URL},
			"--path", "adopt", "--repo", platform.Org+"/legacy-api", "--kind", "web-service")
		if code != 0 {
			t.Fatalf("exit code = %d, want 0\nstderr: %s", code, stderr)
		}
		cloneURL := gh.cloneURL(platform.Org, "legacy-api")
		branchClone := cloneBranch(t, cloneURL, apprepo.AdoptBranch)
		dockerfile, err := os.ReadFile(filepath.Join(branchClone, "Dockerfile"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(dockerfile), "PORT") {
			t.Errorf("Other stub Dockerfile lacks PORT guidance:\n%s", dockerfile)
		}
	})
}

func TestAppAdoptWithExistingDockerfileIsNeverModifiedAndRequiresKind(t *testing.T) {
	platformURL := newPlatformRepository(t, testPlatformYAML)
	gh := newFakeGitHub(t)
	gh.seedAdoptRepository(t, platform.Org, "shop", "main", map[string]string{
		"Dockerfile": adoptedDockerfile,
	})

	t.Run("without --kind, refused", func(t *testing.T) {
		_, stderr, code := createApplication(t, platformURL, cli.Dependencies{GitHubAPI: gh.srv.URL},
			"--path", "adopt", "--repo", platform.Org+"/shop")
		if code == 0 {
			t.Fatalf("exit code = 0, want non-zero")
		}
		if !strings.Contains(stderr, "kind") || !strings.Contains(stderr, "Dockerfile") {
			t.Errorf("stderr = %q, want it to explain the Dockerfile forces --kind", stderr)
		}
	})

	t.Run("with --kind, only the deploy workflow is added", func(t *testing.T) {
		_, stderr, code := createApplication(t, platformURL, cli.Dependencies{GitHubAPI: gh.srv.URL},
			"--path", "adopt", "--repo", platform.Org+"/shop", "--kind", "web-service")
		if code != 0 {
			t.Fatalf("exit code = %d, want 0\nstderr: %s", code, stderr)
		}
		cloneURL := gh.cloneURL(platform.Org, "shop")
		assertOnlyAddedFiles(t, cloneURL, apprepo.AdoptBranch, []string{".github/workflows/deploy.yaml"})

		branchClone := cloneBranch(t, cloneURL, apprepo.AdoptBranch)
		dockerfile, err := os.ReadFile(filepath.Join(branchClone, "Dockerfile"))
		if err != nil {
			t.Fatal(err)
		}
		if string(dockerfile) != adoptedDockerfile {
			t.Errorf("the existing Dockerfile was modified:\n%s", dockerfile)
		}

		values := readYAML(t, filepath.Join(cloneMain(t, platformURL), "applications/shop/prod/values.yaml"))
		if got := lookup(t, values, "kind"); got != "web-service" {
			t.Errorf("values.yaml kind = %v, want web-service (from --kind)", got)
		}
	})
}

func TestAppAdoptRefusesWithoutPushAccess(t *testing.T) {
	platformURL := newPlatformRepository(t, testPlatformYAML)
	gh := newFakeGitHub(t)
	gh.seedAdoptRepository(t, platform.Org, "shop", "main", map[string]string{"package.json": nextJSPackageJSON})
	gh.denyPush(platform.Org, "shop")

	_, stderr, code := createApplication(t, platformURL, cli.Dependencies{GitHubAPI: gh.srv.URL},
		"--path", "adopt", "--repo", platform.Org+"/shop")

	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero")
	}
	if !strings.Contains(stderr, "write access") {
		t.Errorf("stderr = %q, want it to explain the missing push access", stderr)
	}
	if len(gh.pullRequestsTo(platform.Org, "shop")) != 0 {
		t.Errorf("a pull request was opened despite no push access")
	}
	assertNoApplications(t, platformURL)
}

func TestAppAdoptRefusesWhenTheBranchAlreadyExists(t *testing.T) {
	platformURL := newPlatformRepository(t, testPlatformYAML)
	gh := newFakeGitHub(t)
	cloneURL, _ := gh.seedAdoptRepository(t, platform.Org, "shop", "main", map[string]string{"package.json": nextJSPackageJSON})
	pushExistingBranch(t, cloneURL, apprepo.AdoptBranch)

	_, stderr, code := createApplication(t, platformURL, cli.Dependencies{GitHubAPI: gh.srv.URL},
		"--path", "adopt", "--repo", platform.Org+"/shop")

	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero")
	}
	if !strings.Contains(stderr, apprepo.AdoptBranch) || !strings.Contains(stderr, "already exists") {
		t.Errorf("stderr = %q, want it to say the branch already exists", stderr)
	}
	if len(gh.pullRequestsTo(platform.Org, "shop")) != 0 {
		t.Errorf("a pull request was opened despite the branch already existing")
	}
	assertNoApplications(t, platformURL)
}

func TestAppAdoptRefusesARepositoryOutsideTheOrgBeforeAnything(t *testing.T) {
	for _, repo := range []string{"developer42/shop", "https://github.com/developer42/shop"} {
		t.Run(repo, func(t *testing.T) {
			platformURL := newPlatformRepository(t, testPlatformYAML)
			gh := newFakeGitHub(t)
			_, bare := gh.seedAdoptRepository(t, "developer42", "shop", "main", map[string]string{"package.json": nextJSPackageJSON})

			stdout, stderr, code := createApplication(t, platformURL, cli.Dependencies{GitHubAPI: gh.srv.URL},
				"--path", "adopt", "--repo", repo)

			if code == 0 {
				t.Fatalf("exit code = 0, want non-zero\nstdout: %s", stdout)
			}
			for _, want := range []string{"developer42/shop", "Transfer it to " + platform.Org, "--repo " + platform.Org + "/shop"} {
				if !strings.Contains(stderr, want) {
					t.Errorf("stderr lacks %q:\n%s", want, stderr)
				}
			}
			if len(gh.requests) != 0 {
				t.Errorf("the fake GitHub API was called %d times, want 0: the refusal comes before anything else", len(gh.requests))
			}
			assertBranchAbsent(t, bare, apprepo.AdoptBranch)
			assertNoApplications(t, platformURL)
		})
	}
}

func TestAppAdoptRefusesARepositoryGitHubReportsOutsideTheOrg(t *testing.T) {
	// --repo names the org, but the repository was transferred away since:
	// GitHub follows the redirect and reports the new owner. Adopt checks
	// the owner GitHub reports, not only the one typed.
	platformURL := newPlatformRepository(t, testPlatformYAML)
	gh := newFakeGitHub(t)
	_, bare := gh.seedAdoptRepository(t, platform.Org, "shop", "main", map[string]string{"package.json": nextJSPackageJSON})
	gh.transferAway(platform.Org, "shop", "developer42")

	_, stderr, code := createApplication(t, platformURL, cli.Dependencies{GitHubAPI: gh.srv.URL},
		"--path", "adopt", "--repo", platform.Org+"/shop")

	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero")
	}
	if !strings.Contains(stderr, "developer42/shop") || !strings.Contains(stderr, "Transfer it to "+platform.Org) {
		t.Errorf("stderr = %q, want it to name the owner GitHub reports and the transfer", stderr)
	}
	assertBranchAbsent(t, bare, apprepo.AdoptBranch)
	if len(gh.pullRequestsTo(platform.Org, "shop")) != 0 {
		t.Errorf("a pull request was opened on a repository outside the org")
	}
	assertNoApplications(t, platformURL)
}

func TestAppAdoptBindsTheApplicationRepositoryByID(t *testing.T) {
	platformURL := newPlatformRepository(t, testPlatformYAML)
	gh := newFakeGitHub(t)
	gh.seedAdoptRepository(t, platform.Org, "shop", "main", map[string]string{"package.json": nextJSPackageJSON})

	stdout, stderr, code := createApplication(t, platformURL, cli.Dependencies{GitHubAPI: gh.srv.URL},
		"--path", "adopt", "--repo", platform.Org+"/shop", "--name", "storefront")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	clone := cloneMain(t, platformURL)
	// Bound under the Application's name, to the repository by the ids and
	// full name GitHub reports.
	assertBinding(t, clone, "storefront", platform.Org+"/shop", gh.repoID(platform.Org, "shop"), fakeOrgID)
	if got := headSubject(t, platformURL); got != "iidp app create storefront" {
		t.Errorf("head commit = %q, want iidp app create storefront", got)
	}
	added := strings.Fields(gitRun(t, clone, "show", "--name-only", "--format=", "HEAD"))
	if !slices.Contains(added, "applications/storefront/repository.yaml") {
		t.Errorf("the create commit touched %v, want it to include applications/storefront/repository.yaml", added)
	}
}

func TestAppAdoptOrgOwnerGetsNoSecretNote(t *testing.T) {
	platformURL := newPlatformRepository(t, testPlatformYAML)
	gh := newFakeGitHub(t)
	gh.seedAdoptRepository(t, platform.Org, "shop", "main", map[string]string{"package.json": nextJSPackageJSON})

	stdout, stderr, code := createApplication(t, platformURL, cli.Dependencies{GitHubAPI: gh.srv.URL},
		"--path", "adopt", "--repo", platform.Org+"/shop")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr: %s", code, stderr)
	}
	if strings.Contains(stdout, "by hand") {
		t.Errorf("stdout should not mention adding the secret by hand for an org owner:\n%s", stdout)
	}
}

func TestAppAdoptDetectsMigrationToolingFromTheClone(t *testing.T) {
	platformURL := newPlatformRepository(t, testCapabilitiesPlatformYAML)
	gh := newFakeGitHub(t)
	gh.seedAdoptRepository(t, platform.Org, "shop", "main", map[string]string{
		"package.json":         nextJSPackageJSON,
		"prisma/schema.prisma": adoptedPrismaSchema,
	})

	stdout, stderr, code := createApplication(t, platformURL, cli.Dependencies{GitHubAPI: gh.srv.URL},
		"--path", "adopt", "--repo", platform.Org+"/shop", "--postgres")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "Prisma") {
		t.Errorf("stdout lacks the detected migration tooling:\n%s", stdout)
	}
	values := readYAML(t, filepath.Join(cloneMain(t, platformURL), "applications/shop/prod/values.yaml"))
	if got := lookup(t, values, "postgres", "migrationCommand"); got != "npx prisma migrate deploy" {
		t.Errorf("values.yaml postgres.migrationCommand = %v, want the detected Prisma command", got)
	}
}

func TestAppAdoptNameDefaultsToRepositoryNameAndCanBeOverridden(t *testing.T) {
	platformURL := newPlatformRepository(t, testPlatformYAML)

	t.Run("defaults to the repository name", func(t *testing.T) {
		gh := newFakeGitHub(t)
		gh.seedAdoptRepository(t, platform.Org, "shop", "main", map[string]string{"package.json": nextJSPackageJSON})
		_, stderr, code := createApplication(t, platformURL, cli.Dependencies{GitHubAPI: gh.srv.URL},
			"--path", "adopt", "--repo", platform.Org+"/shop")
		if code != 0 {
			t.Fatalf("exit code = %d, want 0\nstderr: %s", code, stderr)
		}
		if _, err := os.Stat(filepath.Join(cloneMain(t, platformURL), "applications/shop/prod/values.yaml")); err != nil {
			t.Errorf("Application name did not default to the repository name: %v", err)
		}
	})

	t.Run("--name overrides", func(t *testing.T) {
		gh := newFakeGitHub(t)
		gh.seedAdoptRepository(t, platform.Org, "shop", "main", map[string]string{"package.json": nextJSPackageJSON})
		_, stderr, code := createApplication(t, platformURL, cli.Dependencies{GitHubAPI: gh.srv.URL},
			"--path", "adopt", "--repo", platform.Org+"/shop", "--name", "storefront")
		if code != 0 {
			t.Fatalf("exit code = %d, want 0\nstderr: %s", code, stderr)
		}
		if _, err := os.Stat(filepath.Join(cloneMain(t, platformURL), "applications/storefront/prod/values.yaml")); err != nil {
			t.Errorf("--name did not override the Application name: %v", err)
		}
	})
}

func TestAppAdoptAcceptsRepoAsAURL(t *testing.T) {
	platformURL := newPlatformRepository(t, testPlatformYAML)
	gh := newFakeGitHub(t)
	gh.seedAdoptRepository(t, platform.Org, "shop", "main", map[string]string{"package.json": nextJSPackageJSON})

	_, stderr, code := createApplication(t, platformURL, cli.Dependencies{GitHubAPI: gh.srv.URL},
		"--path", "adopt", "--repo", "https://github.com/"+platform.Org+"/shop")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr: %s", code, stderr)
	}
	if len(gh.pullRequestsTo(platform.Org, "shop")) != 1 {
		t.Errorf("a pull request was not opened for a --repo given as a URL")
	}
}

func TestAppAdoptRequiresRepoFlag(t *testing.T) {
	platformURL := newPlatformRepository(t, testPlatformYAML)

	_, stderr, code := createApplication(t, platformURL, cli.Dependencies{}, "--path", "adopt")

	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero")
	}
	if !strings.Contains(stderr, "repo") {
		t.Errorf("stderr = %q, want it to mention the missing --repo flag", stderr)
	}
}

func TestAppAdoptRepoRequiresPathAdopt(t *testing.T) {
	platformURL := newPlatformRepository(t, testPlatformYAML)

	_, stderr, code := createApplication(t, platformURL, cli.Dependencies{}, "--name", "shop", "--kind", "web-service", "--repo", "org/name")

	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero")
	}
	if !strings.Contains(stderr, "--path adopt") {
		t.Errorf("stderr = %q, want it to say --repo requires --path adopt", stderr)
	}
}

func TestAppAdoptCapabilitiesApplyTheSameAsCreate(t *testing.T) {
	platformURL := newPlatformRepository(t, testCapabilitiesPlatformYAML)
	gh := newFakeGitHub(t)
	gh.seedAdoptRepository(t, platform.Org, "shop", "main", map[string]string{"package.json": nextJSPackageJSON})

	_, stderr, code := createApplication(t, platformURL, cli.Dependencies{GitHubAPI: gh.srv.URL},
		"--path", "adopt", "--repo", platform.Org+"/shop", "--postgres", "--staging", "--size", "medium",
		"--domain", "shop.example.com")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr: %s", code, stderr)
	}
	clone := cloneMain(t, platformURL)
	for _, env := range []string{"prod", "staging"} {
		values := readYAML(t, filepath.Join(clone, "applications/shop", env, "values.yaml"))
		if got := lookup(t, values, "postgres", "enabled"); got != true {
			t.Errorf("%s postgres.enabled = %v, want true", env, got)
		}
		if got := lookup(t, values, "size"); got != "medium" {
			t.Errorf("%s size = %v, want medium", env, got)
		}
	}
	prodValues := readYAML(t, filepath.Join(clone, "applications/shop/prod/values.yaml"))
	if got, ok := lookup(t, prodValues, "domains").([]any); !ok || len(got) != 1 || got[0] != "shop.example.com" {
		t.Errorf("prod domains = %v, want [shop.example.com]", lookup(t, prodValues, "domains"))
	}
	stagingValues := readYAML(t, filepath.Join(clone, "applications/shop/staging/values.yaml"))
	if got, ok := lookup(t, stagingValues, "domains").([]any); !ok || len(got) != 0 {
		t.Errorf("staging domains = %v, want empty (custom domains are prod-only)", lookup(t, stagingValues, "domains"))
	}
}

// TestAppAdoptRefusesPostgresOnAStaticSiteBeforeOpeningAPullRequest checks
// that --postgres against a detected Static site (Vite React, no --kind
// override) is refused before any branch is pushed or pull request opened
// — not after, the way a check running only once Adopter.Adopt has
// already committed to writing would (docs/implementation-notes/15-cli-adopt-path.md).
func TestAppAdoptRefusesPostgresOnAStaticSiteBeforeOpeningAPullRequest(t *testing.T) {
	platformURL := newPlatformRepository(t, testCapabilitiesPlatformYAML)
	gh := newFakeGitHub(t)
	gh.seedAdoptRepository(t, platform.Org, "storefront", "main", map[string]string{
		"package.json": viteReactPackageJSON,
	})

	_, stderr, code := createApplication(t, platformURL, cli.Dependencies{GitHubAPI: gh.srv.URL},
		"--path", "adopt", "--repo", platform.Org+"/storefront", "--postgres")

	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero")
	}
	if !strings.Contains(stderr, "--postgres") {
		t.Errorf("stderr = %q, want it to explain the Postgres/Static site conflict", stderr)
	}
	if len(gh.pullRequestsTo(platform.Org, "storefront")) != 0 {
		t.Errorf("a pull request was opened despite the Postgres/Static site conflict")
	}
	if gh.requestsTo("GET", "/branches/"+apprepo.AdoptBranch) == 0 {
		t.Fatalf("test bug: the branch-exists check was never made")
	}
	assertNoApplications(t, platformURL)
}

func TestAppAdoptWizardAsksForRepositoryAndSkipsKindAndFramework(t *testing.T) {
	url := newPlatformRepository(t, testPlatformYAML)
	gh := newFakeGitHub(t)
	gh.seedAdoptRepository(t, platform.Org, "shop", "main", map[string]string{"package.json": nextJSPackageJSON})

	// name, path, repository, postgres, staging, domain, login, size, confirm.
	stdin := "shop\nadopt\n" + platform.Org + "/shop\nn\nn\n\n\n\ny\n"

	stdout, stderr, code := createApplicationInteractive(t, url, cli.Dependencies{GitHubAPI: gh.srv.URL}, stdin)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	for _, want := range []string{"Application repository", "Summary:", "Path:       Adopt", "Pull request adds:", "Dockerfile", ".github/workflows/deploy.yaml"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout)
		}
	}
	for _, notWant := range []string{"Framework:\n", "Kind:\n"} {
		if strings.Contains(stdout, notWant) {
			t.Errorf("stdout asked %q, want Kind/Framework skipped for Adopt:\n%s", notWant, stdout)
		}
	}
	if len(gh.pullRequestsTo(platform.Org, "shop")) != 1 {
		t.Errorf("the wizard did not open a pull request")
	}
}

func TestAppAdoptWizardReasksForARepositoryOutsideTheOrg(t *testing.T) {
	url := newPlatformRepository(t, testPlatformYAML)
	gh := newFakeGitHub(t)
	gh.seedAdoptRepository(t, platform.Org, "shop", "main", map[string]string{"package.json": nextJSPackageJSON})

	// name, path, a repository outside the org (refused, asked again), the
	// org's repository, postgres, staging, domain, login, size, confirm.
	stdin := "shop\nadopt\ndeveloper42/shop\n" + platform.Org + "/shop\nn\nn\n\n\n\ny\n"

	stdout, stderr, code := createApplicationInteractive(t, url, cli.Dependencies{GitHubAPI: gh.srv.URL}, stdin)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "Transfer it to "+platform.Org) {
		t.Errorf("stdout lacks the transfer message for the refused answer:\n%s", stdout)
	}
	if n := strings.Count(stdout, "Application repository ("); n != 2 {
		t.Errorf("the repository question was asked %d times, want 2:\n%s", n, stdout)
	}
	assertBinding(t, cloneMain(t, url), "shop", platform.Org+"/shop", gh.repoID(platform.Org, "shop"), fakeOrgID)
}
