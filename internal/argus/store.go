package argus

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"reflect"
	"sort"
	"sync"
	"time"

	"github.com/Itema-as/iidp/internal/platformstate"
)

// The store's timing.
const (
	// TickEvery is how often the model is interpreted again with nothing
	// having changed, for the rules that wait: the 60 s grace, the 5 and
	// 30 minutes, and the cluster lost after 30 s.
	TickEvery = 5 * time.Second
	// settle is how long a change waits for others before the model is
	// interpreted again, so a rollout's burst of updates is one delta.
	settle = 100 * time.Millisecond
	// clientBuffer is how many messages a slow browser may fall behind
	// before its stream is closed; it reconnects to a fresh snapshot.
	clientBuffer = 256
	// seedWarningsFor is how far back the Warning Events seeded into the
	// feed go.
	seedWarningsFor = time.Hour
)

// The cluster connection's states (ClusterState.State).
const (
	Connected   = "connected"
	Interrupted = "interrupted"
	Lost        = "lost"
)

// ClusterState is whether Argus can see the cluster.
type ClusterState struct {
	// State is connected; interrupted, when the API server has not
	// answered since Since, for less than 30 s; or lost, for 30 s or
	// more (platformstate.WatchLostAfter).
	State string `json:"state"`
	// Since is when the API server stopped answering; absent when
	// connected.
	Since *time.Time `json:"since,omitempty"`
}

// Snapshot is the whole picture a browser gets when it connects.
type Snapshot struct {
	// At is the time of the snapshot.
	At time.Time `json:"at"`
	// RestartedAt is when this Argus started: the feed's seam.
	RestartedAt time.Time `json:"restartedAt"`
	// Ready is false until the first look at the cluster is complete;
	// until then the snapshot may be missing objects.
	Ready   bool         `json:"ready"`
	Cluster ClusterState `json:"cluster"`
	// Platform is where the card links out to.
	Platform     Platform                    `json:"platform"`
	Applications []platformstate.Application `json:"applications"`
	Components   []platformstate.Component   `json:"components"`
	// Feed is the recent notes, oldest first.
	Feed []Note `json:"feed"`
}

// Store holds what the informers keep, interprets it with platformstate,
// and streams the domain objects that change, and the notes, to every
// subscriber. It reads nothing itself: the informers put objects in, and
// the probe says whether the API server answers.
type Store struct {
	now func() time.Time
	log *slog.Logger

	mu sync.Mutex
	// objects are the cut-down objects by informer, then
	// namespace/name.
	objects map[string]map[string]any
	// outOfSyncSince is when an ArgoCD Application (namespace/name) was
	// seen turning OutOfSync.
	outOfSyncSince map[string]time.Time
	// deployedAt is when each Environment's image was deployed, as far
	// as this run has seen it (platform.go).
	deployedAt map[string]time.Time
	// platform is where the card links out to.
	platform Platform

	apps          map[string]platformstate.Application
	appJSON       map[string][]byte
	components    map[string]platformstate.Component
	componentJSON map[string][]byte

	feed        feed
	restartedAt time.Time
	seeded      bool

	brokenSince *time.Time
	cluster     ClusterState

	clients map[*client]struct{}
	wake    chan struct{}
}

// client is one browser's stream.
type client struct {
	ch chan []byte
}

// NewStore is an empty store; now is its clock. It starts connected.
func NewStore(now func() time.Time, log *slog.Logger) *Store {
	return &Store{
		now:            now,
		log:            log,
		objects:        map[string]map[string]any{},
		outOfSyncSince: map[string]time.Time{},
		deployedAt:     map[string]time.Time{},
		apps:           map[string]platformstate.Application{},
		appJSON:        map[string][]byte{},
		components:     map[string]platformstate.Component{},
		componentJSON:  map[string][]byte{},
		restartedAt:    now(),
		cluster:        ClusterState{State: Connected},
		clients:        map[*client]struct{}{},
		wake:           make(chan struct{}, 1),
	}
}

// Put keeps obj, one of platformstate's cut-down structs, as the object
// key (namespace/name) of the informer source.
func (s *Store) Put(source, key string, obj any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	bySource := s.objects[source]
	if bySource == nil {
		bySource = map[string]any{}
		s.objects[source] = bySource
	}
	old, had := bySource[key]
	if had && reflect.DeepEqual(old, obj) {
		return
	}
	bySource[key] = obj

	switch o := obj.(type) {
	case platformstate.ArgoCDApplication:
		// ArgoCD does not record when it turned OutOfSync; the watch sees
		// it. An Application first seen OutOfSync has no such time.
		k := o.Metadata.Namespace + "/" + o.Metadata.Name
		was, _ := old.(platformstate.ArgoCDApplication)
		switch {
		case o.Status.Sync.Status != "OutOfSync":
			delete(s.outOfSyncSince, k)
		case had && was.Status.Sync.Status != "OutOfSync":
			s.outOfSyncSince[k] = now
		}
	case platformstate.Event:
		// A new Warning Event is a feed entry, once the store has
		// seeded the feed with those from before it started. The Deploy
		// gate's refusals are the Deploy's own note.
		for other, byKey := range s.objects {
			if _, ok := byKey[key].(platformstate.Event); ok && other != source {
				had = true // another informer holds it too
			}
		}
		if !had && s.seeded && o.Type == "Warning" && o.Reason != platformstate.ReasonDeployRefused {
			s.publishNotes([]Note{eventNote(o, s.argoCDLocked())}, now)
		}
	}
	s.poke()
}

// Delete forgets the object key of source.
func (s *Store) Delete(source, key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if o, ok := s.objects[source][key].(platformstate.ArgoCDApplication); ok {
		delete(s.outOfSyncSince, o.Metadata.Namespace+"/"+o.Metadata.Name)
	}
	delete(s.objects[source], key)
	s.poke()
}

// poke asks Run to interpret the model again soon.
func (s *Store) poke() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// SetPlatform sets where the card links out to. Every snapshot carries
// it, and every Environment gets its links from it.
func (s *Store) SetPlatform(p Platform) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.platform = p
	s.poke()
}

// SetReachable records whether the API server answered the probe.
func (s *Store) SetReachable(ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case ok && s.brokenSince != nil:
		s.brokenSince = nil
		s.poke()
	case !ok && s.brokenSince == nil:
		at := s.now()
		s.brokenSince = &at
		s.poke()
	}
}

// Run interprets the model after every change, once it settles, and
// every TickEvery (tick, in tests), until ctx ends.
func (s *Store) Run(ctx context.Context, tick time.Duration) {
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-s.wake:
			select {
			case <-ctx.Done():
				return
			case <-time.After(settle):
			}
		}
		s.Recompute()
	}
}

// Recompute interprets the model at the store's now, and sends the
// Applications and components that changed, and the notes their changes
// make, to every subscriber. Before Seed it only keeps the model.
func (s *Store) Recompute() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	apps, components := build(gather(s.objects), s.outOfSyncSince, s.platform, s.deployedAt, now)
	quiet := !s.seeded
	var frames [][]byte
	var notes []Note

	names := sortedKeys(apps, s.apps)
	for _, name := range names {
		next, has := apps[name]
		prev, had := s.apps[name]
		if !has {
			frames = append(frames, frame("application-removed", map[string]string{"name": name}))
			delete(s.appJSON, name)
			if !quiet {
				notes = append(notes, applicationNotes(&prev, nil, now)...)
			}
			continue
		}
		data := mustJSON(next)
		if bytes.Equal(data, s.appJSON[name]) {
			continue
		}
		s.appJSON[name] = data
		frames = append(frames, rawFrame("application", data))
		if !quiet {
			var p *platformstate.Application
			if had {
				p = &prev
			}
			notes = append(notes, applicationNotes(p, &next, now)...)
		}
	}
	for _, name := range sortedKeys(components, s.components) {
		next, has := components[name]
		prev, had := s.components[name]
		if !has {
			frames = append(frames, frame("component-removed", map[string]string{"name": name}))
			delete(s.componentJSON, name)
			if !quiet {
				notes = append(notes, componentNotes(name, &prev, nil, now)...)
			}
			continue
		}
		data := mustJSON(next)
		if bytes.Equal(data, s.componentJSON[name]) {
			continue
		}
		s.componentJSON[name] = data
		frames = append(frames, rawFrame("component", data))
		if !quiet {
			var p *platformstate.Component
			if had {
				p = &prev
			}
			notes = append(notes, componentNotes(name, p, &next, now)...)
		}
	}
	s.apps, s.components = apps, components

	cluster := ClusterState{State: Connected}
	if s.brokenSince != nil {
		cluster = ClusterState{State: Interrupted, Since: s.brokenSince}
		if platformstate.WatchLost(s.brokenSince, now) {
			cluster.State = Lost
		}
	}
	if cluster.State != s.cluster.State {
		switch {
		case cluster.State == Lost:
			notes = append(notes, Note{At: now, Loudness: Normal, Message: "Argus has lost the cluster: what it shows is the last it saw"})
		case s.cluster.State == Lost:
			notes = append(notes, Note{At: now, Loudness: Quiet, FeedOnly: true, Message: "Argus sees the cluster again"})
		}
		s.cluster = cluster
		frames = append(frames, frame("cluster", cluster))
	}

	s.broadcast(frames)
	s.publishNotes(notes, now)
}

// publishNotes adds notes to the feed and sends them.
func (s *Store) publishNotes(notes []Note, now time.Time) {
	var frames [][]byte
	for _, n := range notes {
		frames = append(frames, frame("note", s.feed.add(n, now)))
	}
	s.broadcast(frames)
}

// broadcast sends frames to every subscriber. One that has fallen
// clientBuffer messages behind is closed instead: it reconnects, and a
// fresh snapshot catches it up.
func (s *Store) broadcast(frames [][]byte) {
	if len(frames) == 0 {
		return
	}
	for c := range s.clients {
		for _, f := range frames {
			select {
			case c.ch <- f:
				continue
			default:
			}
			close(c.ch)
			delete(s.clients, c)
			break
		}
	}
}

// subscribe is a new stream: its snapshot, and the channel every later
// message comes on, taken together so that nothing falls between them.
func (s *Store) subscribe() ([]byte, *client) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := &client{ch: make(chan []byte, clientBuffer)}
	s.clients[c] = struct{}{}
	return frame("snapshot", s.snapshotLocked()), c
}

// unsubscribe ends a stream.
func (s *Store) unsubscribe(c *client) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.clients[c]; ok {
		delete(s.clients, c)
		close(c.ch)
	}
}

// Snapshot is the whole picture now.
func (s *Store) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshotLocked()
}

func (s *Store) snapshotLocked() Snapshot {
	now := s.now()
	snap := Snapshot{
		At: now, RestartedAt: s.restartedAt, Ready: s.seeded, Cluster: s.cluster, Platform: s.platform,
		Applications: []platformstate.Application{}, Components: []platformstate.Component{},
		Feed: s.feed.list(now),
	}
	for _, name := range sortedKeys(s.apps, nil) {
		snap.Applications = append(snap.Applications, s.apps[name])
	}
	for _, name := range sortedKeys(s.components, nil) {
		snap.Components = append(snap.Components, s.components[name])
	}
	return snap
}

// Ready reports whether the first look at the cluster is complete.
func (s *Store) Ready() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seeded
}

// argoCDLocked are the ArgoCD Applications the store holds.
func (s *Store) argoCDLocked() []platformstate.ArgoCDApplication {
	var out []platformstate.ArgoCDApplication
	for _, byKey := range s.objects {
		for _, obj := range byKey {
			if a, ok := obj.(platformstate.ArgoCDApplication); ok {
				out = append(out, a)
			}
		}
	}
	return out
}

// eventNote is the feed entry for a Warning Event.
func eventNote(e platformstate.Event, argoCD []platformstate.ArgoCDApplication) Note {
	message := e.Regarding.Kind + " " + e.Regarding.Name + ": " + e.Reason
	if e.Note != "" {
		message += ": " + e.Note
	}
	if r := []rune(message); len(r) > 300 {
		message = string(r[:299]) + "…"
	}
	return Note{At: e.Time(), Place: placeOf(e, argoCD), Loudness: Quiet, FeedOnly: true, Message: message}
}

// sortedKeys are the keys of a and b together, sorted.
func sortedKeys[V any](a, b map[string]V) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range []map[string]V{a, b} {
		for k := range m {
			if !seen[k] {
				seen[k] = true
				out = append(out, k)
			}
		}
	}
	sort.Strings(out)
	return out
}

// frame is one server-sent event: its type, and v as JSON on one line.
func frame(event string, v any) []byte {
	return rawFrame(event, mustJSON(v))
}

func rawFrame(event string, data []byte) []byte {
	var b bytes.Buffer
	b.WriteString("event: ")
	b.WriteString(event)
	b.WriteString("\ndata: ")
	b.Write(data)
	b.WriteString("\n\n")
	return b.Bytes()
}

// mustJSON encodes v, which is always one of the model's own types.
func mustJSON(v any) []byte {
	data, err := json.Marshal(v)
	if err != nil {
		panic("argus: encoding " + reflect.TypeOf(v).String() + ": " + err.Error())
	}
	return data
}
