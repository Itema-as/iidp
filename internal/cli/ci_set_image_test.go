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
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/Itema-as/iidp/internal/cli"
	"github.com/Itema-as/iidp/internal/platform"
)

// fakeInstallationTokenServer is an in-process fake of the one GitHub App
// endpoint iidp ci set-image needs: POST /app/installations/{id}/access_tokens.
// It verifies the request is authenticated with a validly-signed RS256 JWT
// for the expected app id (docs/implementation-notes/12-deploy-workflow.md)
// and, if so, returns token.
type fakeInstallationTokenServer struct {
	srv       *httptest.Server
	publicKey *rsa.PublicKey
	appID     int64
	token     string

	mu       sync.Mutex
	requests int
}

func newFakeInstallationTokenServer(t *testing.T, publicKey *rsa.PublicKey, appID int64, token string) *fakeInstallationTokenServer {
	t.Helper()
	f := &fakeInstallationTokenServer{publicKey: publicKey, appID: appID, token: token}
	mux := http.NewServeMux()
	mux.HandleFunc("/app/installations/", f.handle)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeInstallationTokenServer) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests++
	f.mu.Unlock()

	auth := r.Header.Get("Authorization")
	jwt, ok := strings.CutPrefix(auth, "Bearer ")
	if !ok {
		http.Error(w, "missing bearer JWT", http.StatusUnauthorized)
		return
	}
	claims, err := verifyJWT(jwt, f.publicKey)
	if err != nil {
		http.Error(w, "invalid JWT: "+err.Error(), http.StatusUnauthorized)
		return
	}
	if claims.Iss != strconv.FormatInt(f.appID, 10) {
		http.Error(w, fmt.Sprintf("iss = %q, want %d", claims.Iss, f.appID), http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"token": f.token})
}

func (f *fakeInstallationTokenServer) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests
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

// ciPlatformYAML is testPlatformYAML plus the githubApp fields ci
// set-image needs.
func ciPlatformYAML(appID, installationID int64) string {
	return testPlatformYAML + fmt.Sprintf("githubApp:\n  id: %d\n  installationId: %d\n", appID, installationID)
}

// setImage runs iidp ci set-image in-process against the Platform
// repository at url, with the GitHub App private key and API base URL
// wired for a fake installation-token server.
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
	t.Setenv("IIDP_DEPLOY_APP_PRIVATE_KEY", string(keyPEM))
	fake := newFakeInstallationTokenServer(t, &key.PublicKey, 4242, "inst-token-abc")

	// Re-seed platform.yaml with the githubApp fields once shop exists,
	// exactly the way a hand edit or a later commit would add them.
	pushCommit(t, url, "Add githubApp to platform.yaml", map[string]string{
		"platform.yaml": ciPlatformYAML(4242, 99),
	})

	var observedToken string
	deps := cli.Dependencies{
		GitHubAPI:      fake.srv.URL,
		CIAuthObserved: func(token string) { observedToken = token },
	}

	stdout, stderr, code := setImage(t, url, deps, "shop", "prod", "abc123sha")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if fake.requestCount() != 1 {
		t.Errorf("installation-token requests = %d, want 1", fake.requestCount())
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
	t.Setenv("IIDP_DEPLOY_APP_PRIVATE_KEY", string(keyPEM))
	fake := newFakeInstallationTokenServer(t, &key.PublicKey, 1, "tok")
	pushCommit(t, url, "Add githubApp", map[string]string{"platform.yaml": ciPlatformYAML(1, 1)})

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
	t.Setenv("IIDP_DEPLOY_APP_PRIVATE_KEY", string(keyPEM))
	fake := newFakeInstallationTokenServer(t, &key.PublicKey, 1, "tok")
	pushCommit(t, url, "Add githubApp", map[string]string{"platform.yaml": ciPlatformYAML(1, 1)})

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
	t.Setenv("IIDP_DEPLOY_APP_PRIVATE_KEY", string(keyPEM))
	fake := newFakeInstallationTokenServer(t, &key.PublicKey, 1, "tok")
	pushCommit(t, url, "Add githubApp", map[string]string{"platform.yaml": ciPlatformYAML(1, 1)})

	pushes := 0
	deps := cli.Dependencies{
		GitHubAPI: fake.srv.URL,
		BeforePush: func() error {
			pushes++
			if pushes == 1 {
				pushCommit(t, url, "Hand-edit platform.yaml", map[string]string{
					"platform.yaml": ciPlatformYAML(1, 1) + "handEdited: true\n",
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
	clone := cloneMain(t, url)
	values := readYAML(t, filepath.Join(clone, "applications/shop/prod/values.yaml"))
	if got := lookup(t, values, "image", "tag"); got != "sha2" {
		t.Errorf("image.tag = %v, want sha2 (the tag must survive the retry)", got)
	}
}

func TestCISetImageRefusesAnUnknownApplication(t *testing.T) {
	url := newPlatformRepository(t, testPlatformYAML)
	key, keyPEM := generateTestKeyPair(t)
	t.Setenv("IIDP_DEPLOY_APP_PRIVATE_KEY", string(keyPEM))
	fake := newFakeInstallationTokenServer(t, &key.PublicKey, 1, "tok")
	pushCommit(t, url, "Add githubApp", map[string]string{"platform.yaml": ciPlatformYAML(1, 1)})

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

	// No fake server or private key needed: an unknown Environment is
	// refused before any authentication is attempted.
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
	t.Setenv("IIDP_DEPLOY_APP_PRIVATE_KEY", string(keyPEM))
	fake := newFakeInstallationTokenServer(t, &key.PublicKey, 1, "tok")
	pushCommit(t, url, "Add githubApp", map[string]string{"platform.yaml": ciPlatformYAML(1, 1)})

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
	pushCommit(t, url, "Add githubApp", map[string]string{"platform.yaml": ciPlatformYAML(1, 1)})

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

func TestCISetImageFailsClearlyWithoutGitHubAppConfig(t *testing.T) {
	url := newPlatformRepository(t, testPlatformYAML)
	createApplication(t, url, cli.Dependencies{}, "--name", "shop", "--kind", "web-service")
	_, keyPEM := generateTestKeyPair(t)
	t.Setenv("IIDP_DEPLOY_APP_PRIVATE_KEY", string(keyPEM))

	// testPlatformYAML sets no githubApp block at all.
	_, stderr, code := setImage(t, url, cli.Dependencies{}, "shop", "prod", "sha1")
	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero")
	}
	if !strings.Contains(stderr, "githubApp") {
		t.Errorf("stderr = %q, want it to name githubApp", stderr)
	}
}
