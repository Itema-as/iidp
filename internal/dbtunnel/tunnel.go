// Package dbtunnel is the database tunnel: the part of the Platform
// through which a developer reaches an Environment's database from their
// own machine (ADR-0009). iidp app db connect opens one WebSocket per
// Postgres connection (internal/dbtunnel/api). On each, the tunnel checks
// the developer's permission on the Application repository with their own
// GitHub token, picks the role the Environment's access levels give them,
// and logs in to the database as that role itself, so no password leaves
// the cluster. It then copies bytes both ways until either side closes, or
// a limit ends the session, and records the session's start and end, and
// every refusal, as Kubernetes Events and log lines. It never logs SQL,
// query results, passwords or tokens.
package dbtunnel

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/websocket"

	"github.com/Itema-as/iidp/internal/dbtunnel/api"
	"github.com/Itema-as/iidp/internal/kubeevent"
)

// Tunnel is the database tunnel's HTTP service.
type Tunnel struct {
	// OrgID is the numeric id of the org every Application repository
	// must belong to.
	OrgID int64
	// PlatformRepo is the git URL of the Platform repository, which each
	// check clones with the developer's token.
	PlatformRepo string
	// GitHubAPI is the GitHub REST API root; "" means the real one.
	GitHubAPI string
	// Cluster reads the Environments' Clusters and their access roles'
	// password Secrets.
	Cluster Cluster
	// Events records sessions and refusals as Kubernetes Events; nil
	// records nothing.
	Events kubeevent.Sink
	// Log gets one line per session start, session end and refusal; nil
	// discards.
	Log *slog.Logger
	// PostgresAddress is where an Environment's database listens, as
	// host:port; nil means its Cluster's -rw Service on 5432.
	PostgresAddress func(namespace, cluster string) string
	// PostgresSSLMode is libpq's sslmode toward the database; "" means
	// require: encrypted, without checking the operator's own CA.
	PostgresSSLMode string
	// Clock times the session limits; nil means the real one.
	Clock Clock

	once     sync.Once
	stop     context.Context
	stopping context.CancelFunc
	// sessions counts the sessions open, and events the Events being
	// recorded in the background.
	sessions sync.WaitGroup
	events   sync.WaitGroup
	live     liveSessions
}

// Handler serves the tunnel's routes.
func (t *Tunnel) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("GET "+api.CheckPath+"{application}/{environment}", t.serveCheck)
	mux.HandleFunc("GET "+api.ConnectPath+"{application}/{environment}", t.serveConnect)
	return mux
}

// Check runs the tunnel's whole check for one connection.
func (t *Tunnel) Check(ctx context.Context, req Request) (Grant, error) {
	return t.check(ctx, req)
}

// Shutdown ends every open session, and waits until each has recorded
// its end.
func (t *Tunnel) Shutdown() {
	t.init()
	t.stopping()
	t.sessions.Wait()
	t.events.Wait()
}

// WaitForEvents waits until every Event the tunnel has started recording
// is recorded or has failed.
func (t *Tunnel) WaitForEvents() {
	t.events.Wait()
}

func (t *Tunnel) init() {
	t.once.Do(func() { t.stop, t.stopping = context.WithCancel(context.Background()) })
}

func requestOf(r *http.Request) Request {
	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return Request{
		Application: r.PathValue("application"),
		Environment: r.PathValue("environment"),
		ReadOnly:    r.URL.Query().Get(api.ReadOnlyParam) == "true",
		Token:       strings.TrimSpace(token),
	}
}

func (t *Tunnel) serveCheck(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	req := requestOf(r)
	ctx, cancel := context.WithTimeout(r.Context(), startupTimeout)
	defer cancel()
	grant, err := t.Check(ctx, req)
	if err != nil {
		status := http.StatusInternalServerError
		var ref *Refusal
		if errors.As(err, &ref) {
			status = ref.Status
		}
		t.refused(grant, "check", err)
		writeJSON(w, status, api.ErrorResponse{Error: err.Error()})
		return
	}
	t.log().Info("database access checked", "login", grant.Login, "application", grant.Application, "environment", grant.Environment,
		"role", grant.Role, "access", grant.Access, "duration", time.Since(start).Round(time.Millisecond).String())
	writeJSON(w, http.StatusOK, api.Grant{
		Login: grant.Login, Application: grant.Application, Environment: grant.Environment,
		Access: grant.Access, Role: grant.Role, Database: grant.Application,
	})
}

// serveConnect upgrades to the WebSocket that carries one Postgres
// connection. What the check refuses after the upgrade reaches the
// client as Postgres's own ErrorResponse; only a call without a token is
// refused before it.
func (t *Tunnel) serveConnect(w http.ResponseWriter, r *http.Request) {
	t.init()
	req := requestOf(r)
	if req.Token == "" {
		writeJSON(w, http.StatusUnauthorized, api.ErrorResponse{Error: "no GitHub token: send your own as Authorization: Bearer <token>, which iidp app db connect takes from gh auth"})
		return
	}
	if t.stop.Err() != nil {
		writeJSON(w, http.StatusServiceUnavailable, api.ErrorResponse{Error: "the database tunnel is stopping; connect again in a moment"})
		return
	}
	t.sessions.Add(1)
	defer t.sessions.Done()
	// Handshake is set so that x/net/websocket checks no Origin: the CLI
	// is not a browser, and the token is what authenticates.
	server := websocket.Server{
		Handshake: func(*websocket.Config, *http.Request) error { return nil },
		Handler: func(ws *websocket.Conn) {
			ws.PayloadType = websocket.BinaryFrame
			t.serve(t.stop, ws, req)
		},
	}
	server.ServeHTTP(w, r)
}

func (t *Tunnel) log() *slog.Logger {
	if t.Log == nil {
		return slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return t.Log
}

func (t *Tunnel) clock() Clock {
	if t.Clock == nil {
		return realClock{}
	}
	return t.Clock
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
