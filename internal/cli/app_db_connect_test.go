package cli_test

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Itema-as/iidp/internal/cli"
	"github.com/Itema-as/iidp/internal/dbtunnel/api"
)

// fakeTunnel stands in for the database tunnel: a check answers grant or
// refusal, and each connection is an echo of what the client sends.
type fakeTunnel struct {
	url, token string
	grant      api.Grant
	refusal    error

	mu       sync.Mutex
	checks   []string
	connects []string
}

func (f *fakeTunnel) Check(_ context.Context, application, environment string, readOnly bool) (api.Grant, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checks = append(f.checks, call(application, environment, readOnly))
	if f.refusal != nil {
		return api.Grant{}, f.refusal
	}
	return f.grant, nil
}

func (f *fakeTunnel) Connect(_ context.Context, application, environment string, readOnly bool) (net.Conn, error) {
	f.mu.Lock()
	f.connects = append(f.connects, call(application, environment, readOnly))
	f.mu.Unlock()
	client, tunnel := net.Pipe()
	go func() {
		defer tunnel.Close()
		_, _ = io.Copy(tunnel, tunnel)
	}()
	return client, nil
}

func (f *fakeTunnel) calls() (checks, connects []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.checks...), append([]string(nil), f.connects...)
}

func call(application, environment string, readOnly bool) string {
	if readOnly {
		return application + " " + environment + " read-only"
	}
	return application + " " + environment
}

func stagingGrant() api.Grant {
	return api.Grant{Login: "octocat", Application: "shop", Environment: "staging", Access: "read-write", Role: "shop_write", Database: "shop"}
}

// syncBuffer is a bytes.Buffer the test reads while the CLI writes it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type connectRun struct {
	stdout, stderr *syncBuffer
	done           chan int
	stop           context.CancelFunc
}

// dbConnect runs iidp app db connect against tunnel until stop is called,
// as Ctrl-C does.
func dbConnect(t *testing.T, tunnel *fakeTunnel, args ...string) *connectRun {
	t.Helper()
	return dbConnectTo(t, newPlatformRepository(t, statusPlatformYAML), tunnel, args...)
}

// dbConnectTo is dbConnect with the Platform repository at url.
func dbConnectTo(t *testing.T, url string, tunnel *fakeTunnel, args ...string) *connectRun {
	t.Helper()
	ctx, stop := context.WithCancel(context.Background())
	run := &connectRun{stdout: &syncBuffer{}, stderr: &syncBuffer{}, done: make(chan int, 1), stop: stop}
	deps := cli.Dependencies{
		TokenSource: fakeTokenSource{token: "gho_developer"},
		Context:     ctx,
		DatabaseTunnel: func(url, token string) cli.DatabaseTunnel {
			tunnel.url, tunnel.token = url, token
			return tunnel
		},
	}
	go func() {
		run.done <- cli.RunWith(append([]string{"app", "db", "connect", "--platform-repo", url}, args...), strings.NewReader(""), run.stdout, run.stderr, deps)
	}()
	t.Cleanup(stop)
	return run
}

func (r *connectRun) exitCode(t *testing.T) int {
	t.Helper()
	select {
	case code := <-r.done:
		return code
	case <-time.After(30 * time.Second):
		t.Fatalf("iidp app db connect did not exit\nstdout:\n%s\nstderr:\n%s", r.stdout, r.stderr)
		return -1
	}
}

var connectionString = regexp.MustCompile(`postgresql://(\w+)@127\.0\.0\.1:(\d+)/(\w+)\?sslmode=disable`)

// address waits for the connection string and returns its host and port.
func (r *connectRun) address(t *testing.T) string {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if m := connectionString.FindStringSubmatch(r.stdout.String()); m != nil {
			return "127.0.0.1:" + m[2]
		}
	}
	t.Fatalf("no connection string printed\nstdout:\n%s\nstderr:\n%s", r.stdout, r.stderr)
	return ""
}

func TestDBConnectPrintsAConnectionStringAndOpensAWebSocketPerConnection(t *testing.T) {
	tunnel := &fakeTunnel{grant: stagingGrant()}
	run := dbConnect(t, tunnel, "shop")
	address := run.address(t)

	out := run.stdout.String()
	if !strings.Contains(out, "postgresql://shop_write@"+address+"/shop?sslmode=disable") {
		t.Errorf("stdout = %q, want the role, the address and the database in the connection string", out)
	}
	if !strings.Contains(out, "shop staging, read-write as shop_write") {
		t.Errorf("stdout = %q, want the Environment and the level", out)
	}
	if tunnel.url != "https://db.app.example.test" || tunnel.token != "gho_developer" {
		t.Errorf("the tunnel is %s with token %q, want https://db.<baseDomain> from platform.yaml and the gh auth token", tunnel.url, tunnel.token)
	}

	for _, msg := range []string{"first connection", "second connection"} {
		conn, err := net.Dial("tcp", address)
		if err != nil {
			t.Fatal(err)
		}
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
		if _, err := conn.Write([]byte(msg)); err != nil {
			t.Fatal(err)
		}
		got := make([]byte, len(msg))
		if _, err := io.ReadFull(conn, got); err != nil || string(got) != msg {
			t.Errorf("%s: read %q, %v back through the tunnel", msg, got, err)
		}
		conn.Close()
	}
	checks, connects := tunnel.calls()
	// The check asks for the default Environment; the connections for the
	// one it resolved.
	if strings.Join(checks, ",") != "shop auto" || strings.Join(connects, ",") != "shop staging,shop staging" {
		t.Errorf("checks %v, connects %v; want one check of auto and a connection each for staging", checks, connects)
	}

	run.stop()
	if code := run.exitCode(t); code != 0 {
		t.Errorf("exit code after Ctrl-C = %d, want 0\nstderr:\n%s", code, run.stderr)
	}
}

func TestDBConnectAsksForThePreviewOrEnvironmentAndReadOnly(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"shop", "--pr", "7"}, "shop pr-7"},
		{[]string{"shop", "--env", "prod", "--read-only"}, "shop prod read-only"},
	} {
		tunnel := &fakeTunnel{grant: stagingGrant()}
		run := dbConnect(t, tunnel, tc.args...)
		run.address(t)
		run.stop()
		run.exitCode(t)
		if checks, _ := tunnel.calls(); strings.Join(checks, ",") != tc.want {
			t.Errorf("%v: checks %v, want %s", tc.args, checks, tc.want)
		}
	}
}

func TestDBConnectExitsWithTheTunnelsRefusal(t *testing.T) {
	tunnel := &fakeTunnel{refusal: &api.Refused{Status: 409, Message: "no database access set up for prod yet, run `iidp app db access shop --env prod`"}}
	run := dbConnect(t, tunnel, "shop", "--env", "prod")
	if code := run.exitCode(t); code == 0 {
		t.Fatal("a refused check exits 0")
	}
	if !strings.Contains(run.stderr.String(), "no database access set up for prod yet, run `iidp app db access shop --env prod`") {
		t.Errorf("stderr = %q, want the tunnel's message", run.stderr)
	}
	if strings.Contains(run.stdout.String(), "postgresql://") {
		t.Errorf("a refused check printed a connection string: %q", run.stdout)
	}
	if _, connects := tunnel.calls(); len(connects) != 0 {
		t.Errorf("a refused check opened connections: %v", connects)
	}
}

func TestDBConnectRefusesEnvAndPRTogether(t *testing.T) {
	tunnel := &fakeTunnel{grant: stagingGrant()}
	run := dbConnect(t, tunnel, "shop", "--env", "staging", "--pr", "7")
	if code := run.exitCode(t); code == 0 || !strings.Contains(run.stderr.String(), "--env and --pr") {
		t.Errorf("exit code %d, stderr %q; want --env and --pr refused together", code, run.stderr)
	}
	if checks, _ := tunnel.calls(); len(checks) != 0 {
		t.Errorf("the tunnel was asked anyway: %v", checks)
	}
}

// fakePsql puts a psql on PATH that writes its arguments to a file and
// exits with status.
func fakePsql(t *testing.T, status string) (argsFile string) {
	t.Helper()
	dir := t.TempDir()
	argsFile = filepath.Join(dir, "args")
	script := "#!/bin/sh\necho \"$@\" > " + argsFile + "\nexit " + status + "\n"
	if err := os.WriteFile(filepath.Join(dir, "psql"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return argsFile
}

func TestDBConnectPsqlRunsPsqlAndExitsWithItsStatus(t *testing.T) {
	for _, status := range []string{"0", "3"} {
		argsFile := fakePsql(t, status)
		run := dbConnect(t, &fakeTunnel{grant: stagingGrant()}, "shop", "--psql")
		code := run.exitCode(t)
		if want := map[string]int{"0": 0, "3": 3}[status]; code != want {
			t.Errorf("psql exiting %s: exit code %d, want %d\nstderr:\n%s", status, code, want, run.stderr)
		}
		args, err := os.ReadFile(argsFile)
		if err != nil || !connectionString.Match(args) {
			t.Errorf("psql got %q (%v), want the connection string", args, err)
		}
	}
}

func TestDBConnectPsqlSaysWhenPsqlIsMissing(t *testing.T) {
	// A PATH with git, which reading platform.yaml needs, and no psql.
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	if err := os.Symlink(git, filepath.Join(dir, "git")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	tunnel := &fakeTunnel{grant: stagingGrant()}
	run := dbConnect(t, tunnel, "shop", "--psql")
	if code := run.exitCode(t); code == 0 {
		t.Fatal("a missing psql exits 0")
	}
	if stderr := run.stderr.String(); !strings.Contains(stderr, "psql is not on your PATH") || !strings.Contains(stderr, "leave out --psql") {
		t.Errorf("stderr = %q, want it to say psql is missing and to leave out --psql", stderr)
	}
}

// Without platform.yaml, iidp app db connect says so and asks Itema's
// Platform's Database tunnel, which refuses for itself whoever may not
// connect, as iidp app status does with the Deploy gate.
func TestDBConnectFallsBackToTheDefaultTunnelWithoutPlatformYAML(t *testing.T) {
	tunnel := &fakeTunnel{grant: stagingGrant()}
	run := dbConnectTo(t, "file://"+filepath.Join(t.TempDir(), "missing.git"), tunnel, "shop")
	run.address(t)
	run.stop()
	run.exitCode(t)
	if tunnel.url != "https://db.app.itma.no" {
		t.Errorf("the tunnel is %q, want Itema's, platform.DefaultDatabaseTunnelURL", tunnel.url)
	}
	if !strings.Contains(run.stderr.String(), "Could not read platform.yaml, so asking the Database tunnel at https://db.app.itma.no") {
		t.Errorf("stderr = %q, want it to say why it asks the default tunnel", run.stderr)
	}
}
