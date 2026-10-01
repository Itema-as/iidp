package main

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"github.com/Itema-as/iidp/internal/argus"
	"github.com/Itema-as/iidp/internal/platformstate"
)

// listKinds are the list kinds of every resource Argus reads, which the
// fake dynamic client needs.
var listKinds = map[schema.GroupVersionResource]string{
	applicationsGVR: "ApplicationList", deploymentsGVR: "DeploymentList", podsGVR: "PodList", jobsGVR: "JobList",
	cronJobsGVR: "CronJobList", eventsGVR: "EventList", ingressesGVR: "IngressList", clustersGVR: "ClusterList",
	certificatesGVR: "CertificateList", nodesGVR: "NodeList",
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

var t0 = time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)

func ts(d time.Duration) string { return t0.Add(d).Format(time.RFC3339) }

// u is o as the dynamic client decodes it from the API server's JSON.
func u(o obj) *unstructured.Unstructured {
	data, err := json.Marshal(o)
	if err != nil {
		panic(err)
	}
	out := &unstructured.Unstructured{}
	if err := out.UnmarshalJSON(data); err != nil {
		panic(err)
	}
	return out
}

// shopProd is shop's prod Environment serving 1.0.0: its ArgoCD
// Application, its Deployment and its one pod, whose Ready condition is ready
// and last changed at since.
func shopProd(ready bool, since time.Duration) []*unstructured.Unstructured {
	status := "False"
	if ready {
		status = "True"
	}
	labels := obj{"iidp.itema.no/application": "shop", "iidp.itema.no/environment": "prod", "app.kubernetes.io/instance": "shop"}
	return []*unstructured.Unstructured{
		u(obj{"apiVersion": "argoproj.io/v1alpha1", "kind": "Application",
			"metadata": obj{"name": "shop-prod", "namespace": "argocd", "creationTimestamp": ts(-24 * time.Hour), "labels": obj{"iidp.itema.no/application": "shop", "iidp.itema.no/environment": "prod"}},
			"spec":     obj{"destination": obj{"namespace": "shop-prod"}},
			"status": obj{"sync": obj{"status": "Synced", "revisions": []any{"0.4.0", strings.Repeat("1", 40)}}, "health": obj{"status": "Healthy"},
				"reconciledAt": ts(-time.Hour),
				"history":      []any{obj{"id": 1, "revisions": []any{"0.4.0", strings.Repeat("1", 40)}, "deployedAt": ts(-time.Hour)}}}}),
		u(obj{"apiVersion": "apps/v1", "kind": "Deployment",
			"metadata": obj{"name": "shop", "namespace": "shop-prod", "generation": 2, "creationTimestamp": ts(-24 * time.Hour), "labels": labels},
			"spec": obj{"replicas": 1, "selector": obj{"matchLabels": obj{"app.kubernetes.io/instance": "shop"}},
				"template": obj{"spec": obj{"containers": []any{obj{"name": "shop", "image": "ghcr.io/itema-as/shop:1.0.0"}}}}},
			"status": obj{"observedGeneration": 2, "replicas": 1, "updatedReplicas": 1, "readyReplicas": 1, "availableReplicas": 1,
				"conditions": []any{obj{"type": "Progressing", "status": "True", "reason": "NewReplicaSetAvailable"}}}}),
		u(obj{"apiVersion": "v1", "kind": "Pod",
			"metadata": obj{"name": "shop-a", "namespace": "shop-prod", "creationTimestamp": ts(-24 * time.Hour), "labels": labels},
			"spec":     obj{"containers": []any{obj{"name": "shop", "image": "ghcr.io/itema-as/shop:1.0.0"}}},
			"status": obj{"phase": "Running",
				"conditions":        []any{obj{"type": "Ready", "status": status, "lastTransitionTime": ts(since)}},
				"containerStatuses": []any{obj{"name": "shop", "ready": ready, "state": obj{"running": obj{}}}}}}),
	}
}

// From the cluster to the browser: informers on a fake dynamic client,
// through the transforms and platformstate, to the stream. A Deployment
// whose pod goes unready becomes a Degraded delta once 60 s have passed,
// and the Deploy gate's Event becomes a Deploy at Accepted.
func TestInformersToStream(t *testing.T) {
	var initial []runtime.Object
	for _, o := range shopProd(true, -time.Hour) {
		initial = append(initial, o)
	}
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds, initial...)
	clock := &fakeClock{t: t0}
	store := argus.NewStore(clock.now, discard)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go store.Run(ctx, 10*time.Millisecond)
	waitForSync(ctx, startInformers(ctx, client, store, discard), 10*time.Second, discard)
	store.Seed()

	srv := httptest.NewServer((&argus.Server{Store: store, Web: fstest.MapFS{}}).Handler())
	t.Cleanup(srv.Close)
	events := readStream(t, ctx, srv.URL+"/events")

	var snap argus.Snapshot
	decodeEvent(t, next(t, events, "snapshot"), &snap)
	if len(snap.Applications) != 1 || snap.Applications[0].Environments[0].Condition.State != platformstate.Healthy {
		t.Fatalf("snapshot = %+v, want shop prod Healthy", snap.Applications)
	}

	// The pod stops being ready now.
	unready := shopProd(false, 0)[2]
	if _, err := client.Resource(podsGVR).Namespace("shop-prod").Update(ctx, unready, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	env := func() platformstate.EnvironmentState {
		var app platformstate.Application
		decodeEvent(t, next(t, events, "application"), &app)
		return app.Environments[0]
	}
	if e := env(); e.Pods.Ready != 0 || e.Condition.State != platformstate.Healthy {
		t.Fatalf("delta within the grace = pods %+v, %+v; want none ready, still Healthy", e.Pods, e.Condition)
	}
	clock.add(61 * time.Second)
	if e := env(); e.Condition.State != platformstate.Degraded || e.Condition.Reason != "0 of 1 pods ready for over a minute" {
		t.Fatalf("delta after 61 s = %+v, want Degraded", e.Condition)
	}
	var n argus.Note
	decodeEvent(t, next(t, events, "note"), &n)
	if n.Loudness != argus.Loud || n.Place.Application != "shop" || !strings.Contains(n.Message, "Degraded") {
		t.Errorf("note = %+v, want a loud Degraded", n)
	}

	// The Deploy gate accepts a Deploy: an Event in argocd, read through
	// core v1.
	event := u(obj{"apiVersion": "v1", "kind": "Event",
		"metadata": obj{"name": "shop-prod.1", "namespace": "argocd", "creationTimestamp": clock.now().Format(time.RFC3339),
			"annotations": obj{"iidp.itema.no/application": "shop", "iidp.itema.no/environment": "prod", "iidp.itema.no/tag": "1.0.1",
				"iidp.itema.no/kind": "deploy", "iidp.itema.no/commit": strings.Repeat("2", 40)}},
		"reason": "DeployAccepted", "message": "Deploy shop prod 1.0.1 accepted", "type": "Normal",
		"eventTime":      clock.now().Format("2006-01-02T15:04:05.000000Z07:00"),
		"involvedObject": obj{"apiVersion": "argoproj.io/v1alpha1", "kind": "Application", "namespace": "argocd", "name": "shop-prod"}})
	if _, err := client.Resource(eventsGVR).Namespace("argocd").Create(ctx, event, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	e := env()
	if len(e.Deploys) != 1 || e.Deploys[0].Tag != "1.0.1" || e.Deploys[0].Hop != platformstate.HopAccepted || e.Deploys[0].Commit != strings.Repeat("2", 40) {
		t.Fatalf("Deploys = %+v, want 1.0.1 at Accepted", e.Deploys)
	}
	if e.Activity == nil || e.Activity.State != platformstate.Deploying || e.Activity.Deploy == nil || e.Activity.Deploy.Hop != platformstate.HopAccepted {
		t.Errorf("Activity = %+v, want Deploying at Accepted", e.Activity)
	}
	decodeEvent(t, next(t, events, "note"), &n)
	if n.Message != "Deploy 1.0.1 to shop prod accepted" || n.Loudness != argus.Quiet {
		t.Errorf("note = %+v, want the Deploy accepted, quiet", n)
	}
}

type sse struct{ event, data string }

// readStream reads a stream's events as they come.
func readStream(t *testing.T, ctx context.Context, url string) <-chan sse {
	t.Helper()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	out := make(chan sse, 100)
	go func() {
		defer close(out)
		r := bufio.NewReader(resp.Body)
		var m sse
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimSuffix(line, "\n")
			switch {
			case line == "" && m.event != "":
				out <- m
				m = sse{}
			case strings.HasPrefix(line, "event: "):
				m.event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				m.data = strings.TrimPrefix(line, "data: ")
			}
		}
	}()
	return out
}

// next is the next event, which must be of type event.
func next(t *testing.T, events <-chan sse, event string) string {
	t.Helper()
	select {
	case m, ok := <-events:
		if !ok {
			t.Fatal("the stream ended")
		}
		if m.event != event {
			t.Fatalf("event = %s %s, want %s", m.event, m.data, event)
		}
		return m.data
	case <-time.After(5 * time.Second):
		t.Fatalf("no %s within 5 s", event)
	}
	return ""
}

func decodeEvent(t *testing.T, data string, into any) {
	t.Helper()
	if err := json.Unmarshal([]byte(data), into); err != nil {
		t.Fatalf("decoding %s: %v", data, err)
	}
}
