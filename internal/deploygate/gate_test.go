package deploygate_test

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
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Itema-as/iidp/internal/deploygate"
	"github.com/Itema-as/iidp/internal/oidc"
)

// The Deploy gate is tested through its HTTP boundary, the way CI calls
// it, against a fake OIDC issuer serving its own key set, a fake GitHub
// (the App's installation token and bot account) and a local bare Platform
// repository.

const (
	orgID       = 1230559
	shopRepoID  = 812345678
	audience    = "https://deploy.app.example.test"
	appID       = 4242
	installID   = 99
	botUserID   = 41898282
	installTok  = "inst-token-abc"
	appSlug     = "iidp-deploy"
	actor       = "octocat"
	actorID     = "583231"
	shaDeployed = "0123456789abcdef0123456789abcdef01234567"
)

// fakeIssuer signs tokens with its own key and publishes the public half
// as a JSON Web Key Set, the way token.actions.githubusercontent.com does.
type fakeIssuer struct {
	srv *httptest.Server
	key *rsa.PrivateKey
	kid string
}

func newFakeIssuer(t *testing.T) *fakeIssuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeIssuer{key: key, kid: "test-key-1"}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/jwks", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "alg": "RS256", "use": "sig", "kid": f.kid,
			"n": base64.RawURLEncoding.EncodeToString(key.PublicKey.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.PublicKey.E)).Bytes()),
		}}})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// claims are a valid token's claims for shop's repository pushing to main;
// tests change what they are about.
func (f *fakeIssuer) claims() map[string]any {
	now := time.Now()
	return map[string]any{
		"iss":                 f.srv.URL,
		"aud":                 audience,
		"iat":                 now.Unix(),
		"nbf":                 now.Unix(),
		"exp":                 now.Add(5 * time.Minute).Unix(),
		"repository":          "Itema-as/shop",
		"repository_id":       fmt.Sprint(shopRepoID),
		"repository_owner":    "Itema-as",
		"repository_owner_id": fmt.Sprint(orgID),
		"ref":                 "refs/heads/main",
		"ref_type":            "branch",
		"event_name":          "push",
		"actor":               actor,
		"actor_id":            actorID,
		"sha":                 shaDeployed,
		"workflow_ref":        "Itema-as/shop/.github/workflows/deploy.yaml@refs/heads/main",
	}
}

func (f *fakeIssuer) sign(t *testing.T, claims map[string]any) string {
	t.Helper()
	return signWith(t, f.key, f.kid, claims)
}

func signWith(t *testing.T, key *rsa.PrivateKey, kid string, claims map[string]any) string {
	t.Helper()
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT", "kid": kid})
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	input := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	hashed := sha256.Sum256([]byte(input))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, hashed[:])
	if err != nil {
		t.Fatal(err)
	}
	return input + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// fakeGitHub fakes the three GitHub calls the gate makes: the App's
// installation token, the App's slug, and its bot account's id.
type fakeGitHub struct {
	srv      *httptest.Server
	appKey   *rsa.PublicKey
	mu       sync.Mutex
	requests map[string]int
}

func newFakeGitHub(t *testing.T, appKey *rsa.PublicKey) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{appKey: appKey, requests: map[string]int{}}
	mux := http.NewServeMux()
	mux.HandleFunc(fmt.Sprintf("POST /app/installations/%d/access_tokens", installID), func(w http.ResponseWriter, r *http.Request) {
		f.count("token")
		if err := f.verifyAppJWT(r); err != nil {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"token": installTok})
	})
	mux.HandleFunc("GET /app", func(w http.ResponseWriter, r *http.Request) {
		f.count("app")
		if err := f.verifyAppJWT(r); err != nil {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": appID, "slug": appSlug})
	})
	mux.HandleFunc("GET /users/{login}", func(w http.ResponseWriter, r *http.Request) {
		f.count("users")
		if r.Header.Get("Authorization") != "Bearer "+installTok {
			http.Error(w, "want the installation token", http.StatusUnauthorized)
			return
		}
		if r.PathValue("login") != appSlug+"[bot]" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": botUserID, "login": appSlug + "[bot]"})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGitHub) count(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests[name]++
}

func (f *fakeGitHub) requestCount(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[name]
}

func (f *fakeGitHub) verifyAppJWT(r *http.Request) error {
	jwt, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return fmt.Errorf("no bearer JWT")
	}
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return fmt.Errorf("not a JWT")
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return err
	}
	hashed := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(f.appKey, crypto.SHA256, hashed[:], sig); err != nil {
		return fmt.Errorf("the App JWT does not verify: %w", err)
	}
	payload, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var claims struct {
		Iss string `json:"iss"`
	}
	_ = json.Unmarshal(payload, &claims)
	if claims.Iss != fmt.Sprint(appID) {
		return fmt.Errorf("iss = %q, want %d", claims.Iss, appID)
	}
	return nil
}

// env is one test's gate, its fakes and its Platform repository.
type env struct {
	t        *testing.T
	issuer   *fakeIssuer
	github   *fakeGitHub
	gate     *deploygate.Gate
	srv      *httptest.Server
	platform string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	setGitEnv(t)
	issuer := newFakeIssuer(t)
	appKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	gh := newFakeGitHub(t, &appKey.PublicKey)

	// The App credential, laid out the way the Secret volume of ArgoCD's
	// Platform-repository credential is.
	appDir := t.TempDir()
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(appKey)})
	writeFile(t, filepath.Join(appDir, "githubAppID"), fmt.Sprint(appID))
	writeFile(t, filepath.Join(appDir, "githubAppInstallationID"), fmt.Sprint(installID))
	writeFile(t, filepath.Join(appDir, "githubAppPrivateKey"), string(keyPEM))

	e := &env{t: t, issuer: issuer, github: gh, platform: newPlatformRepository(t)}
	e.gate = &deploygate.Gate{
		OIDC: &oidc.Verifier{
			Issuer:   issuer.srv.URL,
			JWKSURL:  issuer.srv.URL + "/.well-known/jwks",
			Audience: audience,
		},
		OrgID:        orgID,
		PlatformRepo: e.platform,
		GitHubAPI:    gh.srv.URL,
		Credentials:  deploygate.CredentialsFromDir(appDir),
	}
	e.srv = httptest.NewServer(e.gate.Handler())
	t.Cleanup(e.srv.Close)
	return e
}

// call posts a deploy request with token and returns the status and the
// decoded body.
func (e *env) call(token string, req deploygate.Request) (int, map[string]any) {
	e.t.Helper()
	body, _ := json.Marshal(req)
	httpReq, _ := http.NewRequest(http.MethodPost, e.srv.URL+deploygate.DeployPath, bytes.NewReader(body))
	if token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		e.t.Fatalf("decoding the response: %v", err)
	}
	return resp.StatusCode, out
}

// deploy calls with a token carrying claims.
func (e *env) deploy(claims map[string]any, application, environment, tag string) (int, map[string]any) {
	e.t.Helper()
	return e.call(e.issuer.sign(e.t, claims), deploygate.Request{Application: application, Environment: environment, Tag: tag})
}

// refused asserts a refusal with status whose message contains every one
// of wants, and that nothing was committed.
func (e *env) refused(status int, body map[string]any, wantStatus int, wants ...string) {
	e.t.Helper()
	msg, _ := body["error"].(string)
	if status != wantStatus {
		e.t.Errorf("status = %d, want %d (%q)", status, wantStatus, msg)
	}
	for _, want := range wants {
		if !strings.Contains(msg, want) {
			e.t.Errorf("error = %q, want it to mention %q", msg, want)
		}
	}
	if got := headSubject(e.t, e.platform); !strings.HasPrefix(got, "Seed") {
		e.t.Errorf("the Platform repository's head is %q, want nothing committed", got)
	}
}

func TestTheGateDeploysMainToProdWithTheActorAsAuthorAndTheAppAsCommitter(t *testing.T) {
	e := newEnv(t)
	addApplication(t, e.platform, "shop", false, binding(shopRepoID, orgID))

	status, body := e.deploy(e.issuer.claims(), "shop", "auto", shaDeployed)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %v", status, body)
	}
	if body["environment"] != "prod" || body["file"] != "applications/shop/prod/values.yaml" || body["commit"] == "" {
		t.Errorf("response = %v, want prod written and a commit", body)
	}

	clone := cloneMain(t, e.platform)
	if got := gitRun(t, clone, "log", "-1", "--format=%s"); strings.TrimSpace(got) != "Deploy shop prod "+shaDeployed {
		t.Errorf("subject = %q", got)
	}
	if got := strings.TrimSpace(gitRun(t, clone, "log", "-1", "--format=%an <%ae>")); got != "octocat <583231+octocat@users.noreply.github.com>" {
		t.Errorf("author = %q, want the token's actor", got)
	}
	if got := strings.TrimSpace(gitRun(t, clone, "log", "-1", "--format=%cn <%ce>")); got != "iidp-deploy[bot] <41898282+iidp-deploy[bot]@users.noreply.github.com>" {
		t.Errorf("committer = %q, want the App's bot", got)
	}
	if got := gitRun(t, clone, "log", "-1", "--format=%b"); !strings.Contains(got, "Itema-as/shop (repository id 812345678)") || !strings.Contains(got, shaDeployed) {
		t.Errorf("body = %q, want the calling repository and commit", got)
	}
	if got := strings.TrimSpace(gitRun(t, clone, "rev-parse", "HEAD")); got != body["commit"] {
		t.Errorf("response commit = %v, head = %s", body["commit"], got)
	}
	values := readFile(t, filepath.Join(clone, "applications/shop/prod/values.yaml"))
	if !strings.Contains(values, "tag: "+shaDeployed) {
		t.Errorf("values.yaml does not carry the tag:\n%s", values)
	}
	if !strings.Contains(values, "# shop's prod values") || !strings.Contains(values, "size: small") {
		t.Errorf("the rest of values.yaml changed:\n%s", values)
	}

	// The bot's id is looked up once and reused.
	if status, body := e.deploy(e.issuer.claims(), "shop", "prod", "second"); status != http.StatusOK {
		t.Fatalf("second deploy: %d %v", status, body)
	}
	if n := e.github.requestCount("users"); n != 1 {
		t.Errorf("GET /users requests = %d, want 1 (cached)", n)
	}
	if n := e.github.requestCount("token"); n != 2 {
		t.Errorf("installation tokens minted = %d, want one per deploy", n)
	}
}

func TestTheGateDeploysMainToStagingWhenThereIsOne(t *testing.T) {
	e := newEnv(t)
	addApplication(t, e.platform, "shop", true, binding(shopRepoID, orgID))

	for _, environment := range []string{"auto", "staging"} {
		status, body := e.deploy(e.issuer.claims(), "shop", environment, "sha-"+environment)
		if status != http.StatusOK || body["environment"] != "staging" {
			t.Fatalf("%s: status %d, body %v, want staging written", environment, status, body)
		}
	}
	clone := cloneMain(t, e.platform)
	if values := readFile(t, filepath.Join(clone, "applications/shop/prod/values.yaml")); !strings.Contains(values, `tag: ""`) {
		t.Errorf("prod changed on a main deploy:\n%s", values)
	}
}

func TestTheGatePromotesAVTagToProd(t *testing.T) {
	e := newEnv(t)
	addApplication(t, e.platform, "shop", true, binding(shopRepoID, orgID))
	claims := e.issuer.claims()
	claims["ref"], claims["ref_type"] = "refs/tags/v1.2.3", "tag"

	for _, environment := range []string{"prod", "auto"} {
		status, body := e.deploy(claims, "shop", environment, "1.2.3")
		if status != http.StatusOK || body["environment"] != "prod" {
			t.Fatalf("%s: status %d, body %v, want prod written", environment, status, body)
		}
	}
	// The second call found prod already on 1.2.3: nothing to commit.
	if status, body := e.deploy(claims, "shop", "prod", "1.2.3"); status != http.StatusOK || body["unchanged"] != true {
		t.Errorf("a repeated promote = %d %v, want 200 and unchanged", status, body)
	}
	if got := headSubject(t, e.platform); got != "Deploy shop prod 1.2.3" {
		t.Errorf("head = %q", got)
	}
}

func TestTheGateRefusesATokenItCannotTrust(t *testing.T) {
	forged, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		token func(e *env) string
		want  string
	}{
		{"no token", func(*env) string { return "" }, "no GitHub Actions OIDC token"},
		{"not a JWT", func(*env) string { return "not-a-token" }, "not a JSON Web Token"},
		{"a bad signature", func(e *env) string { return signWith(t, forged, e.issuer.kid, e.issuer.claims()) }, "signature does not verify"},
		{"an unknown key", func(e *env) string { return signWith(t, forged, "other-key", e.issuer.claims()) }, "does not publish"},
		{"the wrong audience", func(e *env) string {
			c := e.issuer.claims()
			c["aud"] = "https://github.com/Itema-as"
			return e.issuer.sign(t, c)
		}, audience},
		{"another issuer", func(e *env) string {
			c := e.issuer.claims()
			c["iss"] = "https://token.example.test"
			return e.issuer.sign(t, c)
		}, "issued by"},
		{"an expired token", func(e *env) string {
			c := e.issuer.claims()
			c["exp"] = time.Now().Add(-10 * time.Minute).Unix()
			return e.issuer.sign(t, c)
		}, "expired"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			addApplication(t, e.platform, "shop", false, binding(shopRepoID, orgID))
			status, body := e.call(tc.token(e), deploygate.Request{Application: "shop", Environment: "auto", Tag: "sha1"})
			e.refused(status, body, http.StatusUnauthorized, tc.want)
			if n := e.github.requestCount("token"); n != 0 {
				t.Errorf("minted %d installation tokens for an untrusted caller, want 0", n)
			}
		})
	}
}

func TestTheGateRefusesARepositoryOfAnotherOrg(t *testing.T) {
	e := newEnv(t)
	addApplication(t, e.platform, "shop", false, binding(shopRepoID, orgID))
	claims := e.issuer.claims()
	claims["repository"], claims["repository_owner"], claims["repository_owner_id"] = "someone/shop", "someone", "777"

	status, body := e.deploy(claims, "shop", "auto", "sha1")
	e.refused(status, body, http.StatusForbidden, "someone/shop", "Itema-as", "1230559")
}

func TestTheGateRefusesAnotherRepositoryOfTheOrg(t *testing.T) {
	e := newEnv(t)
	addApplication(t, e.platform, "shop", false, binding(shopRepoID, orgID))
	claims := e.issuer.claims()
	claims["repository"], claims["repository_id"] = "Itema-as/other", "999"

	status, body := e.deploy(claims, "shop", "auto", "sha1")
	e.refused(status, body, http.StatusForbidden, "Itema-as/shop (repository id 812345678)", "Itema-as/other (repository id 999)")
}

// A repository deleted and recreated under the same name has a new id, so
// the name alone deploys nothing.
func TestTheGateRefusesARecreatedRepositoryWithTheSameName(t *testing.T) {
	e := newEnv(t)
	addApplication(t, e.platform, "shop", false, binding(shopRepoID, orgID))
	claims := e.issuer.claims()
	claims["repository_id"] = "900000001"

	status, body := e.deploy(claims, "shop", "auto", "sha1")
	e.refused(status, body, http.StatusForbidden, "repository id 900000001")
}

func TestTheGateRefusesAnApplicationWithoutARecordedID(t *testing.T) {
	for name, repositoryYAML := range map[string]string{
		"no binding":       "",
		"no repository id": "repository: Itema-as/shop\nrepositoryOwnerId: 1230559\n",
		"a quoted id":      "repository: Itema-as/shop\nrepositoryId: \"812345678\"\nrepositoryOwnerId: 1230559\n",
		"not YAML":         "repository: [\n",
		"another org's id": "repository: someone/shop\nrepositoryId: 812345678\nrepositoryOwnerId: 777\n",
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			addApplication(t, e.platform, "shop", false, repositoryYAML)
			status, body := e.deploy(e.issuer.claims(), "shop", "auto", "sha1")
			e.refused(status, body, http.StatusForbidden, "iidp app bind shop --repo")
		})
	}
}

func TestTheGateRefusesADisallowedRef(t *testing.T) {
	for _, ref := range []string{"refs/heads/feature", "refs/pull/7/merge", "refs/tags/release-1", "refs/heads/mainline"} {
		t.Run(ref, func(t *testing.T) {
			e := newEnv(t)
			addApplication(t, e.platform, "shop", true, binding(shopRepoID, orgID))
			claims := e.issuer.claims()
			claims["ref"] = ref
			status, body := e.deploy(claims, "shop", "auto", "sha1")
			e.refused(status, body, http.StatusForbidden, ref, "refs/heads/main", "v*")
		})
	}
}

func TestTheGateRefusesAnEnvironmentTheRefMayNotDeploy(t *testing.T) {
	e := newEnv(t)
	addApplication(t, e.platform, "shop", true, binding(shopRepoID, orgID))

	status, body := e.deploy(e.issuer.claims(), "shop", "prod", "sha1")
	e.refused(status, body, http.StatusForbidden, "staging", "v* tag")

	claims := e.issuer.claims()
	claims["ref"] = "refs/tags/v2.0.0"
	status, body = e.deploy(claims, "shop", "staging", "2.0.0")
	e.refused(status, body, http.StatusForbidden, "prod only")
}

func TestTheGateRefusesAMissingEnvironmentOrApplication(t *testing.T) {
	e := newEnv(t)
	addApplication(t, e.platform, "shop", false, binding(shopRepoID, orgID))

	status, body := e.deploy(e.issuer.claims(), "shop", "staging", "sha1")
	e.refused(status, body, http.StatusNotFound, "no staging Environment")

	status, body = e.deploy(e.issuer.claims(), "nothing", "auto", "sha1")
	e.refused(status, body, http.StatusNotFound, "nothing")
}

func TestTheGateRefusesAMalformedRequest(t *testing.T) {
	e := newEnv(t)
	addApplication(t, e.platform, "shop", false, binding(shopRepoID, orgID))
	for _, req := range []deploygate.Request{
		{Application: "Shop!", Environment: "auto", Tag: "sha1"},
		{Application: "shop", Environment: "canary", Tag: "sha1"},
		{Application: "shop", Environment: "auto", Tag: ""},
		{Application: "shop", Environment: "auto", Tag: "sha1\nimage: evil"},
	} {
		status, body := e.call(e.issuer.sign(t, e.issuer.claims()), req)
		e.refused(status, body, http.StatusBadRequest)
	}
}

func TestTheGateRetriesOnceWhenMainMoved(t *testing.T) {
	e := newEnv(t)
	addApplication(t, e.platform, "shop", false, binding(shopRepoID, orgID))
	pushes := 0
	e.gate.BeforePush = func() error {
		pushes++
		if pushes == 1 {
			pushCommit(t, e.platform, "Someone else's change", map[string]string{"other.txt": "moved\n"})
		}
		return nil
	}
	status, body := e.deploy(e.issuer.claims(), "shop", "auto", "sha2")
	if status != http.StatusOK {
		t.Fatalf("status = %d: %v", status, body)
	}
	if pushes != 2 {
		t.Errorf("push attempts = %d, want 2", pushes)
	}
	clone := cloneMain(t, e.platform)
	if got := gitRun(t, clone, "log", "--format=%s", "-3"); !strings.Contains(got, "Deploy shop prod sha2\nSomeone else's change") {
		t.Errorf("log = %q, want the deploy on top of the moved main", got)
	}
}

func TestTheGateAnswersHealthChecks(t *testing.T) {
	e := newEnv(t)
	resp, err := http.Get(e.srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /healthz = %d", resp.StatusCode)
	}
}

// binding is the repository.yaml iidp app create writes.
func binding(repositoryID, ownerID int64) string {
	return fmt.Sprintf("# Written by iidp\nrepository: Itema-as/shop\nrepositoryId: %d\nrepositoryOwnerId: %d\n", repositoryID, ownerID)
}

// addApplication commits an Application with prod (and staging) the way
// iidp app create writes it, bound by repositoryYAML ("" for unbound), as
// the seed commit.
func addApplication(t *testing.T, url, name string, staging bool, repositoryYAML string) {
	t.Helper()
	dir := cloneMain(t, url)
	envs := []string{"prod"}
	if staging {
		envs = append(envs, "staging")
	}
	for _, environment := range envs {
		base := filepath.Join(dir, "applications", name, environment)
		writeFile(t, filepath.Join(base, "application.yaml"), "apiVersion: argoproj.io/v1alpha1\nkind: Application\n")
		writeFile(t, filepath.Join(base, "values.yaml"), fmt.Sprintf("# %s's %s values\napplication:\n  name: %s\nenvironment: %s\nimage:\n  repository: ghcr.io/itema-as/%s\n  tag: \"\"\nsize: small\n", name, environment, name, environment, name))
	}
	if repositoryYAML != "" {
		writeFile(t, filepath.Join(dir, "applications", name, "repository.yaml"), repositoryYAML)
	}
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "--amend", "--no-edit", "-m", "Seed the Platform repository")
	gitRun(t, dir, "push", "--force", "origin", "HEAD:main")
}

func newPlatformRepository(t *testing.T) string {
	t.Helper()
	bare := filepath.Join(t.TempDir(), "iidp-platform.git")
	gitRun(t, t.TempDir(), "init", "--bare", "--initial-branch=main", bare)
	url := "file://" + bare
	seed := cloneMain(t, url)
	writeFile(t, filepath.Join(seed, "platform.yaml"), "baseDomain: app.example.test\nchartVersion: 0.3.1\n")
	writeFile(t, filepath.Join(seed, "applications", ".gitkeep"), "")
	gitRun(t, seed, "add", "-A")
	gitRun(t, seed, "commit", "-m", "Seed the Platform repository")
	gitRun(t, seed, "push", "origin", "HEAD:main")
	return url
}

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

func headSubject(t *testing.T, url string) string {
	t.Helper()
	return strings.TrimSpace(gitRun(t, cloneMain(t, url), "log", "-1", "--format=%s"))
}

// setGitEnv isolates git from the developer's own configuration (signing,
// hooks) and gives the test's own commits an identity.
func setGitEnv(t *testing.T) {
	t.Helper()
	empty := filepath.Join(t.TempDir(), "gitconfig")
	writeFile(t, empty, "")
	t.Setenv("GIT_CONFIG_GLOBAL", empty)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_AUTHOR_NAME", "Test Developer")
	t.Setenv("GIT_AUTHOR_EMAIL", "developer@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "Test Developer")
	t.Setenv("GIT_COMMITTER_EMAIL", "developer@example.com")
}

func cloneMain(t *testing.T, url string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "clone")
	gitRun(t, t.TempDir(), "clone", "--quiet", url, dir)
	return dir
}

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

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
