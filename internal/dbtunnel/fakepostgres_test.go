package dbtunnel_test

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgproto3"
)

// fakePostgres is an in-process Postgres that speaks enough of the
// protocol for the tunnel: a StartupMessage, SCRAM-SHA-256 against the
// roles' passwords, the post-auth messages, and then a simple query, which
// it answers with one row holding the query's text. It refuses a startup
// with refuse when that is set, the way Postgres refuses a role over its
// connection limit.
type fakePostgres struct {
	t         *testing.T
	listener  net.Listener
	passwords map[string]string

	mu       sync.Mutex
	refuse   *pgproto3.ErrorResponse
	startups []map[string]string
	cancels  []*pgproto3.CancelRequest
	conns    []net.Conn
}

const (
	fakePID       = 4242
	fakeServerVer = "18.4"
)

var fakeSecretKey = []byte{0xde, 0xad, 0xbe, 0xef}

func newFakePostgres(t *testing.T, passwords map[string]string) *fakePostgres {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &fakePostgres{t: t, listener: l, passwords: passwords}
	t.Cleanup(func() {
		l.Close()
		p.mu.Lock()
		defer p.mu.Unlock()
		for _, c := range p.conns {
			c.Close()
		}
	})
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			p.mu.Lock()
			p.conns = append(p.conns, conn)
			p.mu.Unlock()
			go p.serve(conn)
		}
	}()
	return p
}

func (p *fakePostgres) address() string { return p.listener.Addr().String() }

func (p *fakePostgres) setRefuse(e *pgproto3.ErrorResponse) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.refuse = e
}

func (p *fakePostgres) startup() []map[string]string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]map[string]string(nil), p.startups...)
}

func (p *fakePostgres) cancelRequests() []*pgproto3.CancelRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]*pgproto3.CancelRequest(nil), p.cancels...)
}

// closeSessions closes every connection, as a restarting database would.
func (p *fakePostgres) closeSessions() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.conns {
		c.Close()
	}
}

func (p *fakePostgres) serve(conn net.Conn) {
	defer conn.Close()
	backend := pgproto3.NewBackend(conn, conn)
	msg, err := backend.ReceiveStartupMessage()
	if err != nil {
		return
	}
	if cancel, ok := msg.(*pgproto3.CancelRequest); ok {
		p.mu.Lock()
		p.cancels = append(p.cancels, cancel)
		p.mu.Unlock()
		return
	}
	startup, ok := msg.(*pgproto3.StartupMessage)
	if !ok {
		return
	}
	p.mu.Lock()
	p.startups = append(p.startups, startup.Parameters)
	refuse := p.refuse
	p.mu.Unlock()
	send := func(msgs ...pgproto3.BackendMessage) {
		for _, m := range msgs {
			backend.Send(m)
		}
		_ = backend.Flush()
	}
	if refuse != nil {
		send(refuse)
		return
	}
	user := startup.Parameters["user"]
	if err := p.scram(backend, send, p.passwords[user]); err != nil {
		send(&pgproto3.ErrorResponse{Severity: "FATAL", SeverityUnlocalized: "FATAL", Code: "28P01", Message: fmt.Sprintf("password authentication failed for user %q", user)})
		return
	}
	send(&pgproto3.AuthenticationOk{},
		&pgproto3.ParameterStatus{Name: "server_version", Value: fakeServerVer},
		&pgproto3.ParameterStatus{Name: "client_encoding", Value: "UTF8"},
		&pgproto3.ParameterStatus{Name: "session_authorization", Value: user},
		&pgproto3.BackendKeyData{ProcessID: fakePID, SecretKey: fakeSecretKey},
		&pgproto3.ReadyForQuery{TxStatus: 'I'})
	for {
		msg, err := backend.Receive()
		if err != nil {
			return
		}
		switch m := msg.(type) {
		case *pgproto3.Query:
			send(&pgproto3.RowDescription{Fields: []pgproto3.FieldDescription{{Name: []byte("echo"), DataTypeOID: 25, DataTypeSize: -1, TypeModifier: -1}}},
				&pgproto3.DataRow{Values: [][]byte{[]byte(m.String)}},
				&pgproto3.CommandComplete{CommandTag: []byte("SELECT 1")},
				&pgproto3.ReadyForQuery{TxStatus: 'I'})
		case *pgproto3.Terminate:
			return
		}
	}
}

// scram is the server's side of SCRAM-SHA-256 (RFC 5802, RFC 7677), as
// Postgres runs it: the client's user name in the exchange is ignored.
func (p *fakePostgres) scram(backend *pgproto3.Backend, send func(...pgproto3.BackendMessage), password string) error {
	send(&pgproto3.AuthenticationSASL{AuthMechanisms: []string{"SCRAM-SHA-256"}})
	if err := backend.SetAuthType(pgproto3.AuthTypeSASL); err != nil {
		return err
	}
	msg, err := backend.Receive()
	if err != nil {
		return err
	}
	initial, ok := msg.(*pgproto3.SASLInitialResponse)
	if !ok || initial.AuthMechanism != "SCRAM-SHA-256" {
		return errors.New("no SCRAM-SHA-256 initial response")
	}
	clientFirst := string(initial.Data)
	gs2, clientFirstBare, ok := cutAfterSecondComma(clientFirst)
	if !ok {
		return errors.New("a malformed client-first-message")
	}
	clientNonce := attribute(clientFirstBare, "r")
	nonce := clientNonce + rand.Text()
	salt := []byte("fake-postgres-salt")
	const iterations = 4096
	serverFirst := fmt.Sprintf("r=%s,s=%s,i=%d", nonce, base64.StdEncoding.EncodeToString(salt), iterations)
	send(&pgproto3.AuthenticationSASLContinue{Data: []byte(serverFirst)})
	if err := backend.SetAuthType(pgproto3.AuthTypeSASLContinue); err != nil {
		return err
	}
	msg, err = backend.Receive()
	if err != nil {
		return err
	}
	response, ok := msg.(*pgproto3.SASLResponse)
	if !ok {
		return errors.New("no SCRAM client-final-message")
	}
	clientFinal := string(response.Data)
	withoutProof, proofText, ok := strings.Cut(clientFinal, ",p=")
	if !ok || attribute(clientFinal, "r") != nonce || attribute(clientFinal, "c") != base64.StdEncoding.EncodeToString([]byte(gs2)) {
		return errors.New("a client-final-message for another exchange")
	}
	salted, err := pbkdf2.Key(sha256.New, password, salt, iterations, sha256.Size)
	if err != nil {
		return err
	}
	clientKey := hmacSHA256(salted, "Client Key")
	storedKey := sha256.Sum256(clientKey)
	authMessage := clientFirstBare + "," + serverFirst + "," + withoutProof
	signature := hmacSHA256(storedKey[:], authMessage)
	proof, err := base64.StdEncoding.DecodeString(proofText)
	if err != nil || len(proof) != len(signature) {
		return errors.New("a malformed proof")
	}
	for i := range proof {
		proof[i] ^= signature[i]
	}
	if got := sha256.Sum256(proof); !hmac.Equal(got[:], storedKey[:]) {
		return errors.New("a wrong password")
	}
	serverSignature := hmacSHA256(hmacSHA256(salted, "Server Key"), authMessage)
	send(&pgproto3.AuthenticationSASLFinal{Data: []byte("v=" + base64.StdEncoding.EncodeToString(serverSignature))})
	return nil
}

func hmacSHA256(key []byte, message string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(message))
	return mac.Sum(nil)
}

// cutAfterSecondComma splits a client-first-message into its GS2 header,
// commas included, and the rest.
func cutAfterSecondComma(s string) (string, string, bool) {
	first := strings.IndexByte(s, ',')
	if first < 0 {
		return "", "", false
	}
	second := strings.IndexByte(s[first+1:], ',')
	if second < 0 {
		return "", "", false
	}
	cut := first + 1 + second + 1
	return s[:cut], s[cut:], true
}

// attribute is the value of name= in a SCRAM message.
func attribute(message, name string) string {
	for _, part := range strings.Split(message, ",") {
		if v, ok := strings.CutPrefix(part, name+"="); ok {
			return v
		}
	}
	return ""
}
