package dbtunnel_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/Itema-as/iidp/internal/dbtunnel"
	"github.com/Itema-as/iidp/internal/kubeevent"
	"github.com/Itema-as/iidp/internal/platformstate"
)

// The tunnel's check is tested against a fake GitHub (GET /user and GET
// /repositories/{id} with the developer's token), a local Platform
// repository, and fakeCluster, an in-memory stand-in for the Kubernetes
// API's get calls.

// The developers' tokens, by their permission on Itema-as/shop.
const (
	adminToken    = "gho_admin"
	maintainToken = "gho_maintain"
	pushToken     = "gho_push"
	triageToken   = "gho_triage"
	pullToken     = "gho_pull"
	strangerToken = "gho_stranger"
	revokedToken  = "gho_revoked"
)

const (
	orgID          = 1230559
	shopID         = 700000002
	shopPassword   = "s3cret-shop-write-password"
	readPassword   = "s3cret-shop-read-password"
	previewWritePw = "s3cret-staging-write-password"
)

// fakeGitHub answers GET /user and GET /repositories/{id} the way GitHub
// does for each token above.
func fakeGitHub(t *testing.T) string {
	t.Helper()
	permissions := map[string]map[string]bool{
		adminToken:    {"admin": true, "maintain": true, "push": true, "triage": true, "pull": true},
		maintainToken: {"maintain": true, "push": true, "triage": true, "pull": true},
		pushToken:     {"push": true, "triage": true, "pull": true},
		triageToken:   {"triage": true, "pull": true},
		pullToken:     {"pull": true},
	}
	token := func(r *http.Request) (string, bool) {
		token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		return token, token != revokedToken && token != ""
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /user", func(w http.ResponseWriter, r *http.Request) {
		token, ok := token(r)
		if !ok {
			http.Error(w, `{"message": "Bad credentials"}`, http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"login": strings.TrimPrefix(token, "gho_") + "-developer"})
	})
	mux.HandleFunc("GET /repositories/{id}", func(w http.ResponseWriter, r *http.Request) {
		token, ok := token(r)
		if !ok {
			http.Error(w, `{"message": "Bad credentials"}`, http.StatusUnauthorized)
			return
		}
		perms, ok := permissions[token]
		if !ok || r.PathValue("id") != strconv.Itoa(shopID) {
			http.Error(w, `{"message": "Not Found"}`, http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": shopID, "full_name": "Itema-as/shop", "permissions": perms})
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server.URL
}

// fakeCluster answers Get from the objects kept by path.
type fakeCluster struct {
	mu      sync.Mutex
	objects map[string]any
}

func (c *fakeCluster) Get(_ context.Context, path string, into any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	obj, ok := c.objects[path]
	if !ok {
		return &platformstate.StatusError{Path: path, Status: http.StatusNotFound, Body: `{"kind":"Status","reason":"NotFound"}`}
	}
	data, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, into)
}

func (c *fakeCluster) put(path string, obj any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.objects[path] = obj
}

func (c *fakeCluster) remove(path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.objects, path)
}

// role is one entry of a Cluster's spec.managed.roles, open with secret
// or, with secret "", absent.
type role struct{ name, secret string }

// clusterObject is a Cluster as the chart renders it, with the access
// levels as annotations.
func clusterObject(readWrite, readOnly string, roles ...role) map[string]any {
	var managed []map[string]any
	for _, r := range roles {
		if r.secret == "" {
			managed = append(managed, map[string]any{"name": r.name, "ensure": "absent"})
			continue
		}
		managed = append(managed, map[string]any{"name": r.name, "ensure": "present", "passwordSecret": map[string]any{"name": r.secret}})
	}
	annotations := map[string]any{}
	if readWrite != "" {
		annotations["iidp.itema.no/db-access-read-write"] = readWrite
		annotations["iidp.itema.no/db-access-read-only"] = readOnly
	}
	return map[string]any{
		"metadata": map[string]any{"annotations": annotations},
		"spec":     map[string]any{"managed": map[string]any{"roles": managed}},
	}
}

func secretObject(username, password string) map[string]any {
	return map[string]any{"type": "kubernetes.io/basic-auth", "data": map[string]any{
		"username": base64.StdEncoding.EncodeToString([]byte(username)),
		"password": base64.StdEncoding.EncodeToString([]byte(password)),
	}}
}

func clusterPath(namespace, name string) string {
	return "/apis/postgresql.cnpg.io/v1/namespaces/" + namespace + "/clusters/" + name
}

func secretPath(namespace, name string) string {
	return "/api/v1/namespaces/" + namespace + "/secrets/" + name
}

// fakeEvents keeps the Events the tunnel records.
type fakeEvents struct {
	mu     sync.Mutex
	events []kubeevent.Event
}

func (f *fakeEvents) CreateEvent(_ context.Context, e kubeevent.Event) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, e)
	return nil
}

func (f *fakeEvents) all() []kubeevent.Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]kubeevent.Event(nil), f.events...)
}

// env is a tunnel over shop, bound to Itema-as/shop, with prod and
// staging, and notes, bound outside the org:
//   - shop-prod: read-write admin and read-only pull, both open;
//   - shop-staging: read-write push and read-only pull, read-write open
//     and read-only not set up;
//   - shop-pr-7: a Preview Environment with staging's levels and Secrets.
type env struct {
	tunnel  *dbtunnel.Tunnel
	cluster *fakeCluster
	events  *fakeEvents
	logs    *bytes.Buffer
	repo    string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	setGitEnv(t)
	repo := newPlatformRepository(t)
	cluster := &fakeCluster{objects: map[string]any{
		clusterPath("shop-prod", "shop-db"):                 clusterObject("admin", "pull", role{"shop_write", "shop-db-write"}, role{"shop_read", "shop-db-read"}),
		secretPath("shop-prod", "shop-db-write"):            secretObject("shop_write", shopPassword),
		secretPath("shop-prod", "shop-db-read"):             secretObject("shop_read", readPassword),
		clusterPath("shop-staging", "shop-staging-db"):      clusterObject("push", "pull", role{"shop_write", "shop-staging-db-write"}, role{"shop_read", ""}),
		secretPath("shop-staging", "shop-staging-db-write"): secretObject("shop_write", previewWritePw),
		clusterPath("shop-pr-7", "shop-pr-7-db"):            clusterObject("push", "pull", role{"shop_write", "shop-staging-db-write"}, role{"shop_read", ""}),
		secretPath("shop-pr-7", "shop-staging-db-write"):    secretObject("shop_write", previewWritePw),
	}}
	events := &fakeEvents{}
	logs := &bytes.Buffer{}
	return &env{
		tunnel: &dbtunnel.Tunnel{
			OrgID:        orgID,
			PlatformRepo: repo,
			GitHubAPI:    fakeGitHub(t),
			Cluster:      cluster,
			Events:       events,
			Log:          slog.New(slog.NewJSONHandler(&syncWriter{w: logs}, nil)),
		},
		cluster: cluster,
		events:  events,
		logs:    logs,
		repo:    repo,
	}
}

func (e *env) check(t *testing.T, token, application, environment string, readOnly bool) (dbtunnel.Grant, error) {
	t.Helper()
	return e.tunnel.Check(context.Background(), dbtunnel.Request{Application: application, Environment: environment, ReadOnly: readOnly, Token: token})
}

// syncWriter serialises the log handler's writes with the test's reads.
type syncWriter struct {
	mu sync.Mutex
	w  *bytes.Buffer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

func newPlatformRepository(t *testing.T) string {
	t.Helper()
	bare := filepath.Join(t.TempDir(), "iidp-platform.git")
	gitRun(t, t.TempDir(), "init", "--bare", "--initial-branch=main", bare)
	url := "file://" + bare
	seed := filepath.Join(t.TempDir(), "seed")
	gitRun(t, t.TempDir(), "clone", "--quiet", url, seed)
	writeFile(t, filepath.Join(seed, "platform.yaml"), "baseDomain: app.example.test\nchartVersion: 0.3.1\n")
	for name, binding := range map[string]string{
		"shop":  fmt.Sprintf("repository: Itema-as/shop\nrepositoryId: %d\nrepositoryOwnerId: %d\n", shopID, orgID),
		"notes": "repository: someone-else/notes\nrepositoryId: 800000001\nrepositoryOwnerId: 999\n",
		"later": "",
	} {
		for _, environment := range []string{"prod", "staging"} {
			if name != "shop" && environment == "staging" {
				continue
			}
			base := filepath.Join(seed, "applications", name, environment)
			writeFile(t, filepath.Join(base, "application.yaml"), "apiVersion: argoproj.io/v1alpha1\nkind: Application\n")
			writeFile(t, filepath.Join(base, "values.yaml"), fmt.Sprintf("application:\n  name: %s\nenvironment: %s\npostgres:\n  enabled: %v\n", name, environment, name == "shop"))
		}
		if binding != "" {
			writeFile(t, filepath.Join(seed, "applications", name, "repository.yaml"), binding)
		}
	}
	gitRun(t, seed, "add", "-A")
	gitRun(t, seed, "commit", "-q", "-m", "Seed the Platform repository")
	gitRun(t, seed, "push", "-q", "origin", "HEAD:main")
	return url
}

// setGitEnv isolates git from the developer's own configuration and gives
// the test's commits an identity.
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
