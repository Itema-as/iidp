package e2e

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// CheckSignInRedirect must fail on a 302 with no Location, which a browser
// renders instead of following, and on a redirect that loses where to come
// back to; and pass on oauth2-proxy's own redirect to the provider.
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

// For a custom domain outside the login cookie domain, CheckSignInRedirect
// wants the host-only proxy's redirect: the callback on the host itself, a
// CSRF cookie with no Domain, and the path alone to return to.
func TestCheckSignInRedirectHostOnly(t *testing.T) {
	want := SignIn{
		LoginURL:   "https://idp.example.test/authorize",
		HostOnly:   true,
		CSRFCookie: "__Host-itema_login_csrf",
	}
	const host, path = "shop.other.test", "/orders?page=2&sort=date"
	const callback = "https://" + host + "/oauth2/callback"
	csrf := &http.Cookie{Name: want.CSRFCookie, Value: "x", Path: "/", Secure: true}
	redirect := func(callback, state string, cookie *http.Cookie) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			http.SetCookie(w, cookie)
			q := url.Values{"redirect_uri": {callback}, "state": {state}}
			http.Redirect(w, r, want.LoginURL+"?"+q.Encode(), http.StatusFound)
		}
	}
	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
		wantErr string
	}{
		{"redirect to the provider", redirect(callback, "hash:"+path, csrf), ""},
		{"callback on the auth address", redirect("https://auth.app.example.test/oauth2/callback", "hash:"+path, csrf), "redirect_uri"},
		{"CSRF cookie with a Domain", redirect(callback, "hash:"+path, &http.Cookie{Name: want.CSRFCookie, Value: "x", Path: "/", Secure: true, Domain: ".other.test"}), "no __Host-itema_login_csrf cookie for the host alone"},
		{"CSRF cookie not Secure", redirect(callback, "hash:"+path, &http.Cookie{Name: want.CSRFCookie, Value: "x", Path: "/"}), "no __Host-itema_login_csrf cookie for the host alone"},
		{"path lost", redirect(callback, "hash:/", csrf), "state"},
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

// The callback must be answered by oauth2-proxy itself.
func TestLoginCallbackAnswer(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		header    http.Header
		ok, retry bool
	}{
		{"oauth2-proxy's error page", http.StatusInternalServerError, nil, true, false},
		{"oauth2-proxy's forbidden page", http.StatusForbidden, nil, true, false},
		{"no route yet", http.StatusNotFound, nil, false, true},
		{"no endpoint yet", http.StatusServiceUnavailable, nil, false, true},
		{"sent to sign-in", http.StatusFound, http.Header{"Location": {"https://idp.example.test/authorize"}}, false, false},
		{"the Application", http.StatusOK, http.Header{"Server": {"nginx/1.30.0"}}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := &http.Response{StatusCode: tc.status, Status: strconv.Itoa(tc.status), Header: tc.header}
			if resp.Header == nil {
				resp.Header = http.Header{}
			}
			ok, retry, message := loginCallbackAnswer(resp)
			if ok != tc.ok || retry != tc.retry {
				t.Errorf("ok, retry = %v, %v (%s), want %v, %v", ok, retry, message, tc.ok, tc.retry)
			}
		})
	}
}

// checkServedCertificate must pass only when the server presents exactly
// the certificate wanted for the SNI it was sent.
func TestCheckServedCertificate(t *testing.T) {
	const host = "shop.other.test"
	certPEM, keyPEM, der, err := selfSignedCertificate(host)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	errorPage := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) }
	for _, tc := range []struct {
		name    string
		cert    *tls.Certificate
		wantErr string
	}{
		{"the host's certificate", &pair, ""},
		{"the default certificate", nil, "presented a certificate other than"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewUnstartedServer(http.HandlerFunc(errorPage))
			if tc.cert != nil {
				server.TLS = &tls.Config{Certificates: []tls.Certificate{*tc.cert}}
			}
			server.StartTLS()
			defer server.Close()
			cluster := &Cluster{HTTPSPort: serverPort(t, server.URL), Log: t.Logf}
			err := cluster.checkServedCertificate(context.Background(), host, "/oauth2/callback", der, time.Second)
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
// its own backend over https and plain http redirects to it there.
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

func serverPort(t *testing.T, serverURL string) int {
	t.Helper()
	port, err := strconv.Atoi(serverURL[strings.LastIndex(serverURL, ":")+1:])
	if err != nil {
		t.Fatal(err)
	}
	return port
}
