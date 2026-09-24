package deploygate_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/Itema-as/iidp/internal/deploygate"
	"github.com/Itema-as/iidp/internal/registry"
)

// The image check (docs/implementation-notes/61-image-check.md), through
// the gate's HTTP boundary, against fakeRegistry: one TLS server standing
// in for every registry host. The gate's HTTP client dials it whatever the
// host, so values.yaml keeps the real ghcr.io and docker.io references and
// the gate's rules about which host gets the pull token are the real ones.

const (
	pullUser  = "platform-admin"
	pullToken = "ghp_pulltoken0123456789"
)

// fakeRegistry answers manifest HEADs and token requests the way GHCR and
// Docker Hub do: a HEAD without a bearer token gets 401 and a Bearer
// challenge naming the realm; the realm hands out a token for a public
// repository to anyone and for a private one only with the pull
// credential (and answers 403 DENIED otherwise, as GHCR does); a HEAD
// with the token gets 200, or 404 for a tag it does not have.
type fakeRegistry struct {
	srv *httptest.Server

	mu       sync.Mutex
	requests []registryRequest
	// missing are the "name:tag"s the registry does not have; every other
	// tag of every repository exists.
	missing map[string]bool
	// public are the repositories an anonymous token may read.
	public map[string]bool
	// realms overrides the token realm a host's challenge names.
	realms map[string]string
	// manifestStatus and tokenStatus, when set, are what every manifest
	// or token request is answered with instead.
	manifestStatus, tokenStatus int
	// unreachable makes every connection fail.
	unreachable bool
}

type registryRequest struct {
	Method, Host, Path, Authorization, Accept string
}

var manifestPath = regexp.MustCompile(`^/v2/(.+)/manifests/([^/]+)$`)

func newFakeRegistry(t *testing.T) *fakeRegistry {
	t.Helper()
	f := &fakeRegistry{missing: map[string]bool{}, public: map[string]bool{"library/nginx": true}, realms: map[string]string{}}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

// checker is the gate's image check, pointed at the fake for every host,
// with the pull credential laid out the way the Secret volume of
// argocd/ghcr-pull-token is.
func (f *fakeRegistry) checker(t *testing.T) *registry.Checker {
	t.Helper()
	pool := x509.NewCertPool()
	pool.AddCert(f.srv.Certificate())
	addr := f.srv.Listener.Addr().String()
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			f.mu.Lock()
			down := f.unreachable
			f.mu.Unlock()
			if down {
				return nil, errors.New("connect: connection refused")
			}
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		},
		// The test certificate is for example.com; the Host header still
		// names the registry the gate meant.
		TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "example.com"},
	}
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "username"), pullUser+"\n")
	writeFile(t, filepath.Join(dir, "token"), pullToken+"\n")
	return &registry.Checker{
		HTTPClient:     &http.Client{Transport: transport},
		CredentialHost: deploygate.GHCR,
		Credential:     deploygate.RegistryCredentialFromDir(dir),
	}
}

func (f *fakeRegistry) set(fn func(f *fakeRegistry)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *fakeRegistry) log() []registryRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]registryRequest(nil), f.requests...)
}

func (f *fakeRegistry) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, registryRequest{r.Method, r.Host, r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("Accept")})

	if r.URL.Path == "/token" {
		f.serveToken(w, r)
		return
	}
	m := manifestPath.FindStringSubmatch(r.URL.Path)
	if m == nil || r.Method != http.MethodHead {
		http.NotFound(w, r)
		return
	}
	name, tag := m[1], m[2]
	if f.manifestStatus != 0 {
		w.WriteHeader(f.manifestStatus)
		return
	}
	if r.Header.Get("Authorization") != "Bearer tok-"+name {
		realm := f.realms[r.Host]
		if realm == "" {
			host := r.Host
			if host == "registry-1.docker.io" {
				host = "auth.docker.io"
			}
			realm = "https://" + host + "/token"
		}
		w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm=%q,service=%q,scope="repository:%s:pull"`, realm, r.Host, name))
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	// A registry answers 404 for a multi-platform image to a client that
	// does not accept an index.
	for _, mediaType := range registry.ManifestMediaTypes {
		if !strings.Contains(r.Header.Get("Accept"), mediaType) {
			w.WriteHeader(http.StatusNotFound)
			return
		}
	}
	if f.missing[name+":"+tag] {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/vnd.oci.image.index.v1+json")
	w.Header().Set("Docker-Content-Digest", "sha256:"+strings.Repeat("a", 64))
	w.WriteHeader(http.StatusOK)
}

func (f *fakeRegistry) serveToken(w http.ResponseWriter, r *http.Request) {
	denied := func(status int, code string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{"errors": []map[string]string{{"code": code, "message": "requested access to the resource is denied"}}})
	}
	if f.tokenStatus != 0 {
		w.WriteHeader(f.tokenStatus)
		return
	}
	name, ok := strings.CutPrefix(r.URL.Query().Get("scope"), "repository:")
	name, ok2 := strings.CutSuffix(name, ":pull")
	if !ok || !ok2 {
		denied(http.StatusBadRequest, "UNSUPPORTED")
		return
	}
	user, password, basic := r.BasicAuth()
	switch {
	case basic && (user != pullUser || password != pullToken):
		denied(http.StatusUnauthorized, "UNAUTHORIZED")
		return
	case !basic && !f.public[name]:
		denied(http.StatusForbidden, "DENIED")
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]string{"token": "tok-" + name})
}

func basicAuth(user, password string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+password))
}

func TestTheGateChecksAPrivateImageWithThePullToken(t *testing.T) {
	e := newEnv(t)
	addApplication(t, e.platform, "shop", false, binding(shopRepoID, orgID))

	status, body := e.deploy(e.issuer.claims(), "shop", "auto", shaDeployed)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %v", status, body)
	}
	if got := headSubject(t, e.platform); got != "Deploy shop prod "+shaDeployed {
		t.Errorf("head = %q, want the deploy", got)
	}

	var sawToken, sawManifest bool
	for _, req := range e.registry.log() {
		if req.Host != "ghcr.io" {
			t.Errorf("the gate asked %s %s%s; want only ghcr.io", req.Method, req.Host, req.Path)
		}
		switch {
		case req.Path == "/token":
			sawToken = true
			if req.Authorization != basicAuth(pullUser, pullToken) {
				t.Errorf("the token request carried %q, want the pull token as basic auth", req.Authorization)
			}
		case req.Authorization != "":
			sawManifest = true
			if req.Method != http.MethodHead || req.Path != "/v2/itema-as/shop/manifests/"+shaDeployed {
				t.Errorf("the authorised manifest request was %s %s", req.Method, req.Path)
			}
			if req.Authorization != "Bearer tok-itema-as/shop" {
				t.Errorf("the manifest request carried %q, want the registry's bearer token", req.Authorization)
			}
		}
	}
	if !sawToken || !sawManifest {
		t.Errorf("registry requests = %+v, want a token request and an authorised manifest HEAD", e.registry.log())
	}
}

func TestTheGateRefusesATagThatDoesNotExist(t *testing.T) {
	e := newEnv(t)
	addApplication(t, e.platform, "shop", true, binding(shopRepoID, orgID))
	e.registry.set(func(f *fakeRegistry) { f.missing["itema-as/shop:"+shaDeployed] = true })

	status, body := e.deploy(e.issuer.claims(), "shop", "auto", shaDeployed)
	e.refused(status, body, http.StatusUnprocessableEntity, "ghcr.io/itema-as/shop:"+shaDeployed+" does not exist", "nothing was deployed")

	// A promotion is checked the same way.
	claims := e.issuer.claims()
	claims["ref"] = "refs/tags/v1.2.3"
	status, body = e.deploy(claims, "shop", "auto", shaDeployed)
	e.refused(status, body, http.StatusUnprocessableEntity, "ghcr.io/itema-as/shop:"+shaDeployed)
}

func TestTheGateChecksAPublicImageAnonymously(t *testing.T) {
	e := newEnv(t)
	addApplication(t, e.platform, "shop", false, binding(shopRepoID, orgID))
	pushCommit(t, e.platform, "Serve nginx", map[string]string{
		"applications/shop/prod/values.yaml": "image:\n  repository: docker.io/library/nginx\n  tag: \"\"\n",
	})

	status, body := e.deploy(e.issuer.claims(), "shop", "auto", "1.27-alpine")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %v", status, body)
	}
	var sawToken bool
	for _, req := range e.registry.log() {
		if strings.HasPrefix(req.Authorization, "Basic ") {
			t.Errorf("%s %s%s carried basic auth; the pull token is for ghcr.io only", req.Method, req.Host, req.Path)
		}
		switch {
		case req.Path == "/token":
			sawToken = true
			if req.Host != "auth.docker.io" {
				t.Errorf("token request to %s, want the challenge's realm auth.docker.io", req.Host)
			}
		case req.Host != "registry-1.docker.io" || req.Path != "/v2/library/nginx/manifests/1.27-alpine":
			t.Errorf("manifest request %s%s, want Docker Hub's API host and library/nginx", req.Host, req.Path)
		}
	}
	if !sawToken {
		t.Errorf("no anonymous token request: %+v", e.registry.log())
	}

	// A tag Docker Hub does not have is refused just the same.
	e.registry.set(func(f *fakeRegistry) { f.missing["library/nginx:0.0-nothing"] = true })
	status, body = e.deploy(e.issuer.claims(), "shop", "auto", "0.0-nothing")
	if msg, _ := body["error"].(string); status != http.StatusUnprocessableEntity || !strings.Contains(msg, "docker.io/library/nginx:0.0-nothing does not exist") {
		t.Errorf("a missing public tag: HTTP %d %q, want 422 naming the image", status, msg)
	}
}

func TestTheGateNeverSendsThePullTokenToAnotherRealm(t *testing.T) {
	e := newEnv(t)
	addApplication(t, e.platform, "shop", false, binding(shopRepoID, orgID))
	e.registry.set(func(f *fakeRegistry) { f.realms["ghcr.io"] = "https://collector.example.test/token" })

	status, body := e.deploy(e.issuer.claims(), "shop", "auto", shaDeployed)
	e.refused(status, body, http.StatusServiceUnavailable, "couldn't check", "ghcr.io/itema-as/shop:"+shaDeployed)
	for _, req := range e.registry.log() {
		if req.Authorization != "" && req.Host != "ghcr.io" {
			t.Errorf("%s%s carried %q; the pull token goes to ghcr.io only", req.Host, req.Path, req.Authorization)
		}
	}
}

func TestTheGateRefusesWhenTheRegistryCannotBeAsked(t *testing.T) {
	for name, tc := range map[string]struct {
		setup func(f *fakeRegistry)
		// token, when set, replaces the gate's pull token.
		token string
		want  string
	}{
		"unreachable":          {setup: func(f *fakeRegistry) { f.unreachable = true }, want: "connection refused"},
		"a 5xx on the tag":     {setup: func(f *fakeRegistry) { f.manifestStatus = http.StatusInternalServerError }, want: "HTTP 500"},
		"a 5xx on the token":   {setup: func(f *fakeRegistry) { f.tokenStatus = http.StatusBadGateway }, want: "HTTP 502"},
		"rate limited":         {setup: func(f *fakeRegistry) { f.manifestStatus = http.StatusTooManyRequests }, want: "HTTP 429"},
		"a revoked pull token": {token: "ghp_revoked", want: "pull token cannot read it"},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			addApplication(t, e.platform, "shop", false, binding(shopRepoID, orgID))
			if tc.setup != nil {
				e.registry.set(tc.setup)
			}
			if tc.token != "" {
				e.gate.Images.Credential = func() (registry.Credential, error) {
					return registry.Credential{Username: pullUser, Password: tc.token}, nil
				}
			}
			status, body := e.deploy(e.issuer.claims(), "shop", "auto", shaDeployed)
			e.refused(status, body, http.StatusServiceUnavailable, "couldn't check that the image ghcr.io/itema-as/shop:"+shaDeployed+" exists", "nothing was deployed", tc.want)
		})
	}
}

func TestTheGateDoesNotCheckATagTheEnvironmentAlreadyRuns(t *testing.T) {
	e := newEnv(t)
	addApplication(t, e.platform, "shop", false, binding(shopRepoID, orgID))
	if status, body := e.deploy(e.issuer.claims(), "shop", "auto", shaDeployed); status != http.StatusOK {
		t.Fatalf("status = %d: %v", status, body)
	}

	// The repeat CI makes after a lost answer commits nothing, so it is
	// answered even while the registry is down.
	e.registry.set(func(f *fakeRegistry) { f.unreachable = true })
	status, body := e.deploy(e.issuer.claims(), "shop", "auto", shaDeployed)
	if status != http.StatusOK || body["unchanged"] != true {
		t.Errorf("the repeat: HTTP %d %v, want 200 and unchanged", status, body)
	}
}

func TestTheGateAsksNoRegistryForAnUnauthorisedCall(t *testing.T) {
	e := newEnv(t)
	addApplication(t, e.platform, "shop", false, binding(shopRepoID, orgID))
	claims := e.issuer.claims()
	claims["repository_id"] = "700000001"
	status, body := e.deploy(claims, "shop", "auto", shaDeployed)
	e.refused(status, body, http.StatusForbidden)
	if got := e.registry.log(); len(got) != 0 {
		t.Errorf("the registry was asked %+v for a call the gate refused", got)
	}
}
