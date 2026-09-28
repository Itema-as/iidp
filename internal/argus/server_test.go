package argus

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/Itema-as/iidp/internal/platformstate"
)

// message is one server-sent event, or a comment.
type message struct {
	event, data, comment string
}

// stream is one browser's connection to /events.
type stream struct {
	t        *testing.T
	messages chan message
	close    func()
}

// connect opens /events and reads its messages as they come.
func connect(t *testing.T, url string) *stream {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url+"/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" || resp.Header.Get("Cache-Control") != "no-cache" {
		cancel()
		t.Fatalf("GET /events: %d, Content-Type %q, Cache-Control %q", resp.StatusCode, resp.Header.Get("Content-Type"), resp.Header.Get("Cache-Control"))
	}
	s := &stream{t: t, messages: make(chan message, 1000)}
	s.close = func() { cancel(); resp.Body.Close() }
	t.Cleanup(s.close)
	go func() {
		defer close(s.messages)
		r := bufio.NewReader(resp.Body)
		var m message
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimSuffix(line, "\n")
			switch {
			case line == "":
				if m != (message{}) {
					s.messages <- m
				}
				m = message{}
			case strings.HasPrefix(line, ": "):
				m.comment = strings.TrimPrefix(line, ": ")
			case strings.HasPrefix(line, "event: "):
				m.event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				m.data = strings.TrimPrefix(line, "data: ")
			}
		}
	}()
	return s
}

// next is the next message that is not a retry line, failing after a
// few seconds.
func (s *stream) next() message {
	s.t.Helper()
	for {
		select {
		case m, ok := <-s.messages:
			if !ok {
				s.t.Fatal("the stream ended")
			}
			if m.event == "" && m.comment == "" {
				continue // retry:
			}
			return m
		case <-time.After(5 * time.Second):
			s.t.Fatal("no message within 5 s")
		}
	}
}

// nextEvent skips keepalives until the next event.
func (s *stream) nextEvent() message {
	s.t.Helper()
	for {
		if m := s.next(); m.event != "" {
			return m
		}
	}
}

func (s *stream) snapshot() Snapshot {
	s.t.Helper()
	m := s.nextEvent()
	if m.event != "snapshot" {
		s.t.Fatalf("first event = %s %s, want the snapshot", m.event, m.data)
	}
	var snap Snapshot
	if err := json.Unmarshal([]byte(m.data), &snap); err != nil {
		s.t.Fatal(err)
	}
	return snap
}

func serve(t *testing.T, s *Store, keepalive time.Duration) *httptest.Server {
	t.Helper()
	web := fstest.MapFS{"index.html": {Data: []byte("<!doctype html><title>Argus</title>")}}
	srv := httptest.NewServer((&Server{Store: s, Web: web, Keepalive: keepalive}).Handler())
	t.Cleanup(srv.Close)
	return srv
}

// serveWeb serves s with the web directory web.
func serveWeb(t *testing.T, s *Store, web fstest.MapFS) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer((&Server{Store: s, Web: web, Keepalive: time.Hour}).Handler())
	t.Cleanup(srv.Close)
	return srv
}

// On connect a browser gets the whole model and the recent feed, then
// each change as it happens: the Application that changed, whole, and
// the note the change makes.
func TestStreamSendsTheSnapshotThenDeltas(t *testing.T) {
	s, clock := newStore(t)
	put(t, s, servingShop()...)
	put(t, s, component("argocd"))
	s.Seed()
	srv := serve(t, s, time.Hour)

	st := connect(t, srv.URL)
	snap := st.snapshot()
	if !snap.Ready || snap.Cluster.State != Connected || !snap.RestartedAt.Equal(t0) {
		t.Errorf("snapshot = ready %v, cluster %+v, restarted %s", snap.Ready, snap.Cluster, snap.RestartedAt)
	}
	if len(snap.Applications) != 1 || snap.Applications[0].Name != "shop" || len(snap.Applications[0].Environments) != 1 ||
		snap.Applications[0].Environments[0].Condition.State != platformstate.Healthy {
		t.Fatalf("snapshot Applications = %+v", snap.Applications)
	}
	if len(snap.Components) != 1 || snap.Components[0].Name != "argocd" {
		t.Errorf("snapshot components = %+v", snap.Components)
	}
	if last := snap.Feed[len(snap.Feed)-1]; !last.Seam {
		t.Errorf("the snapshot's feed ends with %+v, want the seam", last)
	}

	// The pod stops being ready: within the grace nothing changes, after
	// it the Environment is Degraded.
	application := func() platformstate.Application {
		t.Helper()
		m := st.nextEvent()
		if m.event != "application" {
			t.Fatalf("event = %s %s, want the Application", m.event, m.data)
		}
		var app platformstate.Application
		if err := json.Unmarshal([]byte(m.data), &app); err != nil {
			t.Fatal(err)
		}
		return app
	}
	put(t, s, pod("shop-prod", "shop-a", "shop", "1.0.0", false, 0))
	s.Recompute()
	if env := application().Environments[0]; env.Condition.State != platformstate.Healthy || env.Pods.Ready != 0 {
		t.Errorf("delta within the grace = %+v, pods %+v; want Healthy with no pod ready", env.Condition, env.Pods)
	}
	clock.add(61 * time.Second)
	s.Recompute()
	if c := application().Environments[0].Condition; c.State != platformstate.Degraded {
		t.Errorf("delta Condition = %+v, want Degraded", c)
	}
	m := st.nextEvent()
	var n Note
	if err := json.Unmarshal([]byte(m.data), &n); err != nil || m.event != "note" {
		t.Fatalf("event = %s %s, want the note", m.event, m.data)
	}
	if n.Loudness != Loud || n.Place != (Place{Application: "shop", Environment: "prod"}) || !strings.HasPrefix(n.Message, "shop prod is Degraded") || n.FeedOnly || n.Seeded {
		t.Errorf("note = %+v", n)
	}

	// Nothing else changed, so nothing else is sent.
	s.Recompute()
	s.Put("applications", "argocd/unrelated", platformstate.ArgoCDApplication{})
	s.Recompute()
	select {
	case m := <-st.messages:
		t.Errorf("unexpected %+v with nothing changed", m)
	case <-time.After(100 * time.Millisecond):
	}
}

// An idle stream gets a keepalive comment.
func TestStreamKeepsAlive(t *testing.T) {
	s, _ := newStore(t)
	s.Seed()
	st := connect(t, serve(t, s, 20*time.Millisecond).URL)
	st.snapshot()
	for i := 0; i < 2; i++ {
		if m := st.next(); m.comment != "keepalive" || m.event != "" {
			t.Fatalf("message = %+v, want a keepalive comment", m)
		}
	}
	if Keepalive != 15*time.Second {
		t.Errorf("Keepalive = %s, want 15 s", Keepalive)
	}
}

// A reconnect gets a fresh snapshot, with what changed while it was away,
// and nothing is replayed: there are no event ids to resume from.
func TestReconnectGetsAFreshSnapshot(t *testing.T) {
	s, _ := newStore(t)
	put(t, s, servingShop()...)
	s.Seed()
	srv := serve(t, s, time.Hour)
	first := connect(t, srv.URL)
	first.snapshot()
	first.close()

	put(t, s, argoApp("notes", "prod"))
	s.Recompute()

	resp, err := http.Get(srv.URL + "/events")
	if err != nil {
		t.Fatal(err)
	}
	head := make([]byte, 64)
	n, _ := io.ReadAtLeast(resp.Body, head, 20)
	resp.Body.Close()
	if got := string(head[:n]); !strings.HasPrefix(got, "retry: 3000\n\nevent: snapshot\n") || strings.Contains(got, "id:") {
		t.Errorf("a stream starts %q, want retry, then the snapshot, and no id", got)
	}

	second := connect(t, srv.URL)
	snap := second.snapshot()
	var names []string
	for _, a := range snap.Applications {
		names = append(names, a.Name)
	}
	if strings.Join(names, ",") != "notes,shop" {
		t.Errorf("fresh snapshot Applications = %v, want notes and shop", names)
	}
	// notes appearing is in the feed it gets, not replayed as a message.
	if !strings.HasPrefix(snap.Feed[len(snap.Feed)-1].Message, "notes prod is Unreleased") {
		t.Errorf("feed = %+v, want notes last", snap.Feed)
	}
}

// A browser that falls too far behind is closed rather than holding the
// store up; it reconnects to a fresh snapshot.
func TestASlowBrowserIsClosed(t *testing.T) {
	s, _ := newStore(t)
	_, c := s.subscribe()
	s.mu.Lock()
	for i := 0; i < clientBuffer+1; i++ {
		s.broadcast([][]byte{[]byte("event: note\ndata: {}\n\n")})
	}
	_, still := s.clients[c]
	s.mu.Unlock()
	if still {
		t.Fatal("a browser clientBuffer messages behind is still subscribed")
	}
	n := 0
	for range c.ch {
		n++
	}
	if n != clientBuffer {
		t.Errorf("it got %d messages before its stream closed, want %d", n, clientBuffer)
	}
	s.unsubscribe(c) // after the store closed it: no panic
}

// The probes, and the web directory.
func TestProbesAndTheWebDirectory(t *testing.T) {
	s, _ := newStore(t)
	srv := serve(t, s, time.Hour)
	get := func(path string) (int, string) {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}
	if code, _ := get("/healthz"); code != http.StatusOK {
		t.Errorf("/healthz = %d before the first look, want 200: the process serves", code)
	}
	if code, _ := get("/readyz"); code != http.StatusServiceUnavailable {
		t.Errorf("/readyz = %d before the first look, want 503", code)
	}
	s.Seed()
	if code, _ := get("/readyz"); code != http.StatusOK {
		t.Errorf("/readyz = %d after it, want 200", code)
	}
	if code, body := get("/"); code != http.StatusOK || !strings.Contains(body, "<title>Argus</title>") {
		t.Errorf("/ = %d %q, want the web directory's index", code, body)
	}
	resp, err := http.Post(srv.URL+"/events", "text/plain", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST /events = %d, want 405: Argus is read-only", resp.StatusCode)
	}
}

// The cluster signal: interrupted as soon as the API server stops
// answering, lost after 30 s, connected again when it answers.
func TestClusterLostAndFound(t *testing.T) {
	s, clock := newStore(t)
	s.Seed()
	srv := serve(t, s, time.Hour)
	st := connect(t, srv.URL)
	st.snapshot()

	cluster := func() ClusterState {
		t.Helper()
		for {
			m := st.nextEvent()
			if m.event == "cluster" {
				var c ClusterState
				if err := json.Unmarshal([]byte(m.data), &c); err != nil {
					t.Fatal(err)
				}
				return c
			}
		}
	}
	s.SetReachable(false)
	s.Recompute()
	if c := cluster(); c.State != Interrupted || c.Since == nil || !c.Since.Equal(t0) {
		t.Errorf("cluster = %+v, want interrupted since t0", c)
	}
	clock.add(29 * time.Second)
	s.Recompute()
	clock.add(time.Second)
	s.Recompute()
	if c := cluster(); c.State != Lost {
		t.Errorf("cluster = %+v after 30 s, want lost", c)
	}
	s.SetReachable(true)
	s.Recompute()
	if c := cluster(); c.State != Connected || c.Since != nil {
		t.Errorf("cluster = %+v, want connected", c)
	}
	var messages []string
	for _, n := range s.Snapshot().Feed {
		messages = append(messages, n.Loudness+": "+n.Message)
	}
	if got := strings.Join(messages, "; "); !strings.Contains(got, "normal: Argus has lost the cluster") || !strings.Contains(got, "quiet: Argus sees the cluster again") {
		t.Errorf("feed = %s", got)
	}
}
