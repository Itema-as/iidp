package dbtunnel_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"io"
	"net"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/Itema-as/iidp/internal/dbtunnel/api"
)

// The startup is tested at the Postgres client's boundary: bytes over the
// WebSocket iidp app db connect opens, as psql sends and reads them,
// through the tunnel's real HTTP service, to fakePostgres.

// serving starts the tunnel over TLS, logging in to db.
func (e *env) serving(t *testing.T, db *fakePostgres) *httptest.Server {
	t.Helper()
	e.tunnel.PostgresAddress = func(namespace, cluster string) string { return db.address() }
	e.tunnel.PostgresSSLMode = "disable"
	server := httptest.NewTLSServer(e.tunnel.Handler())
	t.Cleanup(func() {
		server.Close()
		e.tunnel.Shutdown()
	})
	return server
}

func client(server *httptest.Server, token string) *api.Client {
	return &api.Client{URL: server.URL, Token: token, TLSConfig: &tls.Config{InsecureSkipVerify: true}} //nolint:gosec // httptest's own certificate
}

// recorder keeps every byte the client has read.
type recorder struct {
	net.Conn
	read bytes.Buffer
}

func (r *recorder) Read(p []byte) (int, error) {
	n, err := r.Conn.Read(p)
	r.read.Write(p[:n])
	return n, err
}

// connect opens one connection through the tunnel and sends what psql
// sends with sslmode=prefer: an SSLRequest, then, once refused, a
// StartupMessage as the client's own user, which the tunnel must ignore.
func connect(t *testing.T, server *httptest.Server, token, environment string, readOnly bool) (*recorder, *pgproto3.Frontend) {
	t.Helper()
	ws, err := client(server, token).Connect(context.Background(), "shop", environment, readOnly)
	if err != nil {
		t.Fatal(err)
	}
	conn := &recorder{Conn: ws}
	t.Cleanup(func() { conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	sslRequest := binary.BigEndian.AppendUint32(binary.BigEndian.AppendUint32(nil, 8), 80877103)
	if _, err := conn.Write(sslRequest); err != nil {
		t.Fatal(err)
	}
	answer := make([]byte, 1)
	if _, err := io.ReadFull(conn, answer); err != nil || answer[0] != 'N' {
		t.Fatalf("the answer to an SSLRequest is %q, %v; want N", answer, err)
	}
	startup, _ := (&pgproto3.StartupMessage{ProtocolVersion: pgproto3.ProtocolVersion30, Parameters: map[string]string{
		"user": "mallory", "database": "postgres", "application_name": "psql", "search_path": "reporting, public", "options": "-c role=shop",
	}}).Encode(nil)
	if _, err := conn.Write(startup); err != nil {
		t.Fatal(err)
	}
	return conn, pgproto3.NewFrontend(conn, conn)
}

// receive reads backend messages until ReadyForQuery or an ErrorResponse.
func receive(t *testing.T, frontend *pgproto3.Frontend) []pgproto3.BackendMessage {
	t.Helper()
	var out []pgproto3.BackendMessage
	for {
		msg, err := frontend.Receive()
		if err != nil {
			t.Fatalf("after %d messages: %v", len(out), err)
		}
		// Receive reuses its messages, so keep a copy.
		data, _ := msg.Encode(nil)
		kept := copyMessage(t, data)
		out = append(out, kept)
		switch kept.(type) {
		case *pgproto3.ReadyForQuery, *pgproto3.ErrorResponse:
			return out
		}
	}
}

func copyMessage(t *testing.T, data []byte) pgproto3.BackendMessage {
	t.Helper()
	f := pgproto3.NewFrontend(bytes.NewReader(data), io.Discard)
	msg, err := f.Receive()
	if err != nil {
		t.Fatal(err)
	}
	return msg
}

func TestTheTunnelLogsInAsTheRoleAndIgnoresTheClientsUser(t *testing.T) {
	e := newEnv(t)
	db := newFakePostgres(t, map[string]string{"shop_write": previewWritePw})
	server := e.serving(t, db)

	conn, frontend := connect(t, server, pushToken, "staging", false)
	messages := receive(t, frontend)
	if _, ok := messages[0].(*pgproto3.AuthenticationOk); !ok {
		t.Fatalf("the first message is %T, want AuthenticationOk without a password asked for", messages[0])
	}
	var key *pgproto3.BackendKeyData
	params := map[string]string{}
	for _, m := range messages {
		switch m := m.(type) {
		case *pgproto3.ParameterStatus:
			params[m.Name] = m.Value
		case *pgproto3.BackendKeyData:
			key = m
		case *pgproto3.AuthenticationCleartextPassword, *pgproto3.AuthenticationSASL, *pgproto3.AuthenticationMD5Password:
			t.Errorf("the tunnel asked the client for a password: %T", m)
		}
	}
	if params["server_version"] != fakeServerVer || params["session_authorization"] != "shop_write" {
		t.Errorf("parameters = %v, want the database's own, logged in as shop_write", params)
	}
	if key == nil || key.ProcessID != fakePID || !bytes.Equal(key.SecretKey, fakeSecretKey) {
		t.Errorf("BackendKeyData = %+v, want the database's", key)
	}
	if _, ok := messages[len(messages)-1].(*pgproto3.ReadyForQuery); !ok {
		t.Errorf("the last message is %T, want ReadyForQuery", messages[len(messages)-1])
	}

	got := db.startup()
	if len(got) != 1 || got[0]["user"] != "shop_write" || got[0]["database"] != "shop" || got[0]["application_name"] != "psql" || got[0]["search_path"] != "reporting, public" {
		t.Errorf("the database saw startups %v, want one as shop_write to shop with the client's application_name and search_path", got)
	}
	if _, ok := got[0]["options"]; ok {
		t.Errorf("the client's options reached the database: %v", got[0])
	}

	// After the startup, bytes go both ways untouched.
	frontend.Send(&pgproto3.Query{String: "SELECT 'through the tunnel'"})
	if err := frontend.Flush(); err != nil {
		t.Fatal(err)
	}
	reply := receive(t, frontend)
	if row, ok := reply[1].(*pgproto3.DataRow); !ok || string(row.Values[0]) != "SELECT 'through the tunnel'" {
		t.Errorf("the query's answer = %v", reply)
	}
	if bytes.Contains(conn.read.Bytes(), []byte(previewWritePw)) {
		t.Error("the role's password reached the client")
	}
}

func TestTheTunnelPassesTheDatabasesStartupErrorThrough(t *testing.T) {
	e := newEnv(t)
	db := newFakePostgres(t, map[string]string{"shop_write": previewWritePw})
	db.setRefuse(&pgproto3.ErrorResponse{Severity: "FATAL", SeverityUnlocalized: "FATAL", Code: "53300", Message: `too many connections for role "shop_write"`})
	server := e.serving(t, db)

	_, frontend := connect(t, server, pushToken, "staging", false)
	messages := receive(t, frontend)
	refusal, ok := messages[len(messages)-1].(*pgproto3.ErrorResponse)
	if len(messages) != 1 || !ok || refusal.Code != "53300" || refusal.Severity != "FATAL" || refusal.Message != `too many connections for role "shop_write"` {
		t.Fatalf("messages = %+v, want only the database's own ErrorResponse", messages)
	}
}

func TestTheTunnelAnswersARefusalAsAnErrorResponse(t *testing.T) {
	e := newEnv(t)
	db := newFakePostgres(t, nil)
	server := e.serving(t, db)

	e.cluster.put(clusterPath("shop-prod", "shop-db"), clusterObject("admin", "none", role{"shop_write", "shop-db-write"}, role{"shop_read", ""}))
	_, frontend := connect(t, server, pullToken, "prod", false)
	messages := receive(t, frontend)
	refusal, ok := messages[0].(*pgproto3.ErrorResponse)
	if !ok || refusal.Severity != "FATAL" || refusal.Code != "28000" || !strings.Contains(refusal.Message, "your permission on Itema-as/shop is pull") {
		t.Fatalf("messages = %+v, want a FATAL 28000 naming the refusal", messages)
	}
	if got := db.startup(); len(got) != 0 {
		t.Errorf("a refused connection reached the database: %v", got)
	}
}

// psql sends Ctrl-C's CancelRequest on a connection of its own, with the
// BackendKeyData it got. It reaches the database for the developer's own
// session, and is dropped for anyone else's.
func TestACancelRequestReachesOnlyTheDevelopersOwnSession(t *testing.T) {
	e := newEnv(t)
	db := newFakePostgres(t, map[string]string{"shop_write": previewWritePw})
	server := e.serving(t, db)
	_, frontend := connect(t, server, pushToken, "staging", false)
	receive(t, frontend)

	cancel := func(token string) {
		t.Helper()
		ws, err := client(server, token).Connect(context.Background(), "shop", "staging", false)
		if err != nil {
			t.Fatal(err)
		}
		defer ws.Close()
		packet, _ := (&pgproto3.CancelRequest{ProcessID: fakePID, SecretKey: fakeSecretKey}).Encode(nil)
		if _, err := ws.Write(packet); err != nil {
			t.Fatal(err)
		}
		// The tunnel closes the connection once it is done with it, as
		// Postgres does.
		_ = ws.SetReadDeadline(time.Now().Add(30 * time.Second))
		if _, err := io.ReadAll(ws); err != nil {
			t.Fatalf("waiting for the tunnel to close the cancel's connection: %v", err)
		}
	}
	cancel(adminToken)
	if got := db.cancelRequests(); len(got) != 0 {
		t.Errorf("another developer's cancel reached the database: %+v", got)
	}
	cancel(pushToken)
	got := db.cancelRequests()
	if len(got) != 1 || got[0].ProcessID != fakePID || !bytes.Equal(got[0].SecretKey, fakeSecretKey) {
		t.Errorf("the database got cancel requests %+v, want the session's own", got)
	}
}
