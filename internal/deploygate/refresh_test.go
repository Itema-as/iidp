package deploygate_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Itema-as/iidp/internal/deploygate"
	"github.com/Itema-as/iidp/internal/platformstate"
)

// fakeArgoCD stands in for the Kubernetes API's merge patch and records
// what it was sent.
type fakeArgoCD struct {
	mu      sync.Mutex
	patches []string // path + " " + patch
	err     error
}

func (f *fakeArgoCD) MergePatch(ctx context.Context, path string, patch []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	f.patches = append(f.patches, path+" "+string(patch))
	return f.err
}

func (f *fakeArgoCD) sent() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.patches...)
}

// syncBuffer is a log destination the test reads while the gate writes.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

const refreshPatch = `{"metadata":{"annotations":{"argocd.argoproj.io/refresh":"normal"}}}`

func refreshOf(name string) string {
	return "/apis/argoproj.io/v1alpha1/namespaces/argocd/applications/" + name + " " + refreshPatch
}

func TestTheGateAsksArgoCDToRefreshAfterADeployAndAPromote(t *testing.T) {
	e := newEnv(t)
	argocd := &fakeArgoCD{}
	e.gate.ArgoCD = argocd
	addApplication(t, e.platform, "shop", true, binding(shopRepoID, orgID))

	// main deploys to staging.
	if status, body := e.deploy(e.issuer.claims(), "shop", "auto", shaDeployed); status != http.StatusOK || body["environment"] != "staging" {
		t.Fatalf("deploy = %d %v, want staging written", status, body)
	}
	if got, want := strings.Join(argocd.sent(), "\n"), refreshOf("shop-staging"); got != want {
		t.Errorf("after the deploy the gate sent:\n%s\nwant:\n%s", got, want)
	}

	// A v* tag promotes to prod.
	claims := e.issuer.claims()
	claims["ref"], claims["ref_type"] = "refs/tags/v1.2.3", "tag"
	if status, body := e.deploy(claims, "shop", "auto", "1.2.3"); status != http.StatusOK || body["environment"] != "prod" {
		t.Fatalf("promote = %d %v, want prod written", status, body)
	}
	if got, want := strings.Join(argocd.sent(), "\n"), refreshOf("shop-staging")+"\n"+refreshOf("shop-prod"); got != want {
		t.Errorf("after the promote the gate sent:\n%s\nwant:\n%s", got, want)
	}
}

func TestTheGateDoesNotAskForARefreshWithoutACommit(t *testing.T) {
	e := newEnv(t)
	argocd := &fakeArgoCD{}
	e.gate.ArgoCD = argocd
	addApplication(t, e.platform, "shop", false, binding(shopRepoID, orgID))

	if status, body := e.deploy(e.issuer.claims(), "shop", "prod", shaDeployed); status != http.StatusOK {
		t.Fatalf("deploy = %d %v", status, body)
	}
	// prod already runs the tag: nothing is committed, so nothing to refresh.
	if status, body := e.deploy(e.issuer.claims(), "shop", "prod", shaDeployed); status != http.StatusOK || body["unchanged"] != true {
		t.Fatalf("repeated deploy = %d %v, want 200 and unchanged", status, body)
	}
	// A refused deploy commits nothing either.
	claims := e.issuer.claims()
	claims["ref"] = "refs/heads/feature"
	status, body := e.deploy(claims, "shop", "prod", "other")
	if status != http.StatusForbidden {
		t.Fatalf("deploy from a feature branch = %d %v, want 403", status, body)
	}
	if got := argocd.sent(); len(got) != 1 {
		t.Errorf("the gate sent %d refreshes, want 1, for the one commit: %v", len(got), got)
	}
}

func TestAFailedRefreshDoesNotFailTheDeploy(t *testing.T) {
	e := newEnv(t)
	argocd := &fakeArgoCD{err: &platformstate.StatusError{Path: deploygate.ArgoCDApplicationPath("shop-prod"), Status: http.StatusForbidden, Body: "forbidden"}}
	e.gate.ArgoCD = argocd
	var logs syncBuffer
	e.gate.Log = slog.New(slog.NewTextHandler(&logs, nil))
	addApplication(t, e.platform, "shop", false, binding(shopRepoID, orgID))

	status, body := e.deploy(e.issuer.claims(), "shop", "auto", shaDeployed)
	if status != http.StatusOK || body["commit"] == "" || body["commit"] == nil {
		t.Fatalf("deploy = %d %v, want 200 with the commit", status, body)
	}
	if got := headSubject(t, e.platform); got != "Deploy shop prod "+shaDeployed {
		t.Errorf("head = %q, want the deploy committed", got)
	}
	if len(argocd.sent()) != 1 {
		t.Errorf("refreshes asked for = %v, want one", argocd.sent())
	}
	out := logs.String()
	for _, want := range []string{"level=WARN", "argocd refresh failed", "argocd_application=shop-prod", "HTTP 403"} {
		if !strings.Contains(out, want) {
			t.Errorf("the log does not mention %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, `msg=deployed`) {
		t.Errorf("the deploy is not logged as deployed:\n%s", out)
	}
}

// KubePatcher sends the patch the way the API server takes a merge patch,
// as the pod's service account, and reports a refusal.
func TestKubePatcherSendsAMergePatchWithTheServiceAccountToken(t *testing.T) {
	var method, path, contentType, auth, body string
	answer := http.StatusOK
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		method, path, contentType, auth, body = r.Method, r.URL.Path, r.Header.Get("Content-Type"), r.Header.Get("Authorization"), string(data)
		if answer != http.StatusOK {
			http.Error(w, `{"kind":"Status","reason":"Forbidden"}`, answer)
			return
		}
		_, _ = w.Write([]byte(`{"kind":"Application"}`))
	}))
	defer api.Close()
	patcher := deploygate.KubePatcher{Kube: &platformstate.Kube{BaseURL: api.URL, Token: func() (string, error) { return "sa-token", nil }}}

	appPath := deploygate.ArgoCDApplicationPath("shop-prod")
	if err := patcher.MergePatch(context.Background(), appPath, []byte(refreshPatch)); err != nil {
		t.Fatal(err)
	}
	if method != http.MethodPatch || path != "/apis/argoproj.io/v1alpha1/namespaces/argocd/applications/shop-prod" ||
		contentType != "application/merge-patch+json" || auth != "Bearer sa-token" || body != refreshPatch {
		t.Errorf("sent %s %s, Content-Type %q, Authorization %q, body %s", method, path, contentType, auth, body)
	}

	answer = http.StatusForbidden
	err := patcher.MergePatch(context.Background(), appPath, []byte(refreshPatch))
	var refusal *platformstate.StatusError
	if !errors.As(err, &refusal) || refusal.Status != http.StatusForbidden {
		t.Errorf("MergePatch = %v, want the API server's 403", err)
	}
}
