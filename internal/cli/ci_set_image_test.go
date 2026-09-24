package cli_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Itema-as/iidp/internal/cli"
)

// iidp ci set-image is the Deploy gate's client
// (docs/implementation-notes/60-deploy-gate.md). These tests run it the way
// the deploy workflow does, against a fake of the GitHub Actions OIDC token
// endpoint and a fake gate.

// fakeActionsTokenService is the endpoint behind
// ACTIONS_ID_TOKEN_REQUEST_URL: it answers {"value": <token>} to a request
// carrying ACTIONS_ID_TOKEN_REQUEST_TOKEN, and records the audience asked
// for.
type fakeActionsTokenService struct {
	srv          *httptest.Server
	mu           sync.Mutex
	audiences    []string
	requestToken string
}

func newFakeActionsTokenService(t *testing.T) *fakeActionsTokenService {
	t.Helper()
	f := &fakeActionsTokenService{requestToken: "actions-request-token"}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "bearer "+f.requestToken {
			http.Error(w, "bad request token", http.StatusUnauthorized)
			return
		}
		if r.URL.Query().Get("api-version") != "2.0" {
			http.Error(w, "the request URL's own query was lost", http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.audiences = append(f.audiences, r.URL.Query().Get("audience"))
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]string{"value": "oidc-token-for-" + r.URL.Query().Get("audience")})
	}))
	t.Cleanup(f.srv.Close)
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_URL", f.srv.URL+"/token?api-version=2.0")
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", f.requestToken)
	return f
}

// fakeGate records the calls it gets and answers each with the next of its
// responses (the last one repeating).
type fakeGate struct {
	srv       *httptest.Server
	mu        sync.Mutex
	calls     []gateCall
	responses []gateResponse
}

type gateCall struct {
	path, authorization string
	body                map[string]string
}

type gateResponse struct {
	status int
	body   string
}

func newFakeGate(t *testing.T, responses ...gateResponse) *fakeGate {
	t.Helper()
	f := &fakeGate{responses: responses}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.calls = append(f.calls, gateCall{path: r.Method + " " + r.URL.Path, authorization: r.Header.Get("Authorization"), body: body})
		resp := f.responses[min(len(f.calls), len(f.responses))-1]
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.status)
		_, _ = w.Write([]byte(resp.body))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGate) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// setImage runs iidp ci set-image in-process.
func setImage(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = cli.RunWith(append([]string{"ci", "set-image"}, args...), strings.NewReader(""), &out, &errOut, cli.Dependencies{CIRetryDelay: time.Millisecond})
	return out.String(), errOut.String(), code
}

const deployed = `{"application":"shop","environment":"staging","tag":"abc123","file":"applications/shop/staging/values.yaml","commit":"0123456789abcdef0123"}`

func TestCISetImageCallsTheGateWithAnOIDCTokenForItsURL(t *testing.T) {
	tokens := newFakeActionsTokenService(t)
	gate := newFakeGate(t, gateResponse{http.StatusOK, deployed})
	// The trailing slash is dropped: the audience is the gate's URL as the
	// gate itself spells it.
	t.Setenv("IIDP_DEPLOY_GATE_URL", gate.srv.URL+"/")

	stdout, stderr, code := setImage(t, "shop", "auto", "abc123")
	if code != 0 {
		t.Fatalf("exit code = %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if len(tokens.audiences) != 1 || tokens.audiences[0] != gate.srv.URL {
		t.Errorf("OIDC token audiences requested = %v, want [%s]", tokens.audiences, gate.srv.URL)
	}
	if gate.callCount() != 1 {
		t.Fatalf("gate calls = %d, want 1", gate.callCount())
	}
	call := gate.calls[0]
	if call.path != "POST /v1/deploy" {
		t.Errorf("gate call = %s, want POST /v1/deploy", call.path)
	}
	if call.authorization != "Bearer oidc-token-for-"+gate.srv.URL {
		t.Errorf("gate Authorization = %q, want the OIDC token", call.authorization)
	}
	if call.body["application"] != "shop" || call.body["environment"] != "auto" || call.body["tag"] != "abc123" {
		t.Errorf("gate request = %v", call.body)
	}
	for _, want := range []string{"Deploy shop staging abc123", "0123456789ab", "applications/shop/staging/values.yaml"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout = %q, want %q", stdout, want)
		}
	}
}

func TestCISetImageTakesTheGateURLFromTheFlag(t *testing.T) {
	newFakeActionsTokenService(t)
	gate := newFakeGate(t, gateResponse{http.StatusOK, deployed})
	t.Setenv("IIDP_DEPLOY_GATE_URL", "https://deploy.elsewhere.invalid")

	_, stderr, code := setImage(t, "--gate-url", gate.srv.URL, "shop", "prod", "1.2.3")
	if code != 0 || gate.callCount() != 1 {
		t.Fatalf("exit code = %d, gate calls = %d\n%s", code, gate.callCount(), stderr)
	}
}

// The App key left CI with the Deploy gate: whatever IIDP_DEPLOY_APP_*
// still holds on a runner is neither needed nor read.
func TestCISetImageNeedsNoAppKey(t *testing.T) {
	newFakeActionsTokenService(t)
	gate := newFakeGate(t, gateResponse{http.StatusOK, deployed})
	t.Setenv("IIDP_DEPLOY_GATE_URL", gate.srv.URL)
	for _, name := range []string{"IIDP_DEPLOY_APP_ID", "IIDP_DEPLOY_APP_PRIVATE_KEY", "IIDP_DEPLOY_APP_PRIVATE_KEY_FILE"} {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}

	stdout, stderr, code := setImage(t, "shop", "auto", "abc123")
	if code != 0 {
		t.Fatalf("exit code = %d\n%s", code, stderr)
	}
	if strings.Contains(stdout+stderr, "IIDP_DEPLOY_APP") {
		t.Errorf("output mentions IIDP_DEPLOY_APP_*:\n%s%s", stdout, stderr)
	}
}

func TestCISetImageShowsTheGatesRefusal(t *testing.T) {
	newFakeActionsTokenService(t)
	refusal := "refused: shop is bound to Itema-as/shop (repository id 1), and this call comes from Itema-as/other (repository id 2). Only an Application's own repository can deploy it"
	body, _ := json.Marshal(map[string]string{"error": refusal})
	gate := newFakeGate(t, gateResponse{http.StatusForbidden, string(body)})
	t.Setenv("IIDP_DEPLOY_GATE_URL", gate.srv.URL)

	_, stderr, code := setImage(t, "shop", "auto", "abc123")
	if code == 0 {
		t.Fatal("exit code = 0, want a failure")
	}
	if !strings.Contains(stderr, refusal) || !strings.Contains(stderr, "HTTP 403") {
		t.Errorf("stderr = %q, want the gate's refusal and status", stderr)
	}
	if gate.callCount() != 1 {
		t.Errorf("gate calls = %d, want 1: a refusal is not retried", gate.callCount())
	}
}

func TestCISetImageRetriesAGateThatIsUnavailable(t *testing.T) {
	newFakeActionsTokenService(t)
	gate := newFakeGate(t, gateResponse{http.StatusServiceUnavailable, "no available server"}, gateResponse{http.StatusBadGateway, "bad gateway"}, gateResponse{http.StatusOK, deployed})
	t.Setenv("IIDP_DEPLOY_GATE_URL", gate.srv.URL)

	_, stderr, code := setImage(t, "shop", "auto", "abc123")
	if code != 0 {
		t.Fatalf("exit code = %d\n%s", code, stderr)
	}
	if gate.callCount() != 3 {
		t.Errorf("gate calls = %d, want 3", gate.callCount())
	}
}

func TestCISetImageGivesUpOnAGateThatStaysUnavailable(t *testing.T) {
	newFakeActionsTokenService(t)
	gate := newFakeGate(t, gateResponse{http.StatusServiceUnavailable, "no available server"})
	t.Setenv("IIDP_DEPLOY_GATE_URL", gate.srv.URL)

	_, stderr, code := setImage(t, "shop", "auto", "abc123")
	if code == 0 || !strings.Contains(stderr, "unavailable") {
		t.Fatalf("exit code = %d, stderr = %q, want a failure saying the gate is unavailable", code, stderr)
	}
	if gate.callCount() != 3 {
		t.Errorf("gate calls = %d, want 3", gate.callCount())
	}
}

func TestCISetImageSaysWhenTheEnvironmentAlreadyRunsTheTag(t *testing.T) {
	newFakeActionsTokenService(t)
	gate := newFakeGate(t, gateResponse{http.StatusOK, `{"application":"shop","environment":"prod","tag":"1.2.3","file":"applications/shop/prod/values.yaml","unchanged":true}`})
	t.Setenv("IIDP_DEPLOY_GATE_URL", gate.srv.URL)

	stdout, stderr, code := setImage(t, "shop", "prod", "1.2.3")
	if code != 0 || !strings.Contains(stdout, "already runs 1.2.3") {
		t.Errorf("exit code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
}

func TestCISetImageFailsClearlyOutsideAJobWithIDTokenWrite(t *testing.T) {
	for _, name := range []string{"ACTIONS_ID_TOKEN_REQUEST_URL", "ACTIONS_ID_TOKEN_REQUEST_TOKEN"} {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
	gate := newFakeGate(t, gateResponse{http.StatusOK, deployed})
	t.Setenv("IIDP_DEPLOY_GATE_URL", gate.srv.URL)

	_, stderr, code := setImage(t, "shop", "auto", "abc123")
	if code == 0 || !strings.Contains(stderr, "id-token: write") {
		t.Errorf("exit code = %d, stderr = %q, want it to name id-token: write", code, stderr)
	}
	if gate.callCount() != 0 {
		t.Errorf("gate calls = %d, want 0", gate.callCount())
	}
}

func TestCISetImageFailsClearlyWithoutTheGatesURL(t *testing.T) {
	tokens := newFakeActionsTokenService(t)
	t.Setenv("IIDP_DEPLOY_GATE_URL", "")
	os.Unsetenv("IIDP_DEPLOY_GATE_URL")

	_, stderr, code := setImage(t, "shop", "auto", "abc123")
	if code == 0 || !strings.Contains(stderr, "IIDP_DEPLOY_GATE_URL") {
		t.Errorf("exit code = %d, stderr = %q, want it to name IIDP_DEPLOY_GATE_URL", code, stderr)
	}
	if len(tokens.audiences) != 0 {
		t.Errorf("requested a token without knowing the audience")
	}
}

func TestCISetImageRefusesBadArgumentsBeforeAnyCall(t *testing.T) {
	tokens := newFakeActionsTokenService(t)
	gate := newFakeGate(t, gateResponse{http.StatusOK, deployed})
	t.Setenv("IIDP_DEPLOY_GATE_URL", gate.srv.URL)

	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"shop", "canary", "sha1"}, "prod, staging or auto"},
		{[]string{"Shop", "prod", "sha1"}, "Shop"},
		{[]string{"shop", "prod", " "}, "tag must not be empty"},
	} {
		_, stderr, code := setImage(t, tc.args...)
		if code == 0 || !strings.Contains(stderr, tc.want) {
			t.Errorf("%v: exit code = %d, stderr = %q, want %q", tc.args, code, stderr, tc.want)
		}
	}
	if len(tokens.audiences) != 0 || gate.callCount() != 0 {
		t.Errorf("made %d token requests and %d gate calls, want none", len(tokens.audiences), gate.callCount())
	}
}

// ci set-image reads iidp.yaml from the checkout it runs in, the commit
// being deployed, and sends its migration command with the tag
// (docs/implementation-notes/66-migration-command-in-repo.md).
func TestCISetImageSendsTheMigrationCommandFromIidpYAML(t *testing.T) {
	for _, tc := range []struct {
		name    string
		file    string // "" means no iidp.yaml at all
		present bool
		want    string
		says    string
	}{
		{"a command", "# comment\nmigrationCommand: npx prisma migrate deploy\n", true, "npx prisma migrate deploy", "Migration command from iidp.yaml: npx prisma migrate deploy"},
		{"a folded command", "migrationCommand: >\n  npm run migrate\n  && npm run seed\n", true, "npm run migrate && npm run seed", "npm run migrate && npm run seed"},
		{"no line", "# migrationCommand: npx prisma migrate deploy\n", true, "", "iidp.yaml sets no migration command"},
		{"an empty command", "migrationCommand: \"\"\n", true, "", "iidp.yaml sets no migration command"},
		{"a null command", "migrationCommand:\n", true, "", "iidp.yaml sets no migration command"},
		{"no iidp.yaml", "", false, "", "No iidp.yaml here"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			newFakeActionsTokenService(t)
			gate := newFakeGate(t, gateResponse{http.StatusOK, deployed})
			t.Setenv("IIDP_DEPLOY_GATE_URL", gate.srv.URL)
			dir := t.TempDir()
			if tc.file != "" {
				if err := os.WriteFile(filepath.Join(dir, "iidp.yaml"), []byte(tc.file), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			t.Chdir(dir)

			stdout, stderr, code := setImage(t, "shop", "auto", "abc123")
			if code != 0 || gate.callCount() != 1 {
				t.Fatalf("exit code = %d, gate calls = %d\n%s", code, gate.callCount(), stderr)
			}
			got, present := gate.calls[0].body["migrationCommand"]
			if present != tc.present || got != tc.want {
				t.Errorf("migrationCommand sent = %q (present: %v), want %q (present: %v)", got, present, tc.want, tc.present)
			}
			if !strings.Contains(stdout, tc.says) {
				t.Errorf("stdout = %q, want %q", stdout, tc.says)
			}
		})
	}
}

func TestCISetImageRefusesABadIidpYAMLBeforeAnyCall(t *testing.T) {
	for _, tc := range []struct {
		name, file, content, want string
	}{
		{"an unknown setting", "iidp.yaml", "migrationComand: npm run migrate\n", `unknown setting "migrationComand"`},
		{"a list", "iidp.yaml", "migrationCommand: [npm, run, migrate]\n", "must be a string"},
		{"two lines", "iidp.yaml", "migrationCommand: |\n  npm run migrate\n  npm run seed\n", "must be one line"},
		{"not YAML", "iidp.yaml", "migrationCommand: [\n", "not valid YAML"},
		{"the other spelling", "iidp.yml", "migrationCommand: npm run migrate\n", "rename it to iidp.yaml"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tokens := newFakeActionsTokenService(t)
			gate := newFakeGate(t, gateResponse{http.StatusOK, deployed})
			t.Setenv("IIDP_DEPLOY_GATE_URL", gate.srv.URL)
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, tc.file), []byte(tc.content), 0o644); err != nil {
				t.Fatal(err)
			}
			t.Chdir(dir)

			_, stderr, code := setImage(t, "shop", "auto", "abc123")
			if code == 0 || !strings.Contains(stderr, tc.want) {
				t.Errorf("exit code = %d, stderr = %q, want %q", code, stderr, tc.want)
			}
			if len(tokens.audiences) != 0 || gate.callCount() != 0 {
				t.Errorf("made %d token requests and %d gate calls, want none", len(tokens.audiences), gate.callCount())
			}
		})
	}
}

func TestCISetImageSaysWhenTheMigrationCommandChanged(t *testing.T) {
	newFakeActionsTokenService(t)
	gate := newFakeGate(t, gateResponse{http.StatusOK, `{"application":"shop","environment":"staging","tag":"abc123","file":"applications/shop/staging/values.yaml","commit":"0123456789abcdef0123","migrationCommandChanged":true}`})
	t.Setenv("IIDP_DEPLOY_GATE_URL", gate.srv.URL)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "iidp.yaml"), []byte("migrationCommand: npm run migrate\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	stdout, stderr, code := setImage(t, "shop", "auto", "abc123")
	if code != 0 || !strings.Contains(stdout, "The migration command changed with it") {
		t.Errorf("exit code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
}
