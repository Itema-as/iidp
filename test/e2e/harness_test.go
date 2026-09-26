package e2e

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// CheckSignInRedirect must fail on what #77/#78 found on the real
// Platform, a 302 with no Location that a browser renders instead of
// following, and on a redirect that loses where to come back to; and pass
// on oauth2-proxy's own redirect to the provider.
func TestCheckSignInRedirect(t *testing.T) {
	want := SignIn{
		LoginURL:     "https://idp.example.test/authorize",
		Callback:     "https://auth.app.example.test/oauth2/callback",
		CookieDomain: "example.test",
		CSRFCookie:   "__Secure-itema_login_csrf",
	}
	const host, path = "shop.app.example.test", "/orders?page=2&sort=date"
	csrf := &http.Cookie{Name: want.CSRFCookie, Value: "x", Domain: ".example.test"}
	redirect := func(state string, cookie *http.Cookie) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			http.SetCookie(w, cookie)
			q := url.Values{"redirect_uri": {want.Callback}, "state": {state}}
			http.Redirect(w, r, want.LoginURL+"?"+q.Encode(), http.StatusFound)
		}
	}
	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
		wantErr string
	}{
		{"redirect to the provider", redirect("hash:https://"+host+path, csrf), ""},
		// What a browser gets from oauth2-proxy before #76: the default
		// cookie name, for the base domain only.
		{"CSRF cookie for the base domain", redirect("hash:https://"+host+path, &http.Cookie{Name: want.CSRFCookie, Value: "x", Domain: ".app.example.test"}), "no __Secure-itema_login_csrf cookie for example.test"},
		{"CSRF cookie with the default name", redirect("hash:https://"+host+path, &http.Cookie{Name: "_oauth2_proxy_csrf", Value: "x", Domain: ".example.test"}), "no __Secure-itema_login_csrf cookie"},
		{"302 without Location", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusFound)
			w.Write([]byte(`<form method="GET" action="/oauth2/start">`))
		}, `Location ""`},
		{"original URL lost", redirect("hash:/", csrf), "state"},
		{"sign-in page", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusForbidden)
		}, "want a redirect"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewTLSServer(tc.handler)
			defer server.Close()
			cluster := &Cluster{HTTPSPort: serverPort(t, server.URL), Log: t.Logf}
			err := cluster.CheckSignInRedirect(context.Background(), host, path, want, time.Second)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("got %v, want no error", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("got %v, want an error mentioning %q", err, tc.wantErr)
			}
		})
	}
}

// checkACMEChallengeServed must pass only when the challenge path reaches
// its own backend over https and plain http redirects to it there: a
// redirect to sign-in, the login middleware's answer, fails it (#76).
func TestCheckACMEChallengeServed(t *testing.T) {
	const host, path = "shop-staging.example.test", "/.well-known/acme-challenge/e2e-token"
	solver := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Server", "nginx/1.27.5")
		w.WriteHeader(http.StatusNotFound)
	}
	signIn := func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://idp.example.test/authorize", http.StatusFound)
	}
	toHTTPS := func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://"+host+r.URL.Path, http.StatusMovedPermanently)
	}
	for _, tc := range []struct {
		name         string
		https, plain http.HandlerFunc
		wantErr      string
	}{
		{"served by the solver", solver, toHTTPS, ""},
		{"sent to sign-in", signIn, toHTTPS, "went through the login middleware"},
		{"plain http not redirected", solver, solver, "want a redirect to https://" + host + path},
	} {
		t.Run(tc.name, func(t *testing.T) {
			secure := httptest.NewTLSServer(tc.https)
			defer secure.Close()
			plain := httptest.NewServer(tc.plain)
			defer plain.Close()
			cluster := &Cluster{HTTPSPort: serverPort(t, secure.URL), HTTPPort: serverPort(t, plain.URL), Log: t.Logf}
			err := cluster.checkACMEChallengeServed(context.Background(), host, path, time.Second)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("got %v, want no error", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("got %v, want an error mentioning %q", err, tc.wantErr)
			}
		})
	}
}

// serverPort is the port of an httptest server's URL.
func serverPort(t *testing.T, serverURL string) int {
	t.Helper()
	port, err := strconv.Atoi(serverURL[strings.LastIndex(serverURL, ":")+1:])
	if err != nil {
		t.Fatal(err)
	}
	return port
}
