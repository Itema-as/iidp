package cli_test

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/Itema-as/iidp/internal/cli"
	"github.com/Itema-as/iidp/internal/platform"
)

// fakeGitHubApp is an in-process fake of the two GitHub App endpoints iidp
// ci set-image needs, in order: GET /app/installations (to discover the
// org's installation id) and POST /app/installations/{id}/access_tokens
// (to mint the token). Both are authenticated as the App itself with a
// JWT; the fake verifies its RS256 signature against the test public key
// and its iss claim against the expected app id
// (docs/implementation-notes/12-deploy-workflow.md).
type fakeGitHubApp struct {
	srv            *httptest.Server
	publicKey      *rsa.PublicKey
	appID          int64
	orgLogin       string
	installationID int64
	token          string

	mu                    sync.Mutex
	installationsRequests int
	tokenRequests         int
}

// newFakeGitHubApp starts the fake with one installation, for orgLogin
// with installationID, that hands back token.
func newFakeGitHubApp(t *testing.T, publicKey *rsa.PublicKey, appID int64, orgLogin string, installationID int64, token string) *fakeGitHubApp {
	t.Helper()
	f := &fakeGitHubApp{publicKey: publicKey, appID: appID, orgLogin: orgLogin, installationID: installationID, token: token}
	mux := http.NewServeMux()
	mux.HandleFunc("/app/installations", f.handleListInstallations)
	mux.HandleFunc("/app/installations/", f.handleCreateToken)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGitHubApp) handleListInstallations(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.installationsRequests++
	f.mu.Unlock()

	if _, err := f.verifyAppJWT(r); err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode([]map[string]any{
		{"id": f.installationID, "account": map[string]string{"login": f.orgLogin}},
	})
}

func (f *fakeGitHubApp) handleCreateToken(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.tokenRequests++
	f.mu.Unlock()

	if _, err := f.verifyAppJWT(r); err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	wantPath := fmt.Sprintf("/app/installations/%d/access_tokens", f.installationID)
	if r.URL.Path != wantPath {
		http.Error(w, fmt.Sprintf("path = %q, want %q (unknown installation id)", r.URL.Path, wantPath), http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"token": f.token})
}

// verifyAppJWT checks the request's Authorization: Bearer <jwt> against
// f.publicKey and f.appID.
func (f *fakeGitHubApp) verifyAppJWT(r *http.Request) (jwtClaims, error) {
	auth := r.Header.Get("Authorization")
	jwt, ok := strings.CutPrefix(auth, "Bearer ")
	if !ok {
		return jwtClaims{}, fmt.Errorf("missing bearer JWT")
	}
	claims, err := verifyJWT(jwt, f.publicKey)
	if err != nil {
		return jwtClaims{}, fmt.Errorf("invalid JWT: %w", err)
	}
	if claims.Iss != strconv.FormatInt(f.appID, 10) {
		return jwtClaims{}, fmt.Errorf("iss = %q, want %d", claims.Iss, f.appID)
	}
	return claims, nil
}

func (f *fakeGitHubApp) installationsRequestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.installationsRequests
}

func (f *fakeGitHubApp) tokenRequestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tokenRequests
}

type jwtClaims struct {
	Iss string `json:"iss"`
	Iat int64  `json:"iat"`
	Exp int64  `json:"exp"`
}

// verifyJWT checks jwt's RS256 signature against publicKey and returns its
// claims, using only the standard library: this is the same verification a
// real GitHub would apply, exercised here against iidp's own signer
// (internal/githubapp.SignJWT).
func verifyJWT(jwt string, publicKey *rsa.PublicKey) (jwtClaims, error) {
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return jwtClaims{}, fmt.Errorf("token has %d parts, want 3", len(parts))
	}
	signingInput := parts[0] + "." + parts[1]
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return jwtClaims{}, err
	}
	hashed := sha256.Sum256([]byte(signingInput))
	if err := rsa.VerifyPKCS1v15(publicKey, crypto.SHA256, hashed[:], sig); err != nil {
		return jwtClaims{}, fmt.Errorf("signature does not verify: %w", err)
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return jwtClaims{}, err
	}
	var claims jwtClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return jwtClaims{}, err
	}
	return claims, nil
}

// generateTestKeyPair returns a fresh RSA key pair and the PEM encoding of
// the private key, the shape the GitHub App private key ships in.
func generateTestKeyPair(t *testing.T) (*rsa.PrivateKey, []byte) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	block := &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}
	return key, pem.EncodeToMemory(block)
}

// setCIAppEnv sets the two environment variables iidp ci set-image
// authenticates with: IIDP_DEPLOY_APP_ID (the org Actions variable) and
// IIDP_DEPLOY_APP_PRIVATE_KEY (the org Actions secret PEM).
func setCIAppEnv(t *testing.T, appID int64, keyPEM []byte) {
	t.Helper()
	t.Setenv("IIDP_DEPLOY_APP_ID", strconv.FormatInt(appID, 10))
	t.Setenv("IIDP_DEPLOY_APP_PRIVATE_KEY", string(keyPEM))
}

// setImage runs iidp ci set-image in-process against the Platform
// repository at url.
func setImage(t *testing.T, url string, deps cli.Dependencies, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	full := append([]string{"ci", "set-image", "--platform-repo", url}, args...)
	var out, errOut bytes.Buffer
	code = cli.RunWith(full, strings.NewReader(""), &out, &errOut, deps)
	return out.String(), errOut.String(), code
}

func TestCISetImageWritesTheExplicitEnvironment(t *testing.T) {
	url := newPlatformRepository(t, testPlatformYAML)
	createApplication(t, url, cli.Dependencies{}, "--name", "shop", "--kind", "web-service")

	key, keyPEM := generateTestKeyPair(t)
	setCIAppEnv(t, 4242, keyPEM)
	fake := newFakeGitHubApp(t, &key.PublicKey, 4242, platform.Org, 99, "inst-token-abc")

	var observedToken string
	deps := cli.Dependencies{
		GitHubAPI:      fake.srv.URL,
		CIAuthObserved: func(token string) { observedToken = token },
	}

	stdout, stderr, code := setImage(t, url, deps, "shop", "prod", "abc123sha")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if fake.installationsRequestCount() != 1 {
		t.Errorf("GET /app/installations requests = %d, want 1", fake.installationsRequestCount())
	}
	if fake.tokenRequestCount() != 1 {
		t.Errorf("installation-token requests = %d, want 1", fake.tokenRequestCount())
	}
	if observedToken != "inst-token-abc" {
		t.Errorf("the git credential saw token %q, want the minted %q", observedToken, "inst-token-abc")
	}
	if !strings.Contains(stdout, "Deploy shop prod abc123sha") {
		t.Errorf("stdout = %q, want the exact commit message logged", stdout)
	}

	clone := cloneMain(t, url)
	if got := strings.TrimSpace(gitRun(t, clone, "log", "-1", "--format=%s")); got != "Deploy shop prod abc123sha" {
		t.Errorf("commit subject = %q, want %q", got, "Deploy shop prod abc123sha")
	}
	values := readYAML(t, filepath.Join(clone, "applications/shop/prod/values.yaml"))
	if got := lookup(t, values, "image", "tag"); got != "abc123sha" {
		t.Errorf("image.tag = %v, want abc123sha", got)
	}
	// Everything else in values.yaml survives untouched.
	if got := lookup(t, values, "image", "repository"); got != platform.Registry+"/shop" {
		t.Errorf("image.repository = %v, changed unexpectedly", got)
	}
	if got := lookup(t, values, "size"); got != "small" {
		t.Errorf("size = %v, changed unexpectedly", got)
	}
}

func TestCISetImageAutoPicksStagingWhenPresent(t *testing.T) {
	url := newPlatformRepository(t, testPlatformYAML)
	createApplication(t, url, cli.Dependencies{}, "--name", "shop", "--kind", "web-service", "--staging")

	key, keyPEM := generateTestKeyPair(t)
	setCIAppEnv(t, 1, keyPEM)
	fake := newFakeGitHubApp(t, &key.PublicKey, 1, platform.Org, 1, "tok")

	deps := cli.Dependencies{GitHubAPI: fake.srv.URL}
	stdout, stderr, code := setImage(t, url, deps, "shop", "auto", "sha1")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "Deploy shop staging sha1") {
		t.Errorf("stdout = %q, want it to say staging was chosen", stdout)
	}
	clone := cloneMain(t, url)
	values := readYAML(t, filepath.Join(clone, "applications/shop/staging/values.yaml"))
	if got := lookup(t, values, "image", "tag"); got != "sha1" {
		t.Errorf("staging image.tag = %v, want sha1", got)
	}
	prodValues := readYAML(t, filepath.Join(clone, "applications/shop/prod/values.yaml"))
	if got := lookup(t, prodValues, "image", "tag"); got != "" {
		t.Errorf("prod image.tag = %v, want untouched (empty)", got)
	}
}

func TestCISetImageAutoPicksProdWhenNoStaging(t *testing.T) {
	url := newPlatformRepository(t, testPlatformYAML)
	createApplication(t, url, cli.Dependencies{}, "--name", "shop", "--kind", "web-service")

	key, keyPEM := generateTestKeyPair(t)
	setCIAppEnv(t, 1, keyPEM)
	fake := newFakeGitHubApp(t, &key.PublicKey, 1, platform.Org, 1, "tok")

	deps := cli.Dependencies{GitHubAPI: fake.srv.URL}
	stdout, stderr, code := setImage(t, url, deps, "shop", "auto", "sha1")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "Deploy shop prod sha1") {
		t.Errorf("stdout = %q, want it to say prod was chosen", stdout)
	}
}

func TestCISetImageRetriesOnceWhenMainMoved(t *testing.T) {
	url := newPlatformRepository(t, testPlatformYAML)
	createApplication(t, url, cli.Dependencies{}, "--name", "shop", "--kind", "web-service")

	key, keyPEM := generateTestKeyPair(t)
	setCIAppEnv(t, 1, keyPEM)
	fake := newFakeGitHubApp(t, &key.PublicKey, 1, platform.Org, 1, "tok")

	pushes := 0
	deps := cli.Dependencies{
		GitHubAPI: fake.srv.URL,
		BeforePush: func() error {
			pushes++
			if pushes == 1 {
				pushCommit(t, url, "Hand-edit platform.yaml", map[string]string{
					"platform.yaml": testPlatformYAML + "handEdited: true\n",
				})
			}
			return nil
		},
	}

	stdout, stderr, code := setImage(t, url, deps, "shop", "prod", "sha2")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if pushes != 2 {
		t.Errorf("push attempts = %d, want 2", pushes)
	}
	// Minting the token happens once, before the Writer's retry loop; the
	// same credential is reused for both attempts.
	if fake.tokenRequestCount() != 1 {
		t.Errorf("installation-token requests = %d, want 1 (minted once, reused across the retry)", fake.tokenRequestCount())
	}
	clone := cloneMain(t, url)
	values := readYAML(t, filepath.Join(clone, "applications/shop/prod/values.yaml"))
	if got := lookup(t, values, "image", "tag"); got != "sha2" {
		t.Errorf("image.tag = %v, want sha2 (the tag must survive the retry)", got)
	}
}

func TestCISetImageRefusesAnUnknownApplication(t *testing.T) {
	url := newPlatformRepository(t, testPlatformYAML)
	key, keyPEM := generateTestKeyPair(t)
	setCIAppEnv(t, 1, keyPEM)
	fake := newFakeGitHubApp(t, &key.PublicKey, 1, platform.Org, 1, "tok")

	deps := cli.Dependencies{GitHubAPI: fake.srv.URL}
	_, stderr, code := setImage(t, url, deps, "no-such-app", "prod", "sha1")
	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero")
	}
	if !strings.Contains(stderr, "no-such-app") {
		t.Errorf("stderr = %q, want it to name the unknown Application", stderr)
	}
}

func TestCISetImageRefusesAnUnknownEnvironment(t *testing.T) {
	url := newPlatformRepository(t, testPlatformYAML)
	createApplication(t, url, cli.Dependencies{}, "--name", "shop", "--kind", "web-service")

	// No fake server, app id or private key needed: an unknown Environment
	// is refused before any authentication is attempted.
	_, stderr, code := setImage(t, url, cli.Dependencies{}, "shop", "canary", "sha1")
	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero")
	}
	if !strings.Contains(stderr, "canary") || !strings.Contains(stderr, "prod, staging or auto") {
		t.Errorf("stderr = %q, want it to explain the Environment", stderr)
	}
}

func TestCISetImageRefusesAnEnvironmentTheApplicationDoesNotHave(t *testing.T) {
	url := newPlatformRepository(t, testPlatformYAML)
	createApplication(t, url, cli.Dependencies{}, "--name", "shop", "--kind", "web-service")

	key, keyPEM := generateTestKeyPair(t)
	setCIAppEnv(t, 1, keyPEM)
	fake := newFakeGitHubApp(t, &key.PublicKey, 1, platform.Org, 1, "tok")

	deps := cli.Dependencies{GitHubAPI: fake.srv.URL}
	_, stderr, code := setImage(t, url, deps, "shop", "staging", "sha1")
	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero")
	}
	if !strings.Contains(stderr, "staging") || !strings.Contains(stderr, "shop") {
		t.Errorf("stderr = %q, want it to explain the missing Environment", stderr)
	}
}

func TestCISetImageFailsClearlyWithoutAPrivateKey(t *testing.T) {
	url := newPlatformRepository(t, testPlatformYAML)
	createApplication(t, url, cli.Dependencies{}, "--name", "shop", "--kind", "web-service")

	t.Setenv("IIDP_DEPLOY_APP_ID", "1")
	t.Setenv("IIDP_DEPLOY_APP_PRIVATE_KEY", "")
	t.Setenv("IIDP_DEPLOY_APP_PRIVATE_KEY_FILE", "")

	_, stderr, code := setImage(t, url, cli.Dependencies{}, "shop", "prod", "sha1")
	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero")
	}
	if !strings.Contains(stderr, "IIDP_DEPLOY_APP_PRIVATE_KEY") {
		t.Errorf("stderr = %q, want it to name IIDP_DEPLOY_APP_PRIVATE_KEY", stderr)
	}
}

func TestCISetImageFailsClearlyWithoutAppID(t *testing.T) {
	url := newPlatformRepository(t, testPlatformYAML)
	createApplication(t, url, cli.Dependencies{}, "--name", "shop", "--kind", "web-service")

	_, keyPEM := generateTestKeyPair(t)
	t.Setenv("IIDP_DEPLOY_APP_ID", "")
	t.Setenv("IIDP_DEPLOY_APP_PRIVATE_KEY", string(keyPEM))

	_, stderr, code := setImage(t, url, cli.Dependencies{}, "shop", "prod", "sha1")
	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero")
	}
	if !strings.Contains(stderr, "IIDP_DEPLOY_APP_ID") {
		t.Errorf("stderr = %q, want it to name IIDP_DEPLOY_APP_ID", stderr)
	}
}

func TestCISetImageFailsClearlyWhenNoInstallationMatchesTheOrg(t *testing.T) {
	url := newPlatformRepository(t, testPlatformYAML)
	createApplication(t, url, cli.Dependencies{}, "--name", "shop", "--kind", "web-service")

	key, keyPEM := generateTestKeyPair(t)
	setCIAppEnv(t, 1, keyPEM)
	// The fake's one installation is for a different account entirely.
	fake := newFakeGitHubApp(t, &key.PublicKey, 1, "someone-else", 1, "tok")

	deps := cli.Dependencies{GitHubAPI: fake.srv.URL}
	_, stderr, code := setImage(t, url, deps, "shop", "prod", "sha1")
	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero")
	}
	if !strings.Contains(stderr, platform.Org) {
		t.Errorf("stderr = %q, want it to name %s", stderr, platform.Org)
	}
}

// TestCISetImageNeverReadsThePlatformRepositoryAnonymously locks in the
// fix for the production bug this test guards against: the Platform
// repository is private, and iidp ci set-image must authenticate before
// its first (and only) clone, never attempting an anonymous read first. A
// pre-receive-style guard cannot observe an anonymous git clone directly
// (file:// remotes never consult a credential helper either way), so this
// is asserted the way the rest of this file already does: the whole flow
// succeeds using only the fake GitHub App's discovery-then-mint sequence,
// with no other credential source (no gh token: deps.TokenSource is left
// nil and unused by ci set-image) and no platform.yaml githubApp block at
// all (testPlatformYAML sets none), proving the app id and installation id
// came entirely from the environment and GitHub's API.
func TestCISetImageNeverReadsThePlatformRepositoryAnonymously(t *testing.T) {
	url := newPlatformRepository(t, testPlatformYAML)
	createApplication(t, url, cli.Dependencies{}, "--name", "shop", "--kind", "web-service")

	key, keyPEM := generateTestKeyPair(t)
	setCIAppEnv(t, 7, keyPEM)
	fake := newFakeGitHubApp(t, &key.PublicKey, 7, platform.Org, 77, "tok")

	deps := cli.Dependencies{GitHubAPI: fake.srv.URL}
	_, stderr, code := setImage(t, url, deps, "shop", "prod", "sha1")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr: %s", code, stderr)
	}
	if fake.installationsRequestCount() != 1 || fake.tokenRequestCount() != 1 {
		t.Errorf("installations requests = %d, token requests = %d, want 1 and 1", fake.installationsRequestCount(), fake.tokenRequestCount())
	}
}

// A GitHub-hosted runner has no git identity: nothing in the environment,
// no user.name or user.email in any config, and an empty account name for
// git to guess one from. The deploy write-back failed there on the first
// real run ("Author identity unknown") while every test passed, because
// the tests always supplied an identity. It must commit as the user whose
// push or tag started the workflow, so the Platform repository's log
// records who deployed what.
func TestCISetImageCommitsAsTheWorkflowActorOnARunnerWithNoGitIdentity(t *testing.T) {
	url := newPlatformRepository(t, testPlatformYAML)
	createApplication(t, url, cli.Dependencies{}, "--name", "shop", "--kind", "web-service")

	withoutGitIdentity(t)
	t.Setenv("GITHUB_ACTOR", "octocat")
	t.Setenv("GITHUB_ACTOR_ID", "583231")
	key, keyPEM := generateTestKeyPair(t)
	setCIAppEnv(t, 4242, keyPEM)
	fake := newFakeGitHubApp(t, &key.PublicKey, 4242, platform.Org, 99, "inst-token-abc")

	stdout, stderr, code := setImage(t, url, cli.Dependencies{GitHubAPI: fake.srv.URL}, "shop", "prod", "abc123sha")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}

	clone := cloneMain(t, url)
	want := "octocat <583231+octocat@users.noreply.github.com>"
	for _, format := range []string{"%an <%ae>", "%cn <%ce>"} {
		if got := strings.TrimSpace(gitRun(t, clone, "log", "-1", "--format="+format)); got != want {
			t.Errorf("git log --format=%q = %q, want %q", format, got, want)
		}
	}
}

// Outside Actions (an admin running the command by hand) there is no
// workflow actor, and the commit keeps git's own configured identity.
func TestCISetImageKeepsGitsOwnIdentityOutsideActions(t *testing.T) {
	url := newPlatformRepository(t, testPlatformYAML)
	createApplication(t, url, cli.Dependencies{}, "--name", "shop", "--kind", "web-service")

	for _, k := range []string{"GITHUB_ACTOR", "GITHUB_ACTOR_ID"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	key, keyPEM := generateTestKeyPair(t)
	setCIAppEnv(t, 4242, keyPEM)
	fake := newFakeGitHubApp(t, &key.PublicKey, 4242, platform.Org, 99, "inst-token-abc")

	stdout, stderr, code := setImage(t, url, cli.Dependencies{GitHubAPI: fake.srv.URL}, "shop", "prod", "abc123sha")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}

	clone := cloneMain(t, url)
	if got := strings.TrimSpace(gitRun(t, clone, "log", "-1", "--format=%an <%ae>")); got != "Test Developer <developer@example.com>" {
		t.Errorf("author = %q, want git's configured identity", got)
	}
}

// withoutGitIdentity makes git behave as on a fresh GitHub runner: no
// identity in the environment or any config file, and user.useConfigOnly
// so git refuses to guess one from the account and host name, as it
// otherwise quietly would on a developer's machine.
func withoutGitIdentity(t *testing.T) {
	t.Helper()
	cfg := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(cfg, []byte("[user]\n\tuseConfigOnly = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", cfg)
	for _, k := range []string{"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL", "EMAIL"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
}
