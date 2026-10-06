package e2e

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/Itema-as/iidp/internal/cli"
	"github.com/Itema-as/iidp/internal/dbtunnel/api"
)

// The bootstrap installs the Database tunnel; the harness builds its image
// from this working tree, as it does the Deploy gate's. iidp app db
// connect runs in-process, with the fake GitHub's developer token, and
// reaches the tunnel through Traefik's host port.
const (
	dbTunnelImage = "iidp-e2e.local/db-tunnel:dev"

	// DatabaseTunnelHost is the tunnel's host under the fixture's
	// baseDomain.
	DatabaseTunnelHost = "db.app.example.test"
	// DeveloperLogin is the GitHub login fakegithub gives DeveloperToken.
	DeveloperLogin = "e2e-developer"
)

// BuildDatabaseTunnelImage builds and loads the tunnel's image. It must
// run before the root Application, so the tunnel's first pod finds it.
func (c *Cluster) BuildDatabaseTunnelImage(ctx context.Context) error {
	c.Log("building the Database tunnel image with %s", c.Provider)
	return c.BuildLocalImage(ctx, dbTunnelImage, "./cmd/iidp-db-tunnel", "iidp-db-tunnel", filepath.Join("cmd", "iidp-db-tunnel", "Dockerfile"))
}

// dialTraefik connects to Traefik's websecure host port, whatever host a
// client asks for: the fixture's addresses resolve nowhere.
func (c *Cluster) dialTraefik(ctx context.Context, _, _ string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "tcp", fmt.Sprintf("127.0.0.1:%d", c.HTTPSPort))
}

// WaitForDatabaseTunnel polls the tunnel's health check through Traefik
// until it answers.
func (c *Cluster) WaitForDatabaseTunnel(ctx context.Context, timeout time.Duration) error {
	client := &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{DialContext: c.dialTraefik, TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, //nolint:gosec // kind has no real certificate to check
	}
	var last string
	return pollUntil(ctx, timeout, 3*time.Second,
		func() (bool, error) {
			resp, err := client.Get("https://" + DatabaseTunnelHost + "/healthz")
			if err != nil {
				last = err.Error()
				return false, nil
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			last = fmt.Sprintf("HTTP %d: %s", resp.StatusCode, body)
			return resp.StatusCode == http.StatusOK, nil
		},
		func() error {
			return fmt.Errorf("the Database tunnel at %s did not answer /healthz within %s: %s", DatabaseTunnelHost, timeout, last)
		})
}

// Connection is one iidp app db connect, running until Stop.
type Connection struct {
	// ConnString is the connection string it printed; "" when it exited
	// first.
	ConnString string
	stdout     *lockedBuffer
	stderr     *lockedBuffer
	done       chan int
	stop       context.CancelFunc
}

var connString = regexp.MustCompile(`postgresql://\S+`)

// DatabaseConnect runs iidp app db connect with args, as the fixture's
// developer, against the Platform repository at platformRepo, until it
// prints its connection string or exits.
func (c *Cluster) DatabaseConnect(ctx context.Context, platformRepo string, args ...string) (*Connection, error) {
	runCtx, stop := context.WithCancel(ctx)
	conn := &Connection{stdout: &lockedBuffer{}, stderr: &lockedBuffer{}, done: make(chan int, 1), stop: stop}
	deps := cli.Dependencies{
		TokenSource: staticToken(DeveloperToken),
		Context:     runCtx,
		DatabaseTunnel: func(url, token string) cli.DatabaseTunnel {
			return &api.Client{URL: url, Token: token, Dial: c.dialTraefik, TLSConfig: &tls.Config{InsecureSkipVerify: true}} //nolint:gosec // kind has no real certificate to check
		},
	}
	go func() {
		conn.done <- cli.RunWith(append([]string{"app", "db", "connect", "--platform-repo", platformRepo}, args...), strings.NewReader(""), conn.stdout, conn.stderr, deps)
	}()
	deadline := time.After(2 * time.Minute)
	for {
		if m := connString.FindString(conn.stdout.String()); m != "" {
			conn.ConnString = m
			return conn, nil
		}
		select {
		case code := <-conn.done:
			conn.done <- code
			return conn, nil
		case <-deadline:
			stop()
			return nil, fmt.Errorf("iidp app db connect %v printed no connection string within 2 minutes\nstdout:\n%s\nstderr:\n%s", args, conn.stdout, conn.stderr)
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// Stop ends the command, as Ctrl-C does, and returns its exit code and its
// output.
func (c *Connection) Stop() (code int, stdout, stderr string) {
	c.stop()
	code = <-c.done
	return code, c.stdout.String(), c.stderr.String()
}

type staticToken string

func (s staticToken) Token() (string, error) { return string(s), nil }

// lockedBuffer is a buffer the CLI writes while the harness reads it.
type lockedBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}
