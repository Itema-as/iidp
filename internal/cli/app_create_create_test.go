package cli_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
	srv *httptest.Server
	t   *testing.T

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

	// oauthScopes is the X-OAuth-Scopes header every response carries,
	// the way GitHub reports a classic OAuth token's scopes
	// (docs/implementation-notes/47-workflow-scope.md); omitScopes drops
	// the header altogether, the way GitHub answers a fine-grained or
	// GitHub App token.
	oauthScopes string
	omitScopes  bool

	// repoIDs and ownerIDs are the numeric ids GitHub reports for a
	// repository and for its owner, assigned the first time the fake
	// learns of each (docs/implementation-notes/58-repository-binding.md).
	// transferredTo makes GET /repos/{owner}/{name} report a different
	// owner, the way GitHub follows the redirect of a repository that was
	// transferred away.
	repoIDs       map[string]int64
	ownerIDs      map[string]int64
	nextID        int64
	transferredTo map[string]string
}

// fakeOrgID is the numeric id the fake reports for platform.Org.
const fakeOrgID = 4242

// gh auth login's minimum scopes, plus workflow: the fake's default token.
const scopesWithWorkflow = "gist, read:org, repo, workflow"

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
		existing:            map[string]bool{},
		created:             map[string]string{},
		defaults:            map[string]string{},
		reposDir:            t.TempDir(),
		createDefaultBranch: "master",
		repoDefaultBranch:   map[string]string{},
		canPush:             map[string]bool{},
		bareDir:             map[string]string{},
		oauthScopes:         scopesWithWorkflow,
		repoIDs:             map[string]int64{},
		ownerIDs:            map[string]int64{platform.Org: fakeOrgID},
		nextID:              900001,
		transferredTo:       map[string]string{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/{$}", f.handleRoot)
	mux.HandleFunc("/orgs/", f.handleOrgRepos)
	mux.HandleFunc("/repos/", f.handleRepo)
	f.srv = httptest.NewServer(f.withScopes(mux))
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

// repoID returns the numeric id the fake reports for owner/name, assigning
// one the first time it is asked.
func (f *fakeGitHub) repoID(owner, name string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.repoIDLocked(owner + "/" + name)
}

func (f *fakeGitHub) repoIDLocked(key string) int64 {
	if id, ok := f.repoIDs[key]; ok {
		return id
	}
	f.nextID++
	f.repoIDs[key] = f.nextID
	return f.nextID
}

// ownerIDLocked returns the numeric id the fake reports for the account
// login: fakeOrgID for platform.Org, another stable id for anyone else.
func (f *fakeGitHub) ownerIDLocked(login string) int64 {
	if id, ok := f.ownerIDs[login]; ok {
		return id
	}
	f.nextID++
	f.ownerIDs[login] = f.nextID
	return f.nextID
}

// transferAway makes GET /repos/{owner}/{name} answer as if the repository
// had been transferred to newOwner: GitHub follows the redirect and
// reports the new owner and full name.
func (f *fakeGitHub) transferAway(owner, name, newOwner string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.transferredTo[owner+"/"+name] = newOwner
}

// rename makes owner/newName exist with owner/oldName's numeric id, the
// way GitHub reports a renamed repository.
func (f *fakeGitHub) rename(owner, oldName, newName string) {
	id := f.repoID(owner, oldName)
	f.markExisting(owner, newName)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.repoIDs[owner+"/"+newName] = id
}

// recreate gives owner/name a new numeric id, the way deleting a
// repository and creating another under the same name does.
func (f *fakeGitHub) recreate(owner, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.repoIDs, owner+"/"+name)
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
		if strings.Contains(r.Path, owner) {
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

// setScopes makes every response report scopes (a comma-separated list, as
// GitHub sends it) as the token's OAuth scopes.
func (f *fakeGitHub) setScopes(scopes string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.oauthScopes, f.omitScopes = scopes, false
}

// reportNoScopes makes every response omit X-OAuth-Scopes, the way GitHub
// answers a fine-grained personal access token or a GitHub App token.
func (f *fakeGitHub) reportNoScopes() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.omitScopes = true
}

// withScopes adds the X-OAuth-Scopes header to every response, as GitHub
// does for a classic token, unless reportNoScopes was called.
func (f *fakeGitHub) withScopes(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		scopes, omit := f.oauthScopes, f.omitScopes
		f.mu.Unlock()
		if !omit {
			w.Header()["X-Oauth-Scopes"] = []string{scopes}
		}
		next.ServeHTTP(w, r)
	})
}

// handleRoot answers GET /, the API root the CLI reads the token's scopes
// from.
func (f *fakeGitHub) handleRoot(w http.ResponseWriter, r *http.Request) {
	f.record(r)
	writeJSON(w, http.StatusOK, map[string]any{"current_user_url": f.srv.URL + "/user"})
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
		owner, fullName := parts[0], key
		if to, ok := f.transferredTo[key]; ok {
			owner, fullName = to, to+"/"+parts[1]
		}
		resp := map[string]any{
			"id":             f.repoIDLocked(key),
			"full_name":      fullName,
			"owner":          map[string]any{"login": owner, "id": f.ownerIDLocked(owner)},
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
	id, ownerID := f.repoIDLocked(key), f.ownerIDLocked(owner)
	f.mu.Unlock()
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":             id,
		"clone_url":      cloneURL,
		"full_name":      key,
		"owner":          map[string]any{"login": owner, "id": ownerID},
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

func TestAppCreatePathHasNoOwnerFlag(t *testing.T) {
	// Only repositories in the org can be Applications (ADR-0005), so
	// Create no longer offers a personal account: --owner is not a flag.
	platformURL := newPlatformRepository(t, testPlatformYAML)
	gh := newFakeGitHub(t)

	for _, owner := range []string{"user", "org"} {
		_, stderr, code := createApplication(t, platformURL, cli.Dependencies{GitHubAPI: gh.srv.URL},
			"--name", "shop", "--path", "create", "--framework", "nextjs", "--owner", owner)

		if code == 0 {
			t.Fatalf("--owner %s: exit code = 0, want non-zero", owner)
		}
		if !strings.Contains(stderr, "unknown flag: --owner") {
			t.Errorf("--owner %s: stderr = %q, want it to say --owner is unknown", owner, stderr)
		}
	}
	if len(gh.requests) != 0 {
		t.Errorf("the fake GitHub API was called %d times, want 0", len(gh.requests))
	}
	assertNoApplications(t, platformURL)

	var help bytes.Buffer
	cli.Run([]string{"app", "create", "--help"}, strings.NewReader(""), &help, &help)
	if strings.Contains(help.String(), "--owner") || strings.Contains(help.String(), "personal") {
		t.Errorf("app create --help still mentions --owner or a personal account:\n%s", help.String())
	}
}

func TestAppCreatePathBindsTheApplicationRepositoryByID(t *testing.T) {
	platformURL := newPlatformRepository(t, testPlatformYAML)
	gh := newFakeGitHub(t)

	stdout, stderr, code := createApplication(t, platformURL, cli.Dependencies{GitHubAPI: gh.srv.URL},
		"--name", "shop", "--path", "create", "--framework", "nextjs", "--staging")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	clone := cloneMain(t, platformURL)
	assertBinding(t, clone, "shop", platform.Org+"/shop", gh.repoID(platform.Org, "shop"), fakeOrgID)

	// In the same commit as the Environments, not a second one.
	added := strings.Fields(gitRun(t, clone, "show", "--name-only", "--format=", "HEAD"))
	if !slices.Contains(added, "applications/shop/repository.yaml") {
		t.Errorf("the create commit touched %v, want it to include applications/shop/repository.yaml", added)
	}
	if got := headSubject(t, platformURL); got != "iidp app create shop" {
		t.Errorf("head commit = %q, want iidp app create shop", got)
	}
	want := fmt.Sprintf("Repository: %s/shop (repository id %d, owner id %d)", platform.Org, gh.repoID(platform.Org, "shop"), fakeOrgID)
	if !strings.Contains(stdout, want) {
		t.Errorf("stdout lacks %q:\n%s", want, stdout)
	}
	if strings.Contains(stdout, "iidp app bind") {
		t.Errorf("stdout tells the developer to bind an Application Create already bound:\n%s", stdout)
	}
}

// assertBinding checks applications/<name>/repository.yaml in clone: the
// shape docs/platform-repository.md promises the Deploy gate.
func assertBinding(t *testing.T, clone, name, repository string, repositoryID, ownerID int64) {
	t.Helper()
	path := filepath.Join(clone, "applications", name, "repository.yaml")
	doc := readYAML(t, path)
	for key, want := range map[string]any{
		"repository":        repository,
		"repositoryId":      int(repositoryID),
		"repositoryOwnerId": int(ownerID),
	} {
		if got := doc[key]; got != want {
			t.Errorf("%s: %s = %v (%T), want %v", path, key, got, got, want)
		}
	}
	if len(doc) != 3 {
		t.Errorf("%s has keys %v, want exactly repository, repositoryId and repositoryOwnerId", path, doc)
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
		"iidp app bind shop --repo " + platform.Org + "/shop",
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
	// A short caller of the reusable deploy workflow at the major tag (v0
	// for this dev build), with the Application's name and the Deploy
	// gate's URL, rendered in from platform.yaml's baseDomain since CI
	// can't read the private Platform repository
	// (docs/implementation-notes/74-reusable-deploy-workflow.md).
	for _, want := range []string{
		"uses: " + platform.DeployWorkflow + "@v0\n",
		"application: shop\n",
		"deploy-gate-url: https://deploy.app.itma.no\n",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("deploy.yaml lacks %q:\n%s", want, content)
		}
	}
	if !strings.Contains(stdout, "calls "+platform.DeployWorkflow+"@v0") {
		t.Errorf("stdout doesn't say which reusable workflow deploy.yaml calls:\n%s", stdout)
	}
}

// The migration command lives in the Application repository's iidp.yaml,
// which the deploy workflow sends the Deploy gate with every deploy; app
// create writes it there, not into the Platform repository
// (docs/implementation-notes/66-migration-command-in-repo.md).
func TestAppCreatePathWritesTheMigrationCommandIntoIidpYAML(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"given", []string{"--postgres", "--migration-command", "npm run migrate"}, "\nmigrationCommand: npm run migrate\n"},
		{"without Postgres", nil, "\n# migrationCommand: npx prisma migrate deploy\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			platformURL := newPlatformRepository(t, testCapabilitiesPlatformYAML)
			gh := newFakeGitHub(t)

			stdout, stderr, code := createApplication(t, platformURL, cli.Dependencies{GitHubAPI: gh.srv.URL},
				append([]string{"--name", "shop", "--path", "create", "--framework", "nextjs"}, tc.args...)...)
			if code != 0 {
				t.Fatalf("exit code = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
			}

			appConfig := readFile(t, filepath.Join(cloneAppRepo(t, gh, platform.Org, "shop"), "iidp.yaml"))
			if !strings.Contains(appConfig, tc.want) || !strings.Contains(appConfig, "runs before every rollout") {
				t.Errorf("iidp.yaml lacks %q or its explanation:\n%s", tc.want, appConfig)
			}
			if !strings.Contains(stdout, "  iidp.yaml\n") {
				t.Errorf("stdout does not list iidp.yaml among the pushed files:\n%s", stdout)
			}
			values := readYAML(t, filepath.Join(cloneMain(t, platformURL), "applications/shop/prod/values.yaml"))
			if got := lookup(t, values, "postgres", "migrationCommand"); got != "" {
				t.Errorf("values.yaml postgres.migrationCommand = %v, want it left for the Deploy gate", got)
			}
		})
	}
}
