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
		CookieDomain: "app.example.test",
	}
	const host, path = "shop.app.example.test", "/orders?page=2&sort=date"
	redirect := func(state string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			http.SetCookie(w, &http.Cookie{Name: "_oauth2_proxy_csrf", Value: "x", Domain: ".app.example.test"})
			q := url.Values{"redirect_uri": {want.Callback}, "state": {state}}
			http.Redirect(w, r, want.LoginURL+"?"+q.Encode(), http.StatusFound)
		}
	}
	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
		wantErr string
	}{
		{"redirect to the provider", redirect("hash:https://" + host + path), ""},
		{"302 without Location", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusFound)
			w.Write([]byte(`<form method="GET" action="/oauth2/start">`))
		}, `Location ""`},
		{"original URL lost", redirect("hash:/"), "state"},
		{"sign-in page", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusForbidden)
		}, "want a redirect"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewTLSServer(tc.handler)
			defer server.Close()
			port, err := strconv.Atoi(server.URL[strings.LastIndex(server.URL, ":")+1:])
			if err != nil {
				t.Fatal(err)
			}
			cluster := &Cluster{HTTPSPort: port, Log: t.Logf}
			err = cluster.CheckSignInRedirect(context.Background(), host, path, want, time.Second)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("got %v, want no error", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("got %v, want an error mentioning %q", err, tc.wantErr)
			}
		})
	}
}
