package argus

import (
	"crypto/sha256"
	"encoding/base64"
	"io/fs"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// Keepalive is how often a stream gets a comment, so that proxies and
// browsers keep it open however quiet the Platform is.
const Keepalive = 15 * time.Second

// Server serves Argus: the stream, the probes and the web directory.
type Server struct {
	Store *Store
	// Web is the frontend, served at /.
	Web fs.FS
	// Keepalive overrides the Keepalive interval, for tests.
	Keepalive time.Duration
}

// Handler routes:
//
//	GET /events   the stream (doc.go)
//	GET /healthz  200 while the process serves: the liveness probe
//	GET /readyz   200 once the first look at the cluster is complete, 503
//	              before: the readiness probe
//	GET /         the web directory
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /events", s.events)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if !s.Store.Ready() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("still reading the cluster\n"))
			return
		}
		_, _ = w.Write([]byte("ok\n"))
	})
	files := http.FileServerFS(s.Web)
	csp := ContentSecurityPolicy(s.Web)
	mux.Handle("GET /", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", csp)
		files.ServeHTTP(w, r)
	}))
	return mux
}

// importMap finds index.html's inline import maps: the only inline
// scripts the page has.
var importMap = regexp.MustCompile(`(?s)<script type="importmap">(.*?)</script>`)

// ContentSecurityPolicy is the policy the web directory is served with.
// The page loads nothing from outside Argus. Its one inline script, the
// import map, is allowed by its hash, read from web's index.html. The
// card's links are navigations, which the policy does not govern.
func ContentSecurityPolicy(web fs.FS) string {
	scripts := []string{"'self'"}
	if index, err := fs.ReadFile(web, "index.html"); err == nil {
		for _, m := range importMap.FindAllSubmatch(index, -1) {
			sum := sha256.Sum256(m[1])
			scripts = append(scripts, "'sha256-"+base64.StdEncoding.EncodeToString(sum[:])+"'")
		}
	}
	return strings.Join([]string{
		"default-src 'self'",
		"script-src " + strings.Join(scripts, " "),
		"style-src 'self'",
		"img-src 'self'",
		"font-src 'self'",
		"connect-src 'self'",
		"object-src 'none'",
		"base-uri 'none'",
		"form-action 'none'",
		"frame-ancestors 'none'",
	}, "; ")
}

// events is the stream: the snapshot, then every message as it happens,
// and a keepalive comment every Keepalive. It ends when the browser goes,
// when the server shuts down, or when the browser falls too far behind;
// the browser's EventSource then reconnects and gets a fresh snapshot.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	rc := http.NewResponseController(w)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	// Tells a buffering proxy not to buffer the stream (nginx's
	// convention); Traefik does not buffer a streamed response anyway.
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	snapshot, c := s.Store.subscribe()
	defer s.Store.unsubscribe(c)
	// No id: lines, so a reconnect sends no Last-Event-ID and there is
	// nothing to replay. retry: is how long the browser waits before
	// reconnecting.
	if _, err := w.Write(append([]byte("retry: 3000\n\n"), snapshot...)); err != nil {
		return
	}
	if rc.Flush() != nil {
		return
	}
	every := s.Keepalive
	if every <= 0 {
		every = Keepalive
	}
	keepalive := time.NewTicker(every)
	defer keepalive.Stop()
	for {
		var out []byte
		select {
		case <-r.Context().Done():
			return
		case f, ok := <-c.ch:
			if !ok {
				return
			}
			out = f
		case <-keepalive.C:
			out = []byte(": keepalive\n\n")
		}
		if _, err := w.Write(out); err != nil {
			return
		}
		if rc.Flush() != nil {
			return
		}
	}
}
