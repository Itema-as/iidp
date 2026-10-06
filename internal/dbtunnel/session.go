package dbtunnel

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
)

// The session limits. They are constants, not settings.
const (
	// IdleLimit ends a session with no traffic in either direction.
	IdleLimit = 30 * time.Minute
	// MaxSession ends a session however busy it is.
	MaxSession = 8 * time.Hour
)

// startupTimeout bounds a check call, and a connection's startup: the
// client's startup packet, the check and the tunnel's own login.
const startupTimeout = time.Minute

// The request codes of the startup packets that are not a
// StartupMessage, and the protocol version the tunnel speaks.
const (
	cancelRequestCode   = 80877102
	sslRequestCode      = 80877103
	gssEncRequestCode   = 80877104
	protocolVersion3    = pgproto3.ProtocolVersion30
	maxStartupPacketLen = 10000
)

// forwardedParameters are the client's startup parameters the tunnel
// passes on to the database. The user and the database are the tunnel's
// own choice, and options or replication could ask for more than a
// session.
var forwardedParameters = []string{"application_name", "client_encoding", "DateStyle", "TimeZone", "IntervalStyle", "extra_float_digits", "search_path"}

// serve is one Postgres connection over client: the client's startup,
// the check, the tunnel's own login as the role the check chose, then the
// session.
func (t *Tunnel) serve(ctx context.Context, client net.Conn, req Request) {
	defer client.Close()
	_ = client.SetDeadline(time.Now().Add(startupTimeout))
	startup, err := readStartup(client)
	if err != nil {
		t.log().Info("a database connection ended before its startup", "application", req.Application, "environment", req.Environment, "error", err.Error())
		return
	}
	if cancel, ok := startup.(*pgproto3.CancelRequest); ok {
		t.cancel(ctx, req, cancel)
		return
	}
	message := startup.(*pgproto3.StartupMessage)

	checkCtx, cancelCheck := context.WithTimeout(ctx, startupTimeout)
	defer cancelCheck()
	grant, err := t.Check(checkCtx, req)
	if err != nil {
		t.refused(grant, "connect", err)
		code := "08004"
		var ref *Refusal
		if errors.As(err, &ref) && (ref.Status == http.StatusUnauthorized || ref.Status == http.StatusForbidden) {
			code = "28000"
		}
		writeMessages(client, &pgproto3.ErrorResponse{Severity: "FATAL", SeverityUnlocalized: "FATAL", Code: code, Message: err.Error()})
		return
	}
	server, err := t.login(checkCtx, grant, message.Parameters)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			t.refused(grant, "connect", fmt.Errorf("the database refused: %s", pgErr.Message))
			writeMessages(client, errorResponse(pgErr))
			return
		}
		t.log().Warn("the Database tunnel could not log in to the database", append(grant.logAttrs(), "error", err.Error())...)
		t.refused(grant, "connect", errors.New("the Database tunnel could not reach the database"))
		writeMessages(client, &pgproto3.ErrorResponse{Severity: "FATAL", SeverityUnlocalized: "FATAL", Code: "08006",
			Message: fmt.Sprintf("the Database tunnel could not reach %s %s's database; try again in a moment", grant.Application, grant.Environment)})
		return
	}
	defer server.Conn.Close()

	// The client is told it is in without being asked for a password:
	// whatever user and password it has are never used.
	messages := []pgproto3.BackendMessage{}
	if message.ProtocolVersion != protocolVersion3 {
		messages = append(messages, &pgproto3.NegotiateProtocolVersion{NewestMinorProtocol: 0, UnrecognizedOptions: unsupportedOptions(message.Parameters)})
	}
	messages = append(messages, &pgproto3.AuthenticationOk{})
	for name, value := range server.ParameterStatuses {
		messages = append(messages, &pgproto3.ParameterStatus{Name: name, Value: value})
	}
	messages = append(messages, &pgproto3.BackendKeyData{ProcessID: server.PID, SecretKey: server.SecretKey}, &pgproto3.ReadyForQuery{TxStatus: server.TxStatus})
	if err := writeMessages(client, messages...); err != nil {
		return
	}
	_ = client.SetDeadline(time.Time{})

	key := cancelKey(server.PID, server.SecretKey)
	t.live.add(key, grant)
	defer t.live.remove(key)
	t.started(grant)
	ended := Relay(ctx, client, server.Conn, t.clock())
	t.ended(grant, ended)
}

// readStartup reads the client's startup packet, answering an SSLRequest
// or GSSENCRequest with N: the WebSocket is already TLS. Each packet is
// read to its length and no further, so nothing the client sends after it
// is lost to a buffer.
func readStartup(client io.ReadWriter) (pgproto3.FrontendMessage, error) {
	for {
		var head [4]byte
		if _, err := io.ReadFull(client, head[:]); err != nil {
			return nil, err
		}
		n := binary.BigEndian.Uint32(head[:])
		if n < 8 || n > maxStartupPacketLen {
			return nil, fmt.Errorf("a startup packet of %d bytes", n)
		}
		body := make([]byte, n-4)
		if _, err := io.ReadFull(client, body); err != nil {
			return nil, err
		}
		switch code := binary.BigEndian.Uint32(body); {
		case code == sslRequestCode, code == gssEncRequestCode:
			if _, err := client.Write([]byte("N")); err != nil {
				return nil, err
			}
		case code == cancelRequestCode:
			msg := &pgproto3.CancelRequest{}
			return msg, msg.Decode(body)
		case code>>16 == 3:
			msg := &pgproto3.StartupMessage{}
			return msg, msg.Decode(body)
		default:
			return nil, fmt.Errorf("a startup packet for protocol %d.%d", code>>16, code&0xffff)
		}
	}
}

// unsupportedOptions are the protocol options (_pq_.*) a client asked
// for; the tunnel supports none.
func unsupportedOptions(parameters map[string]string) []string {
	options := []string{}
	for name := range parameters {
		if strings.HasPrefix(name, "_pq_.") {
			options = append(options, name)
		}
	}
	return options
}

// postgresAddress is where grant's database listens: its Cluster's -rw
// Service, or PostgresAddress's answer.
func (t *Tunnel) postgresAddress(g Grant) string {
	if t.PostgresAddress != nil {
		return t.PostgresAddress(g.Namespace, g.Cluster)
	}
	return net.JoinHostPort(g.Cluster+"-rw."+g.Namespace+".svc", "5432")
}

// login logs in to the Environment's database as the grant's role, with
// SCRAM-SHA-256 alone, and hands over the connection, idle and ready for
// the client's first query.
func (t *Tunnel) login(ctx context.Context, grant Grant, client map[string]string) (*pgconn.HijackedConn, error) {
	host, port, err := net.SplitHostPort(t.postgresAddress(grant))
	if err != nil {
		return nil, err
	}
	sslMode := t.PostgresSSLMode
	if sslMode == "" {
		sslMode = "require"
	}
	config, err := pgconn.ParseConfigWithOptions(fmt.Sprintf("host=%s port=%s dbname=%s user=%s sslmode=%s require_auth=scram-sha-256 max_protocol_version=3.0 connect_timeout=10",
		host, port, grant.Application, grant.Role, sslMode), pgconn.ParseConfigOptions{})
	if err != nil {
		return nil, err
	}
	config.Password = grant.Password
	for _, name := range forwardedParameters {
		if value, ok := client[name]; ok {
			config.RuntimeParams[name] = value
		}
	}
	conn, err := pgconn.ConnectConfig(ctx, config)
	if err != nil {
		return nil, err
	}
	if err := conn.SyncConn(ctx); err != nil {
		conn.Close(context.Background())
		return nil, err
	}
	return conn.Hijack()
}

// errorResponse is the database's own startup error, as the client would
// have had it from the database.
func errorResponse(e *pgconn.PgError) *pgproto3.ErrorResponse {
	return &pgproto3.ErrorResponse{
		Severity:            e.Severity,
		SeverityUnlocalized: e.SeverityUnlocalized,
		Code:                e.Code,
		Message:             e.Message,
		Detail:              e.Detail,
		Hint:                e.Hint,
		Where:               e.Where,
		File:                e.File,
		Line:                e.Line,
		Routine:             e.Routine,
	}
}

func writeMessages(w io.Writer, messages ...pgproto3.BackendMessage) error {
	var buf []byte
	for _, m := range messages {
		var err error
		if buf, err = m.Encode(buf); err != nil {
			return err
		}
	}
	_, err := w.Write(buf)
	return err
}

// Ended is how a session ended.
type Ended struct {
	// Reason is why, in words.
	Reason   string
	Duration time.Duration
	// FromClient and ToClient are the bytes copied each way.
	FromClient, ToClient int64
}

// The reasons a session ends.
const (
	EndedByClient   = "the client closed it"
	EndedByDatabase = "the database closed it"
	EndedIdle       = "30 minutes without traffic"
	EndedMaxSession = "the 8-hour limit"
	EndedShutdown   = "the Database tunnel stopped"
)

// Clock is what Relay times the limits with.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// Relay copies bytes both ways between a client and its database until
// either side closes, the session has had no traffic in either direction
// for IdleLimit, it has lasted MaxSession, or ctx ends. It closes both.
func Relay(ctx context.Context, client, server net.Conn, clock Clock) Ended {
	start := clock.Now()
	var last atomic.Int64
	last.Store(start.UnixNano())
	var fromClient, toClient atomic.Int64
	done := make(chan string, 2)
	copyOne := func(dst, src net.Conn, count *atomic.Int64, reason string) {
		buf := make([]byte, 32<<10)
		for {
			n, err := src.Read(buf)
			if n > 0 {
				last.Store(clock.Now().UnixNano())
				if _, werr := dst.Write(buf[:n]); werr != nil {
					done <- reason
					return
				}
				count.Add(int64(n))
			}
			if err != nil {
				done <- reason
				return
			}
		}
	}
	go copyOne(server, client, &fromClient, EndedByClient)
	go copyOne(client, server, &toClient, EndedByDatabase)

	reason := ""
	for reason == "" {
		now := clock.Now()
		idleAt := time.Unix(0, last.Load()).Add(IdleLimit)
		maxAt := start.Add(MaxSession)
		switch {
		case !now.Before(maxAt):
			reason = EndedMaxSession
			continue
		case !now.Before(idleAt):
			reason = EndedIdle
			continue
		}
		wake := idleAt
		if maxAt.Before(wake) {
			wake = maxAt
		}
		select {
		case reason = <-done:
		case <-ctx.Done():
			reason = EndedShutdown
		case <-clock.After(wake.Sub(now)):
		}
	}
	client.Close()
	server.Close()
	// The other direction stops once both are closed.
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
	return Ended{Reason: reason, Duration: clock.Now().Sub(start), FromClient: fromClient.Load(), ToClient: toClient.Load()}
}

// liveSessions are the open sessions by their cancel key, so that a
// CancelRequest reaches the database only for the developer's own
// session.
type liveSessions struct {
	mu       sync.Mutex
	sessions map[string]Grant
}

func (l *liveSessions) add(key string, grant Grant) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.sessions == nil {
		l.sessions = map[string]Grant{}
	}
	l.sessions[key] = grant
}

func (l *liveSessions) remove(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.sessions, key)
}

func (l *liveSessions) get(key string) (Grant, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	g, ok := l.sessions[key]
	return g, ok
}

func cancelKey(pid uint32, secret []byte) string {
	return fmt.Sprintf("%d/%x", pid, secret)
}

// cancel passes a CancelRequest, which a client sends on a connection of
// its own (psql's Ctrl-C), on to the database, after the same check as a
// session and only for a session of the same developer on the same
// Environment. Any other is dropped: the client gets no answer either
// way, as from Postgres itself.
func (t *Tunnel) cancel(ctx context.Context, req Request, msg *pgproto3.CancelRequest) {
	ctx, cancel := context.WithTimeout(ctx, startupTimeout)
	defer cancel()
	grant, err := t.Check(ctx, req)
	if err != nil {
		t.log().Info("a cancel request was dropped", append(grant.logAttrs(), "error", err.Error())...)
		return
	}
	session, ok := t.live.get(cancelKey(msg.ProcessID, msg.SecretKey))
	if !ok || session.Login != grant.Login || session.Application != grant.Application || session.Environment != grant.Environment {
		t.log().Info("a cancel request was dropped", append(grant.logAttrs(), "error", "it names no session of this developer on this Environment")...)
		return
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", t.postgresAddress(session))
	if err != nil {
		t.log().Warn("a cancel request could not reach the database", append(grant.logAttrs(), "error", err.Error())...)
		return
	}
	defer conn.Close()
	packet, err := msg.Encode(nil)
	if err == nil {
		_, err = conn.Write(packet)
	}
	if err != nil {
		t.log().Warn("a cancel request could not reach the database", append(grant.logAttrs(), "error", err.Error())...)
		return
	}
	// Postgres closes the connection once it has the request.
	_, _ = io.Copy(io.Discard, io.LimitReader(conn, 1))
	t.log().Info("a cancel request was passed on", session.logAttrs()...)
}
