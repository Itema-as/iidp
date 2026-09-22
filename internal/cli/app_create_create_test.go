package cli_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Itema-as/iidp/internal/cli"
	"github.com/Itema-as/iidp/internal/platform"
)

// fakeGitHub is an in-process fake of the few GitHub REST endpoints the
// Create path uses. It records every request and, on repository creation,
// makes a real local bare repository and returns its file:// URL as
// clone_url, so the pushed template can be inspected by cloning it, the
// way docs/design.md's testing seam asks for.
type fakeGitHub struct {
	srv   *httptest.Server
	t     *testing.T
	login string

	mu                  sync.Mutex
	requests            []fakeRequest
	existing            map[string]bool
	created             map[string]string
	defaults            map[string]string
	reposDir            string
	createDefaultBranch string

	// The following support the Adopt path's endpoints
	// (docs/implementation-notes/15-cli-adopt-path.md): reading an existing
	// repository's default branch and push permission, checking whether a
	// branch already exists (against the real bare repository, so a push
	// the CLI makes is genuinely observable afterwards), and opening a
	// pull request.
	repoDefaultBranch map[string]string // owner/name -> default branch
	canPush           map[string]bool   // owner/name -> permissions.push
	bareDir           map[string]string // owner/name -> the bare repository's filesystem path
	pulls             []fakePullRequest
	nextPRNumber      int
}

type fakeRequest struct {
	Method string
	Path   string
	Body   map[string]any
}

// fakePullRequest is one POST /repos/{owner}/{name}/pulls the fake
// recorded, for asserting on its base, head, title and body.
type fakePullRequest struct {
	Owner, Name string
	Number      int
	Title       string
	Head        string
	Base        string
	Body        string
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()
	setGitEnv(t)
	f := &fakeGitHub{
		t:                   t,
		login:               "devuser",
		existing:            map[string]bool{},
		created:             map[string]string{},
		defaults:            map[string]string{},
		reposDir:            t.TempDir(),
		createDefaultBranch: "master",
		repoDefaultBranch:   map[string]string{},
		canPush:             map[string]bool{},
		bareDir:             map[string]string{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/user", f.handleUser)
	mux.HandleFunc("/orgs/", f.handleOrgRepos)
	mux.HandleFunc("/user/repos", f.handleUserRepos)
	mux.HandleFunc("/repos/", f.handleRepo)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// seedAdoptRepository seeds a real bare Application repository for
// owner/name on defaultBranch with files, registers it with the fake as
// existing with push access, and returns its file:// clone URL and bare
// directory (for pushExistingBranch, to simulate a pre-existing AdoptBranch).
func (f *fakeGitHub) seedAdoptRepository(t *testing.T, owner, name, defaultBranch string, files map[string]string) (cloneURL, bareDir string) {
	t.Helper()
	bare, url := seedApplicationRepository(t, defaultBranch, files)
	key := owner + "/" + name
	f.mu.Lock()
	f.existing[key] = true
	f.created[key] = url
	f.bareDir[key] = bare
	f.repoDefaultBranch[key] = defaultBranch
	f.canPush[key] = true
	f.mu.Unlock()
	return url, bare
}

// denyPush makes owner/name (already seeded) report permissions.push:
// false, the way a repository the developer cannot push to does.
func (f *fakeGitHub) denyPush(owner, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.canPush[owner+"/"+name] = false
}

// pullRequestsTo returns the pull requests the fake recorded for
// owner/name.
func (f *fakeGitHub) pullRequestsTo(owner, name string) []fakePullRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []fakePullRequest
	for _, pr := range f.pulls {
		if pr.Owner == owner && pr.Name == name {
			out = append(out, pr)
		}
	}
	return out
}

// markExisting makes owner/name answer 200 to GET /repos/owner/name, the
// way a repository that already exists does.
func (f *fakeGitHub) markExisting(owner, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.existing[owner+"/"+name] = true
	f.canPush[owner+"/"+name] = true
	f.repoDefaultBranch[owner+"/"+name] = "main"
}

// requestsTo reports how many recorded requests match method and a path
// suffix.
func (f *fakeGitHub) requestsTo(method, pathSuffix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.requests {
		if r.Method == method && strings.HasSuffix(r.Path, pathSuffix) {
			n++
		}
	}
	return n
}

// createBody returns the JSON body of the POST that created owner/name,
// or nil if none was made.
func (f *fakeGitHub) createBody(owner, name string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.requests {
		if r.Method != http.MethodPost {
			continue
		}
		if body, _ := r.Body["name"].(string); body != name {
			continue
		}
		if strings.Contains(r.Path, owner) || r.Path == "/user/repos" {
			return r.Body
		}
	}
	return nil
}

// cloneURL returns the clone_url the fake handed back for owner/name.
func (f *fakeGitHub) cloneURL(owner, name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.created[owner+"/"+name]
}

// defaultBranch returns the branch a PATCH set as owner/name's default, or
// "" if none was made.
func (f *fakeGitHub) defaultBranch(owner, name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.defaults[owner+"/"+name]
}

func (f *fakeGitHub) record(r *http.Request) map[string]any {
	var body map[string]any
	if r.Body != nil {
		data, _ := io.ReadAll(r.Body)
		if len(data) > 0 {
			_ = json.Unmarshal(data, &body)
		}
	}
	f.mu.Lock()
	f.requests = append(f.requests, fakeRequest{Method: r.Method, Path: r.URL.Path, Body: body})
	f.mu.Unlock()
	return body
}

func (f *fakeGitHub) handleUser(w http.ResponseWriter, r *http.Request) {
	f.record(r)
	writeJSON(w, http.StatusOK, map[string]any{"login": f.login})
}

func (f *fakeGitHub) handleRepo(w http.ResponseWriter, r *http.Request) {
	body := f.record(r)
	rest := strings.TrimPrefix(r.URL.Path, "/repos/")

	if r.Method == http.MethodPost && strings.HasSuffix(rest, "/pulls") {
		owner, name := splitOwnerName(strings.TrimSuffix(rest, "/pulls"))
		f.createPullRequest(w, owner, name, body)
		return
	}
	if idx := strings.Index(rest, "/branches/"); idx >= 0 {
		owner, name := splitOwnerName(rest[:idx])
		branch := rest[idx+len("/branches/"):]
		f.handleBranchExists(w, owner, name, branch)
		return
	}

	parts := strings.SplitN(rest, "/", 2)
	if len(parts) != 2 {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	key := parts[0] + "/" + parts[1]
	switch r.Method {
	case http.MethodGet:
		f.mu.Lock()
		exists := f.existing[key]
		resp := map[string]any{
			"full_name":      key,
			"default_branch": f.repoDefaultBranch[key],
			"clone_url":      f.created[key],
			"permissions":    map[string]any{"push": f.canPush[key]},
		}
		f.mu.Unlock()
		if !exists {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, resp)
	case http.MethodPatch:
		branch, _ := body["default_branch"].(string)
		f.mu.Lock()
		f.defaults[key] = branch
		f.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"full_name": key, "default_branch": branch})
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// splitOwnerName splits "owner/name" into its two parts.
func splitOwnerName(s string) (owner, name string) {
	parts := strings.SplitN(s, "/", 2)
	if len(parts) != 2 {
		return s, ""
	}
	return parts[0], parts[1]
}

// handleBranchExists answers GET /repos/{owner}/{name}/branches/{branch}
// by checking the real bare repository the fake seeded or created for
// owner/name: a genuine reflection of whatever the CLI has actually pushed,
// rather than a separately tracked flag.
func (f *fakeGitHub) handleBranchExists(w http.ResponseWriter, owner, name, branch string) {
	f.mu.Lock()
	bareDir := f.bareDir[owner+"/"+name]
	f.mu.Unlock()
	if bareDir == "" {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	cmd := exec.Command("git", "--git-dir", bareDir, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	if err := cmd.Run(); err != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": branch})
}

// createPullRequest answers POST /repos/{owner}/{name}/pulls.
func (f *fakeGitHub) createPullRequest(w http.ResponseWriter, owner, name string, body map[string]any) {
	title, _ := body["title"].(string)
	head, _ := body["head"].(string)
	base, _ := body["base"].(string)
	prBody, _ := body["body"].(string)

	f.mu.Lock()
	f.nextPRNumber++
	n := f.nextPRNumber
	f.pulls = append(f.pulls, fakePullRequest{Owner: owner, Name: name, Number: n, Title: title, Head: head, Base: base, Body: prBody})
	f.mu.Unlock()

	url := fmt.Sprintf("https://github.com/%s/%s/pull/%d", owner, name, n)
	writeJSON(w, http.StatusCreated, map[string]any{"html_url": url, "number": n})
}

func (f *fakeGitHub) handleOrgRepos(w http.ResponseWriter, r *http.Request) {
	body := f.record(r)
	if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/repos") {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	org := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/orgs/"), "/repos")
	f.createRepo(w, org, body)
}

func (f *fakeGitHub) handleUserRepos(w http.ResponseWriter, r *http.Request) {
	body := f.record(r)
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	f.createRepo(w, f.login, body)
}

func (f *fakeGitHub) createRepo(w http.ResponseWriter, owner string, body map[string]any) {
	name, _ := body["name"].(string)
	key := owner + "/" + name
	bare := filepath.Join(f.reposDir, strings.ReplaceAll(key, "/", "_")+".git")
	cmd := exec.Command("git", "init", "--quiet", "--bare", "--initial-branch=main", bare)
	if err := cmd.Run(); err != nil {
		f.t.Fatalf("fake GitHub: init bare repo for %s: %v", key, err)
	}
	cloneURL := "file://" + bare
	f.mu.Lock()
	f.created[key] = cloneURL
	f.existing[key] = true
	f.bareDir[key] = bare
	f.repoDefaultBranch[key] = f.createDefaultBranch
	f.canPush[key] = true
	f.mu.Unlock()
	writeJSON(w, http.StatusCreated, map[string]any{
		"clone_url":      cloneURL,
		"full_name":      key,
		"default_branch": f.createDefaultBranch,
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// cloneAppRepo clones the Application repository the fake created for
// owner/name into a fresh directory and returns it.
func cloneAppRepo(t *testing.T, f *fakeGitHub, owner, name string) string {
	t.Helper()
	url := f.cloneURL(owner, name)
	if url == "" {
		t.Fatalf("fake GitHub never created %s/%s", owner, name)
	}
	return cloneMain(t, url)
}

func TestAppCreatePathNextJSUnderOrgIsAWebService(t *testing.T) {
	platformURL := newPlatformRepository(t, testPlatformYAML)
	gh := newFakeGitHub(t)

	stdout, stderr, code := createApplication(t, platformURL, cli.Dependencies{GitHubAPI: gh.srv.URL},
		"--name", "shop", "--path", "create", "--framework", "nextjs")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}

	body := gh.createBody(platform.Org, "shop")
	if body == nil {
		t.Fatalf("no repository creation request recorded")
	}
	if got := body["private"]; got != true {
		t.Errorf("create body private = %v, want true (default)", got)
	}
	if got := body["auto_init"]; got != false {
		t.Errorf("create body auto_init = %v, want false", got)
	}
	if n := gh.requestsTo(http.MethodPost, "/orgs/"+platform.Org+"/repos"); n != 1 {
		t.Errorf("POST /orgs/%s/repos called %d times, want 1", platform.Org, n)
	}
	if n := gh.requestsTo(http.MethodPost, "/user/repos"); n != 0 {
		t.Errorf("POST /user/repos called %d times, want 0 (org owner)", n)
	}
	if got := gh.defaultBranch(platform.Org, "shop"); got != "main" {
		t.Errorf("default branch set to %q, want main", got)
	}

	clone := cloneAppRepo(t, gh, platform.Org, "shop")
	assertFileExists(t, filepath.Join(clone, "Dockerfile"))
	assertFileExists(t, filepath.Join(clone, ".gitignore"))
	assertFileExists(t, filepath.Join(clone, ".dockerignore"))
	pkg := readYAML(t, filepath.Join(clone, "package.json"))
	if got := lookup(t, pkg, "name"); got != "shop" {
		t.Errorf("package.json name = %v, want shop", got)
	}
	if got := strings.TrimSpace(gitRun(t, clone, "log", "-1", "--format=%s")); got != "Initial commit from iidp" {
		t.Errorf("commit subject = %q", got)
	}

	values := readYAML(t, filepath.Join(cloneMain(t, platformURL), "applications/shop/prod/values.yaml"))
	if got := lookup(t, values, "kind"); got != "web-service" {
		t.Errorf("values.yaml kind = %v, want web-service", got)
	}
	if got := lookup(t, values, "image", "repository"); got != platform.Registry+"/shop" {
		t.Errorf("values.yaml image.repository = %v, want %s", got, platform.Registry+"/shop")
	}

	for _, want := range []string{
		"https://github.com/" + platform.Org + "/shop",
		"https://shop.app.itma.no",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout)
		}
	}
}

func TestAppCreatePathSkipsSettingTheDefaultBranchWhenAlreadyMain(t *testing.T) {
	platformURL := newPlatformRepository(t, testPlatformYAML)
	gh := newFakeGitHub(t)
	gh.createDefaultBranch = "main"

	_, stderr, code := createApplication(t, platformURL, cli.Dependencies{GitHubAPI: gh.srv.URL},
		"--name", "shop", "--path", "create", "--framework", "nextjs")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr: %s", code, stderr)
	}
	if n := gh.requestsTo(http.MethodPatch, "/repos/"+platform.Org+"/shop"); n != 0 {
		t.Errorf("PATCH /repos/%s/shop called %d times, want 0: GitHub already reported main as the default", platform.Org, n)
	}
}

func TestAppCreatePathViteReactIsAStaticSite(t *testing.T) {
	platformURL := newPlatformRepository(t, testPlatformYAML)
	gh := newFakeGitHub(t)

	_, stderr, code := createApplication(t, platformURL, cli.Dependencies{GitHubAPI: gh.srv.URL},
		"--name", "shop", "--path", "create", "--framework", "vite-react")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr: %s", code, stderr)
	}
	clone := cloneAppRepo(t, gh, platform.Org, "shop")
	assertFileExists(t, filepath.Join(clone, "Dockerfile"))
	assertFileExists(t, filepath.Join(clone, ".gitignore"))
	assertFileExists(t, filepath.Join(clone, ".dockerignore"))
	assertFileExists(t, filepath.Join(clone, "index.html"))
	pkg := readYAML(t, filepath.Join(clone, "package.json"))
	if got := lookup(t, pkg, "name"); got != "shop" {
		t.Errorf("package.json name = %v, want shop", got)
	}

	values := readYAML(t, filepath.Join(cloneMain(t, platformURL), "applications/shop/prod/values.yaml"))
	if got := lookup(t, values, "kind"); got != "static-site" {
		t.Errorf("values.yaml kind = %v, want static-site", got)
	}
}

func TestAppCreatePathOtherProducesOnlyADockerfileStub(t *testing.T) {
	platformURL := newPlatformRepository(t, testPlatformYAML)
	gh := newFakeGitHub(t)

	_, stderr, code := createApplication(t, platformURL, cli.Dependencies{GitHubAPI: gh.srv.URL},
		"--name", "shop", "--path", "create", "--framework", "other", "--kind", "web-service")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr: %s", code, stderr)
	}
	clone := cloneAppRepo(t, gh, platform.Org, "shop")
	assertFileExists(t, filepath.Join(clone, "Dockerfile"))
	if _, err := os.Stat(filepath.Join(clone, "package.json")); err == nil {
		t.Errorf("package.json exists, want no framework files for Other")
	}
	dockerfile, err := os.ReadFile(filepath.Join(clone, "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(dockerfile), "PORT") {
		t.Errorf("Dockerfile stub does not mention PORT:\n%s", dockerfile)
	}

	values := readYAML(t, filepath.Join(cloneMain(t, platformURL), "applications/shop/prod/values.yaml"))
	if got := lookup(t, values, "kind"); got != "web-service" {
		t.Errorf("values.yaml kind = %v, want web-service", got)
	}
}

func TestAppCreatePathOtherRequiresKind(t *testing.T) {
	platformURL := newPlatformRepository(t, testPlatformYAML)
	gh := newFakeGitHub(t)

	_, stderr, code := createApplication(t, platformURL, cli.Dependencies{GitHubAPI: gh.srv.URL},
		"--name", "shop", "--path", "create", "--framework", "other")

	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero")
	}
	if !strings.Contains(stderr, "kind") {
		t.Errorf("stderr = %q, want it to mention kind", stderr)
	}
	if n := gh.requestsTo(http.MethodPost, "/repos"); n != 0 {
		t.Errorf("a repository was created despite the missing --kind")
	}
}

func TestAppCreatePathFrameworkDerivesKindAndRefusesExplicitKind(t *testing.T) {
	platformURL := newPlatformRepository(t, testPlatformYAML)
	gh := newFakeGitHub(t)

	_, stderr, code := createApplication(t, platformURL, cli.Dependencies{GitHubAPI: gh.srv.URL},
		"--name", "shop", "--path", "create", "--framework", "nextjs", "--kind", "static-site")

	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero")
	}
	if !strings.Contains(stderr, "derived") {
		t.Errorf("stderr = %q, want it to explain the Kind is derived", stderr)
	}
}

func TestAppCreatePathPersonalOwnerUsesTheDevelopersLogin(t *testing.T) {
	platformURL := newPlatformRepository(t, testPlatformYAML)
	gh := newFakeGitHub(t)
	gh.login = "developer42"

	stdout, stderr, code := createApplication(t, platformURL, cli.Dependencies{GitHubAPI: gh.srv.URL},
		"--name", "shop", "--path", "create", "--framework", "nextjs", "--owner", "user")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr: %s", code, stderr)
	}
	if n := gh.requestsTo(http.MethodGet, "/user"); n != 1 {
		t.Errorf("GET /user called %d times, want 1", n)
	}
	if n := gh.requestsTo(http.MethodPost, "/user/repos"); n != 1 {
		t.Errorf("POST /user/repos called %d times, want 1", n)
	}
	if n := gh.requestsTo(http.MethodPost, "/orgs/"+platform.Org+"/repos"); n != 0 {
		t.Errorf("POST /orgs/%s/repos called %d times, want 0 (personal owner)", platform.Org, n)
	}
	if !strings.Contains(stdout, "https://github.com/developer42/shop") {
		t.Errorf("stdout lacks the Application repository URL:\n%s", stdout)
	}
	values := readYAML(t, filepath.Join(cloneMain(t, platformURL), "applications/shop/prod/values.yaml"))
	if got := lookup(t, values, "image", "repository"); got != "ghcr.io/developer42/shop" {
		t.Errorf("values.yaml image.repository = %v, want ghcr.io/developer42/shop", got)
	}
}

func TestAppCreatePathDefaultsToPrivateAndHonoursPublic(t *testing.T) {
	platformURL := newPlatformRepository(t, testPlatformYAML)

	t.Run("default private", func(t *testing.T) {
		gh := newFakeGitHub(t)
		_, stderr, code := createApplication(t, platformURL, cli.Dependencies{GitHubAPI: gh.srv.URL},
			"--name", "priv", "--path", "create", "--framework", "nextjs")
		if code != 0 {
			t.Fatalf("exit code = %d, want 0\nstderr: %s", code, stderr)
		}
		if got := gh.createBody(platform.Org, "priv")["private"]; got != true {
			t.Errorf("private = %v, want true", got)
		}
	})

	t.Run("--public", func(t *testing.T) {
		gh := newFakeGitHub(t)
		_, stderr, code := createApplication(t, platformURL, cli.Dependencies{GitHubAPI: gh.srv.URL},
			"--name", "pub", "--path", "create", "--framework", "nextjs", "--public")
		if code != 0 {
			t.Fatalf("exit code = %d, want 0\nstderr: %s", code, stderr)
		}
		if got := gh.createBody(platform.Org, "pub")["private"]; got != false {
			t.Errorf("private = %v, want false", got)
		}
	})

	t.Run("--private and --public conflict", func(t *testing.T) {
		gh := newFakeGitHub(t)
		_, stderr, code := createApplication(t, platformURL, cli.Dependencies{GitHubAPI: gh.srv.URL},
			"--name", "conflict", "--path", "create", "--framework", "nextjs", "--private", "--public")
		if code == 0 {
			t.Fatalf("exit code = 0, want non-zero")
		}
		if !strings.Contains(stderr, "mutually exclusive") {
			t.Errorf("stderr = %q, want it to explain the conflict", stderr)
		}
	})
}

func TestAppCreatePathRefusesWhenTheRepositoryAlreadyExists(t *testing.T) {
	platformURL := newPlatformRepository(t, testPlatformYAML)
	gh := newFakeGitHub(t)
	gh.markExisting(platform.Org, "shop")

	_, stderr, code := createApplication(t, platformURL, cli.Dependencies{GitHubAPI: gh.srv.URL},
		"--name", "shop", "--path", "create", "--framework", "nextjs")

	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero")
	}
	if !strings.Contains(stderr, platform.Org+"/shop") || !strings.Contains(stderr, "already exists") {
		t.Errorf("stderr = %q, want it to say the repository already exists", stderr)
	}
	if n := gh.requestsTo(http.MethodPost, "/repos"); n != 0 {
		t.Errorf("a repository creation request was made despite the refusal")
	}
	assertNoApplications(t, platformURL)
}

func TestAppCreatePathValidatesThePlatformRepositoryBeforeCreatingAnything(t *testing.T) {
	platformURL := newPlatformRepository(t, testPlatformYAML)
	// A live Environment (application.yaml included), not a leftover from
	// iidp app delete: only a live Environment refuses iidp app create.
	pushCommit(t, platformURL, "Add shop by hand", map[string]string{
		"applications/shop/prod/application.yaml": "apiVersion: argoproj.io/v1alpha1\nkind: Application\nmetadata:\n  name: shop\n",
		"applications/shop/prod/values.yaml":      "application:\n  name: shop\n",
	})
	gh := newFakeGitHub(t)

	_, stderr, code := createApplication(t, platformURL, cli.Dependencies{GitHubAPI: gh.srv.URL},
		"--name", "shop", "--path", "create", "--framework", "nextjs")

	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero")
	}
	if !strings.Contains(stderr, "shop") || !strings.Contains(stderr, "already") {
		t.Errorf("stderr = %q, want it to say shop already exists", stderr)
	}
	if n := gh.requestsTo(http.MethodPost, "/orgs/"+platform.Org+"/repos"); n != 0 {
		t.Errorf("the Application repository was created despite the Platform repository already having shop")
	}
}

func TestAppCreatePathReportsWhatWasCreatedWhenThePlatformPushFails(t *testing.T) {
	platformURL := newPlatformRepository(t, testPlatformYAML)
	refusePushes(t, platformURL, "Permission to iidp-platform denied to developer.")
	gh := newFakeGitHub(t)

	stdout, stderr, code := createApplication(t, platformURL, cli.Dependencies{GitHubAPI: gh.srv.URL},
		"--name", "shop", "--path", "create", "--framework", "nextjs")

	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero")
	}
	if gh.cloneURL(platform.Org, "shop") == "" {
		t.Fatalf("the Application repository was not created")
	}
	if !strings.Contains(stderr, "write access") {
		t.Errorf("stderr = %q, want it to explain the Platform repository push was refused", stderr)
	}
	for _, want := range []string{
		"https://github.com/" + platform.Org + "/shop",
		"created and pushed",
		"Finish by hand",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout = %q, want it to contain %q", stdout, want)
		}
	}
	// The Application repository itself is untouched: it holds only the
	// pushed template.
	clone := cloneAppRepo(t, gh, platform.Org, "shop")
	assertFileExists(t, filepath.Join(clone, "Dockerfile"))
}

// The Adopt path itself (--path adopt --repo ...) is covered end to end in
// app_create_adopt_test.go; it is implemented, not refused, as of #15.

func TestAppCreatePathFrameworkRequiresPathCreate(t *testing.T) {
	platformURL := newPlatformRepository(t, testPlatformYAML)

	_, stderr, code := createApplication(t, platformURL, cli.Dependencies{},
		"--name", "shop", "--kind", "web-service", "--framework", "nextjs")

	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero")
	}
	if !strings.Contains(stderr, "--path create") {
		t.Errorf("stderr = %q, want it to say --framework requires --path create", stderr)
	}
}

func TestAppCreatePathRequiresLogin(t *testing.T) {
	platformURL := newPlatformRepository(t, testPlatformYAML)
	gh := newFakeGitHub(t)
	deps := cli.Dependencies{
		GitHubAPI:   gh.srv.URL,
		TokenSource: fakeTokenSource{err: fmt.Errorf("gh auth token: not logged in")},
	}

	_, stderr, code := createApplication(t, platformURL, deps, "--name", "shop", "--path", "create", "--framework", "nextjs")

	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero")
	}
	if !strings.Contains(stderr, "not logged in") {
		t.Errorf("stderr = %q, want it to explain the missing login", stderr)
	}
	if len(gh.requests) != 0 {
		t.Errorf("the fake GitHub API was called %d times, want 0", len(gh.requests))
	}
}

func assertFileExists(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Errorf("%s: %v", path, err)
	}
}

func TestAppCreatePathAddsTheDeployWorkflow(t *testing.T) {
	platformURL := newPlatformRepository(t, testPlatformYAML)
	gh := newFakeGitHub(t)

	stdout, stderr, code := createApplication(t, platformURL, cli.Dependencies{GitHubAPI: gh.srv.URL},
		"--name", "shop", "--path", "create", "--framework", "nextjs")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	clone := cloneAppRepo(t, gh, platform.Org, "shop")
	workflowPath := filepath.Join(clone, ".github", "workflows", "deploy.yaml")
	assertFileExists(t, workflowPath)

	data, err := os.ReadFile(workflowPath)
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	if !strings.Contains(content, "ghcr.io/"+strings.ToLower(platform.Org)+"/shop") {
		t.Errorf("deploy.yaml lacks the expected image reference:\n%s", content)
	}
	if !strings.Contains(content, "iidp ci set-image shop auto") {
		t.Errorf("deploy.yaml lacks the main-branch set-image invocation:\n%s", content)
	}
	if !strings.Contains(content, "iidp ci set-image shop prod") {
		t.Errorf("deploy.yaml lacks the tag-promotion set-image invocation:\n%s", content)
	}
	if !strings.Contains(stdout, "pinned to iidp latest") {
		t.Errorf("stdout lacks the dev-build iidp version note:\n%s", stdout)
	}
}

func TestAppCreatePathPersonalOwnerIsToldToAddTheSecretByHand(t *testing.T) {
	platformURL := newPlatformRepository(t, testPlatformYAML)
	gh := newFakeGitHub(t)
	gh.login = "developer42"

	stdout, stderr, code := createApplication(t, platformURL, cli.Dependencies{GitHubAPI: gh.srv.URL},
		"--name", "shop", "--path", "create", "--framework", "nextjs", "--owner", "user")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, "IIDP_DEPLOY_APP_PRIVATE_KEY") || !strings.Contains(stdout, "by hand") {
		t.Errorf("stdout lacks the personal-account secret note:\n%s", stdout)
	}
}

func TestAppCreatePathOrgOwnerGetsNoSecretNote(t *testing.T) {
	platformURL := newPlatformRepository(t, testPlatformYAML)
	gh := newFakeGitHub(t)

	stdout, stderr, code := createApplication(t, platformURL, cli.Dependencies{GitHubAPI: gh.srv.URL},
		"--name", "shop", "--path", "create", "--framework", "nextjs")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr: %s", code, stderr)
	}
	if strings.Contains(stdout, "by hand") {
		t.Errorf("stdout should not mention adding the secret by hand for an org owner:\n%s", stdout)
	}
}
