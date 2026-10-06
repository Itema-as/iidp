// Package api is the database tunnel's contract with iidp app db connect:
// its two calls, what they answer, and a client for them. It is apart from
// internal/dbtunnel so that the CLI does not link the tunnel's Postgres
// driver.
//
// Both calls name the Application and the Environment in the path, take
// the read-only flag as a query parameter, and carry the developer's
// gh auth token:
//
//	GET /v1/check/<app>/<environment>[?readOnly=true]
//	GET /v1/connect/<app>/<environment>[?readOnly=true]
//	Authorization: Bearer <the developer's GitHub token>
//
// <environment> is prod, staging, pr-<pull request number>, or auto:
// staging when the Application has one, else prod. Check runs the
// tunnel's whole check once and answers 200 with a Grant, or an
// ErrorResponse. Connect upgrades to a WebSocket that carries one Postgres
// connection in binary messages, and runs the same check on it; a refusal
// then reaches the Postgres client as an ErrorResponse.
package api

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/websocket"
)

// The tunnel's calls, each followed by <app>/<environment>.
const (
	CheckPath   = "/v1/check/"
	ConnectPath = "/v1/connect/"
)

// ReadOnlyParam is the query parameter that asks for read-only, "true".
const ReadOnlyParam = "readOnly"

// EnvironmentAuto asks for staging when the Application has one, else
// prod.
const EnvironmentAuto = "auto"

// Grant is what a check allows: the body of a check's 200.
type Grant struct {
	// Login is the developer's GitHub login.
	Login       string `json:"login"`
	Application string `json:"application"`
	// Environment is the one resolved, never auto.
	Environment string `json:"environment"`
	// Access is read-write or read-only.
	Access string `json:"access"`
	// Role is the Postgres role the tunnel logs in as, and Database the
	// database it logs in to.
	Role     string `json:"role"`
	Database string `json:"database"`
}

// ErrorResponse is the body of every refusal.
type ErrorResponse struct {
	Error string `json:"error"`
}

// Refused is the tunnel's own refusal of a call.
type Refused struct {
	Status  int
	Message string
}

func (r *Refused) Error() string { return r.Message }

// Client calls the database tunnel at URL with a developer's token.
type Client struct {
	// URL is the tunnel's address, https://db.<baseDomain>.
	URL   string
	Token string
	// Dial opens the TCP connection to URL's host and port; nil means a
	// plain net.Dialer. TLSConfig is the TLS on top of it; nil means the
	// system's roots, for URL's host.
	Dial      func(ctx context.Context, network, address string) (net.Conn, error)
	TLSConfig *tls.Config
}

// Check asks the tunnel to run its check for one connection, without
// opening one.
func (c *Client) Check(ctx context.Context, application, environment string, readOnly bool) (Grant, error) {
	u, err := c.callURL(CheckPath, application, environment, readOnly)
	if err != nil {
		return Grant{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return Grant{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "application/json")
	client := &http.Client{Timeout: time.Minute, Transport: &http.Transport{DialContext: c.dial, TLSClientConfig: c.TLSConfig}}
	resp, err := client.Do(req)
	if err != nil {
		return Grant{}, fmt.Errorf("the database tunnel at %s cannot be reached: %w", c.URL, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusOK {
		var grant Grant
		if err := json.Unmarshal(data, &grant); err != nil || grant.Role == "" {
			return Grant{}, fmt.Errorf("the database tunnel at %s answered with something other than a check: %s", c.URL, strings.TrimSpace(string(data)))
		}
		return grant, nil
	}
	var refusal ErrorResponse
	if json.Unmarshal(data, &refusal) != nil || refusal.Error == "" {
		// Not the tunnel's own answer: a proxy's error page, or no tunnel.
		return Grant{}, fmt.Errorf("the database tunnel at %s is unavailable: HTTP %d: %s", c.URL, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return Grant{}, &Refused{Status: resp.StatusCode, Message: refusal.Error}
}

// Connect opens one Postgres connection through the tunnel: a WebSocket
// whose binary messages carry the connection's bytes each way.
func (c *Client) Connect(ctx context.Context, application, environment string, readOnly bool) (net.Conn, error) {
	u, err := c.callURL(ConnectPath, application, environment, readOnly)
	if err != nil {
		return nil, err
	}
	port := u.Port()
	if port == "" {
		port = "443"
	}
	raw, err := c.dial(ctx, "tcp", net.JoinHostPort(u.Hostname(), port))
	if err != nil {
		return nil, fmt.Errorf("the database tunnel at %s cannot be reached: %w", c.URL, err)
	}
	tlsConfig := &tls.Config{ServerName: u.Hostname(), MinVersion: tls.VersionTLS12}
	if c.TLSConfig != nil {
		tlsConfig = c.TLSConfig.Clone()
		if tlsConfig.ServerName == "" {
			tlsConfig.ServerName = u.Hostname()
		}
	}
	conn := tls.Client(raw, tlsConfig)
	if err := conn.HandshakeContext(ctx); err != nil {
		raw.Close()
		return nil, fmt.Errorf("the database tunnel at %s cannot be reached: %w", c.URL, err)
	}
	u.Scheme = "wss"
	config, err := websocket.NewConfig(u.String(), c.URL)
	if err != nil {
		conn.Close()
		return nil, err
	}
	config.Header = http.Header{"Authorization": {"Bearer " + c.Token}}
	// The upgrade is answered before the tunnel's check runs, so it is
	// quick; the deadline only guards against a tunnel that never answers.
	_ = conn.SetDeadline(time.Now().Add(time.Minute))
	ws, err := websocket.NewClient(config, conn)
	if err != nil {
		conn.Close()
		if errors.Is(err, websocket.ErrBadStatus) {
			return nil, fmt.Errorf("the database tunnel at %s refused the connection; run iidp app db connect again to see why", c.URL)
		}
		return nil, fmt.Errorf("the database tunnel at %s cannot be reached: %w", c.URL, err)
	}
	_ = conn.SetDeadline(time.Time{})
	ws.PayloadType = websocket.BinaryFrame
	return ws, nil
}

func (c *Client) callURL(path, application, environment string, readOnly bool) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSuffix(c.URL, "/") + path + url.PathEscape(application) + "/" + url.PathEscape(environment))
	if err != nil {
		return nil, fmt.Errorf("the database tunnel's address %q: %w", c.URL, err)
	}
	if readOnly {
		u.RawQuery = ReadOnlyParam + "=true"
	}
	return u, nil
}

func (c *Client) dial(ctx context.Context, network, address string) (net.Conn, error) {
	if c.Dial != nil {
		return c.Dial(ctx, network, address)
	}
	var d net.Dialer
	return d.DialContext(ctx, network, address)
}
