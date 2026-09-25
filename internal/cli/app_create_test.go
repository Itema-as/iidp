package cli_test

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/Itema-as/iidp/internal/cli"
	"github.com/Itema-as/iidp/internal/platform"
)

// The Platform repository is stood in for by a local bare git repository,
// seeded through a temporary clone with platform.yaml and an empty
// applications/ directory, the layout docs/platform-repository.md describes.

const testPlatformYAML = `baseDomain: app.itma.no
chartVersion: 0.3.1
argocdURL: https://argocd.platform.itma.no
grafanaURL: https://itema.grafana.net
somethingAnotherTicketAdds: true
`

// fakeTokenSource stands in for the gh CLI.
type fakeTokenSource struct {
	token string
	err   error
}

func (f fakeTokenSource) Token() (string, error) { return f.token, f.err }

// setGitEnv makes git commits work on any machine, including CI runners
// with no git identity, and isolates the tests from the developer's own
// global git configuration (signing, hooks, credential helpers).
func setGitEnv(t *testing.T) {
	t.Helper()
	empty := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", empty)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_AUTHOR_NAME", "Test Developer")
	t.Setenv("GIT_AUTHOR_EMAIL", "developer@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "Test Developer")
	t.Setenv("GIT_COMMITTER_EMAIL", "developer@example.com")
}

// gitRun runs git in dir and fails the test on error.
func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out.String())
	}
	return out.String()
}

// newPlatformRepository creates a bare repository whose main branch holds
// platform.yaml, an empty applications/ directory and
// bootstrap/templates/backups-credentials.enc.yaml (the file the bootstrap wizard
// writes once for the Platform's age key, docs/implementation-notes/42-backups-credentials.md),
// all in the one "Seed the Platform repository" commit every existing test
// already expects: every --postgres test needs something for the CLI to
// copy, so it is seeded here rather than by each test individually. Tests
// that exercise its absence use newPlatformRepositoryWithoutBackupsCredentials
// instead. Returns the repository's file:// URL.
func newPlatformRepository(t *testing.T, platformYAML string) string {
	t.Helper()
	return newSeededPlatformRepository(t, platformYAML, true)
}

// newPlatformRepositoryWithoutBackupsCredentials is newPlatformRepository
// without bootstrap/templates/backups-credentials.enc.yaml: only tests asserting the
// CLI's refusal when the file is missing use this directly.
func newPlatformRepositoryWithoutBackupsCredentials(t *testing.T, platformYAML string) string {
	t.Helper()
	return newSeededPlatformRepository(t, platformYAML, false)
}

func newSeededPlatformRepository(t *testing.T, platformYAML string, withBackupsCredentials bool) string {
	t.Helper()
	setGitEnv(t)
	bare := filepath.Join(t.TempDir(), "iidp-platform.git")
	gitRun(t, t.TempDir(), "init", "--bare", "--initial-branch=main", bare)
	url := "file://" + bare

	seed := cloneMain(t, url)
	writeFile(t, filepath.Join(seed, "platform.yaml"), platformYAML)
	writeFile(t, filepath.Join(seed, "applications", ".gitkeep"), "")
	if withBackupsCredentials {
		writeFile(t, filepath.Join(seed, "bootstrap", "templates", "backups-credentials.enc.yaml"), string(readBackupsCredentialsFixture(t)))
	}
	gitRun(t, seed, "add", "-A")
	gitRun(t, seed, "commit", "-m", "Seed the Platform repository")
	gitRun(t, seed, "push", "origin", "HEAD:main")
	return url
}

// seedApplicationRepository creates a bare repository seeded with files on
// defaultBranch, the way an existing Application repository the Adopt path
// reads looks from the outside, and returns its bare directory (so a test
// can push a branch to it directly, simulating one already present) and
// its file:// clone URL.
func seedApplicationRepository(t *testing.T, defaultBranch string, files map[string]string) (bareDir, cloneURL string) {
	t.Helper()
	setGitEnv(t)
	bare := filepath.Join(t.TempDir(), "app-repo.git")
	gitRun(t, t.TempDir(), "init", "--bare", "--initial-branch="+defaultBranch, bare)
	url := "file://" + bare

	seed := filepath.Join(t.TempDir(), "seed")
	gitRun(t, t.TempDir(), "clone", "--quiet", url, seed)
	for name, content := range files {
		writeFile(t, filepath.Join(seed, name), content)
	}
	gitRun(t, seed, "add", "-A")
	gitRun(t, seed, "commit", "-m", "Initial commit")
	gitRun(t, seed, "push", "origin", "HEAD:"+defaultBranch)
	return bare, url
}

// cloneMain clones the Platform repository into a fresh directory.
func cloneMain(t *testing.T, url string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "clone")
	gitRun(t, t.TempDir(), "clone", "--quiet", url, dir)
	return dir
}

// headSubject is the subject of the newest commit on the Platform
// repository's main.
func headSubject(t *testing.T, url string) string {
	t.Helper()
	return strings.TrimSpace(gitRun(t, cloneMain(t, url), "log", "-1", "--format=%s"))
}

// refusePushes installs a pre-receive hook that refuses every push with
// message, the way a missing write permission arrives at git.
func refusePushes(t *testing.T, url, message string) {
	t.Helper()
	bare := strings.TrimPrefix(url, "file://")
	hook := filepath.Join(bare, "hooks", "pre-receive")
	writeFile(t, hook, "#!/bin/sh\necho \""+message+"\" >&2\nexit 1\n")
	if err := os.Chmod(hook, 0o755); err != nil {
		t.Fatal(err)
	}
}

// pushCommit adds files to the Platform repository behind the CLI's back,
// the way another developer or a hand edit would.
func pushCommit(t *testing.T, url, message string, files map[string]string) {
	t.Helper()
	dir := cloneMain(t, url)
	for name, content := range files {
		writeFile(t, filepath.Join(dir, name), content)
	}
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "-m", message)
	gitRun(t, dir, "push", "origin", "HEAD:main")
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readYAML(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse %s: %v\n%s", path, err, data)
	}
	return doc
}

// lookup walks nested maps and lists by string keys and integer indexes.
func lookup(t *testing.T, doc any, path ...any) any {
	t.Helper()
	cur := doc
	for _, p := range path {
		switch key := p.(type) {
		case string:
			m, ok := cur.(map[string]any)
			if !ok {
				t.Fatalf("looking up %v: %q is not a map (%T)", path, key, cur)
			}
			cur, ok = m[key]
			if !ok {
				t.Fatalf("looking up %v: key %q missing", path, key)
			}
		case int:
			l, ok := cur.([]any)
			if !ok || key >= len(l) {
				t.Fatalf("looking up %v: index %d out of range (%T)", path, key, cur)
			}
			cur = l[key]
		}
	}
	return cur
}

// createApplication runs iidp app create in-process against the Platform
// repository at url with a fake token, the way tests drive the seam.
func createApplication(t *testing.T, url string, deps cli.Dependencies, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	if deps.TokenSource == nil {
		deps.TokenSource = fakeTokenSource{token: "gho_test"}
	}
	full := append([]string{"app", "create", "--platform-repo", url}, args...)
	var out, errOut bytes.Buffer
	code = cli.RunWith(full, strings.NewReader(""), &out, &errOut, deps)
	return out.String(), errOut.String(), code
}

func TestAppCreateWritesProdEnvironmentToPlatformRepository(t *testing.T) {
	url := newPlatformRepository(t, testPlatformYAML)

	stdout, stderr, code := createApplication(t, url, cli.Dependencies{},
		"--name", "shop", "--kind", "web-service")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}

	clone := cloneMain(t, url)
	if got := strings.TrimSpace(gitRun(t, clone, "log", "-1", "--format=%s")); got != "iidp app create shop" {
		t.Errorf("commit subject = %q, want %q", got, "iidp app create shop")
	}
	if got := strings.TrimSpace(gitRun(t, clone, "log", "-1", "--format=%an <%ae>")); got != "Test Developer <developer@example.com>" {
		t.Errorf("commit author = %q, want the developer's git identity", got)
	}
	added := strings.Fields(gitRun(t, clone, "show", "--name-only", "--format=", "HEAD"))
	wantAdded := []string{"applications/shop/prod/application.yaml", "applications/shop/prod/values.yaml"}
	if strings.Join(added, " ") != strings.Join(wantAdded, " ") {
		t.Errorf("commit touched %v, want %v", added, wantAdded)
	}

	app := readYAML(t, filepath.Join(clone, "applications/shop/prod/application.yaml"))
	for _, tc := range []struct {
		path []any
		want any
	}{
		{[]any{"apiVersion"}, "argoproj.io/v1alpha1"},
		{[]any{"kind"}, "Application"},
		{[]any{"metadata", "name"}, "shop-prod"},
		{[]any{"metadata", "namespace"}, "argocd"},
		{[]any{"spec", "project"}, "default"},
		{[]any{"spec", "sources", 0, "repoURL"}, platform.Registry + "/charts"},
		{[]any{"spec", "sources", 0, "chart"}, "application"},
		{[]any{"spec", "sources", 0, "targetRevision"}, "0.3.1"},
		{[]any{"spec", "sources", 0, "helm", "valueFiles", 0}, "$values/applications/shop/prod/values.yaml"},
		{[]any{"spec", "sources", 1, "repoURL"}, platform.RepositoryURL},
		{[]any{"spec", "sources", 1, "targetRevision"}, "main"},
		{[]any{"spec", "sources", 1, "ref"}, "values"},
		{[]any{"spec", "destination", "server"}, "https://kubernetes.default.svc"},
		{[]any{"spec", "destination", "namespace"}, "shop-prod"},
		{[]any{"spec", "syncPolicy", "automated", "prune"}, true},
		{[]any{"spec", "syncPolicy", "automated", "selfHeal"}, true},
		{[]any{"spec", "syncPolicy", "syncOptions", 0}, "CreateNamespace=true"},
		// A retry is always against the newest commit, so a fix pushed while
		// a sync keeps failing is picked up (#75).
		{[]any{"spec", "syncPolicy", "retry", "limit"}, -1},
		{[]any{"spec", "syncPolicy", "retry", "refresh"}, true},
		{[]any{"spec", "syncPolicy", "retry", "backoff", "duration"}, "10s"},
		{[]any{"spec", "syncPolicy", "retry", "backoff", "factor"}, 2},
		{[]any{"spec", "syncPolicy", "retry", "backoff", "maxDuration"}, "3m"},
	} {
		if got := lookup(t, app, tc.path...); got != tc.want {
			t.Errorf("application.yaml %v = %v, want %v", tc.path, got, tc.want)
		}
	}
	if _, hasChart := lookup(t, app, "spec", "sources", 1).(map[string]any)["chart"]; hasChart {
		t.Errorf("the values source must not set chart")
	}

	values := readYAML(t, filepath.Join(clone, "applications/shop/prod/values.yaml"))
	for _, tc := range []struct {
		path []any
		want any
	}{
		{[]any{"application", "name"}, "shop"},
		{[]any{"environment"}, "prod"},
		{[]any{"platform", "baseDomain"}, "app.itma.no"},
		{[]any{"kind"}, "web-service"},
		{[]any{"image", "repository"}, platform.Registry + "/shop"},
		{[]any{"image", "tag"}, ""},
		{[]any{"size"}, "small"},
		{[]any{"port"}, 3000},
		{[]any{"probe", "path"}, "/"},
	} {
		if got := lookup(t, values, tc.path...); got != tc.want {
			t.Errorf("values.yaml %v = %v (%T), want %v", tc.path, got, got, tc.want)
		}
	}
	if env, ok := lookup(t, values, "env").(map[string]any); !ok || len(env) != 0 {
		t.Errorf("values.yaml env = %v, want an empty map", lookup(t, values, "env"))
	}

	for _, want := range []string{"https://shop.app.itma.no", "https://argocd.platform.itma.no", "https://itema.grafana.net"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout)
		}
	}
}

func TestAppCreateHonoursSizeImagePortAndProbeFlags(t *testing.T) {
	url := newPlatformRepository(t, testPlatformYAML)

	_, stderr, code := createApplication(t, url, cli.Dependencies{},
		"--name", "shop", "--kind", "web-service", "--size", "large",
		"--image", "ghcr.io/someone/shop-image", "--port", "8080", "--probe-path", "/healthz")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr: %s", code, stderr)
	}
	values := readYAML(t, filepath.Join(cloneMain(t, url), "applications/shop/prod/values.yaml"))
	for _, tc := range []struct {
		path []any
		want any
	}{
		{[]any{"size"}, "large"},
		{[]any{"image", "repository"}, "ghcr.io/someone/shop-image"},
		{[]any{"port"}, 8080},
		{[]any{"probe", "path"}, "/healthz"},
	} {
		if got := lookup(t, values, tc.path...); got != tc.want {
			t.Errorf("values.yaml %v = %v, want %v", tc.path, got, tc.want)
		}
	}
}

func TestAppCreateListsMissingRequiredFlags(t *testing.T) {
	url := newPlatformRepository(t, testPlatformYAML)
	for _, tc := range []struct {
		name    string
		args    []string
		missing []string
	}{
		{"no name", []string{"--kind", "web-service"}, []string{`"name" not set`}},
		{"no kind", []string{"--name", "shop"}, []string{`"kind" not set`}},
		{"nothing", nil, []string{`"kind"`, `"name"`, "not set"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, stderr, code := createApplication(t, url, cli.Dependencies{}, tc.args...)
			if code == 0 {
				t.Fatalf("exit code = 0, want non-zero")
			}
			for _, want := range tc.missing {
				if !strings.Contains(stderr, want) {
					t.Errorf("stderr = %q, want it to contain %q", stderr, want)
				}
			}
		})
	}
	assertNoApplications(t, url)
}

// assertNoApplications checks that the Platform repository still holds only
// what it was seeded with: nothing was written before the refusal.
func assertNoApplications(t *testing.T, url string) {
	t.Helper()
	if got := headSubject(t, url); got != "Seed the Platform repository" {
		t.Errorf("Platform repository has a new commit %q, want none", got)
	}
	clone := cloneMain(t, url)
	entries, err := os.ReadDir(filepath.Join(clone, "applications"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != ".gitkeep" {
			t.Errorf("applications/ contains %q, want nothing", e.Name())
		}
	}
}

func TestAppCreateRefusesInvalidNamesBeforeWriting(t *testing.T) {
	url := newPlatformRepository(t, testPlatformYAML)
	for _, name := range []string{
		"Shop",
		"1shop",
		"shop-",
		"shop_api",
		"shop.api",
		"a-name-that-is-longer-than-forty-characters-ok",
		"",
	} {
		t.Run(name, func(t *testing.T) {
			_, stderr, code := createApplication(t, url, cli.Dependencies{}, "--name", name, "--kind", "web-service")
			if code == 0 {
				t.Fatalf("exit code = 0, want non-zero")
			}
			if !strings.Contains(stderr, "name") {
				t.Errorf("stderr = %q, want it to explain the name", stderr)
			}
		})
	}
	assertNoApplications(t, url)
}

func TestAppCreateAcceptsFortyCharacterName(t *testing.T) {
	url := newPlatformRepository(t, testPlatformYAML)
	name := "a-name-that-is-exactly-forty-characters1"
	if len(name) != 40 {
		t.Fatalf("test name is %d characters", len(name))
	}

	_, stderr, code := createApplication(t, url, cli.Dependencies{}, "--name", name, "--kind", "web-service")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr: %s", code, stderr)
	}
}

func TestAppCreateRefusesKindsAndSizesTheChartDoesNotRender(t *testing.T) {
	url := newPlatformRepository(t, testPlatformYAML)
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"unknown kind", []string{"--name", "shop", "--kind", "cron-job"}, "cron-job"},
		{"unknown size", []string{"--name", "shop", "--kind", "web-service", "--size", "huge"}, "huge"},
		{"bad port", []string{"--name", "shop", "--kind", "web-service", "--port", "0"}, "port"},
		{"bad probe path", []string{"--name", "shop", "--kind", "web-service", "--probe-path", "healthz"}, "probe-path"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, stderr, code := createApplication(t, url, cli.Dependencies{}, tc.args...)
			if code == 0 {
				t.Fatalf("exit code = 0, want non-zero")
			}
			if !strings.Contains(stderr, tc.want) {
				t.Errorf("stderr = %q, want it to contain %q", stderr, tc.want)
			}
		})
	}
	assertNoApplications(t, url)
}

func TestAppCreateAcceptsStaticSiteKind(t *testing.T) {
	url := newPlatformRepository(t, testPlatformYAML)

	_, stderr, code := createApplication(t, url, cli.Dependencies{}, "--name", "shop", "--kind", "static-site")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr: %s", code, stderr)
	}
	values := readYAML(t, filepath.Join(cloneMain(t, url), "applications/shop/prod/values.yaml"))
	if got := lookup(t, values, "kind"); got != "static-site" {
		t.Errorf("values.yaml kind = %v, want static-site", got)
	}
}

// TestAppCreateRefusesAnExistingApplication covers a live Environment: a
// prod/ (or staging/) with its own application.yaml, the file ArgoCD's own
// bootstrap/applications.yaml glob and checkApplicationAbsent both treat as
// "this Environment exists". A directory with values.yaml but no
// application.yaml is the different, leftover case
// TestAppCreateAfterDeleteReusesTheNameAndClearsTheLeftover covers.
func TestAppCreateRefusesAnExistingApplication(t *testing.T) {
	url := newPlatformRepository(t, testPlatformYAML)
	pushCommit(t, url, "Add shop by hand", map[string]string{
		"applications/shop/prod/application.yaml": "apiVersion: argoproj.io/v1alpha1\nkind: Application\nmetadata:\n  name: shop\n",
		"applications/shop/prod/values.yaml":      "application:\n  name: shop\n",
	})

	_, stderr, code := createApplication(t, url, cli.Dependencies{}, "--name", "shop", "--kind", "web-service")

	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero")
	}
	if !strings.Contains(stderr, "shop") || !strings.Contains(stderr, "already") {
		t.Errorf("stderr = %q, want it to say shop already exists", stderr)
	}
	if got := headSubject(t, url); got != "Add shop by hand" {
		t.Errorf("Platform repository head is %q, want the hand-made commit untouched", got)
	}
}

func TestAppCreateRequiresAGitHubLogin(t *testing.T) {
	url := newPlatformRepository(t, testPlatformYAML)
	deps := cli.Dependencies{TokenSource: fakeTokenSource{err: errors.New("gh auth token: not logged in")}}

	_, stderr, code := createApplication(t, url, deps, "--name", "shop", "--kind", "web-service")

	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero")
	}
	if !strings.Contains(stderr, platform.Repository) || !strings.Contains(stderr, "not logged in") {
		t.Errorf("stderr = %q, want it to name the Platform repository and the missing login", stderr)
	}
	assertNoApplications(t, url)
}

func TestAppCreateExplainsARefusedPush(t *testing.T) {
	url := newPlatformRepository(t, testPlatformYAML)
	refusePushes(t, url, "Permission to iidp-platform denied to developer.")

	_, stderr, code := createApplication(t, url, cli.Dependencies{}, "--name", "shop", "--kind", "web-service")

	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero")
	}
	for _, want := range []string{platform.Repository, "write access", "Permission to iidp-platform denied"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr = %q, want it to contain %q", stderr, want)
		}
	}
	assertNoApplications(t, url)
}

func TestAppCreateRetriesOnceWhenMainMoved(t *testing.T) {
	url := newPlatformRepository(t, testPlatformYAML)
	pushes := 0
	deps := cli.Dependencies{BeforePush: func() error {
		pushes++
		if pushes == 1 {
			// Someone else lands an unrelated hand edit while the CLI runs.
			pushCommit(t, url, "Hand-edit platform.yaml", map[string]string{
				"platform.yaml": testPlatformYAML + "handEdited: true\n",
				"README.md":     "edited by hand\n",
			})
		}
		return nil
	}}

	stdout, stderr, code := createApplication(t, url, deps, "--name", "shop", "--kind", "web-service")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if pushes != 2 {
		t.Errorf("push attempts = %d, want 2", pushes)
	}
	clone := cloneMain(t, url)
	subjects := strings.Split(strings.TrimSpace(gitRun(t, clone, "log", "--format=%s")), "\n")
	want := []string{"iidp app create shop", "Hand-edit platform.yaml", "Seed the Platform repository"}
	if strings.Join(subjects, "|") != strings.Join(want, "|") {
		t.Errorf("history = %v, want %v", subjects, want)
	}
	if _, err := os.Stat(filepath.Join(clone, "applications/shop/prod/application.yaml")); err != nil {
		t.Errorf("application.yaml missing after retry: %v", err)
	}
	if _, err := os.Stat(filepath.Join(clone, "README.md")); err != nil {
		t.Errorf("the hand edit was lost: %v", err)
	}
}

func TestAppCreateFailsWhenTheApplicationAppearedWhileRunning(t *testing.T) {
	url := newPlatformRepository(t, testPlatformYAML)
	pushes := 0
	deps := cli.Dependencies{BeforePush: func() error {
		pushes++
		if pushes == 1 {
			// A live Environment, application.yaml included -- the same
			// files a real, concurrently-successful iidp app create shop
			// would have pushed. Seeding values.yaml alone would look like
			// a leftover from iidp app delete instead, which the retry is
			// meant to reuse, not refuse.
			pushCommit(t, url, "iidp app create shop", map[string]string{
				"applications/shop/prod/application.yaml": "apiVersion: argoproj.io/v1alpha1\nkind: Application\nmetadata:\n  name: shop\n",
				"applications/shop/prod/values.yaml":      "application:\n  name: shop\n",
			})
		}
		return nil
	}}

	_, stderr, code := createApplication(t, url, deps, "--name", "shop", "--kind", "web-service")

	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero")
	}
	if pushes != 1 {
		t.Errorf("push attempts = %d, want 1: the retry must stop before pushing", pushes)
	}
	if !strings.Contains(stderr, "shop") || !strings.Contains(stderr, "while this command ran") {
		t.Errorf("stderr = %q, want it to explain the conflict", stderr)
	}
	clone := cloneMain(t, url)
	if n := strings.Count(gitRun(t, clone, "log", "--format=%s"), "iidp app create shop"); n != 1 {
		t.Errorf("found %d create commits, want only the other developer's", n)
	}
}

func TestAppCreateHonoursChartRepositoryFromPlatformYAML(t *testing.T) {
	url := newPlatformRepository(t, testPlatformYAML+"chartRepository: oci://registry.example.com/itema/charts/application\n")

	_, stderr, code := createApplication(t, url, cli.Dependencies{}, "--name", "shop", "--kind", "web-service")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr: %s", code, stderr)
	}
	app := readYAML(t, filepath.Join(cloneMain(t, url), "applications/shop/prod/application.yaml"))
	if got := lookup(t, app, "spec", "sources", 0, "repoURL"); got != "registry.example.com/itema/charts" {
		t.Errorf("chart repoURL = %v", got)
	}
	if got := lookup(t, app, "spec", "sources", 0, "chart"); got != "application" {
		t.Errorf("chart name = %v", got)
	}
}

func TestAppCreateRefusesAPlatformYAMLWithoutChartVersion(t *testing.T) {
	url := newPlatformRepository(t, "baseDomain: app.itma.no\n")

	_, stderr, code := createApplication(t, url, cli.Dependencies{}, "--name", "shop", "--kind", "web-service")

	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero")
	}
	if !strings.Contains(stderr, "chartVersion") {
		t.Errorf("stderr = %q, want it to name chartVersion", stderr)
	}
	assertNoApplications(t, url)
}
