package argus

import (
	"encoding/json"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/Itema-as/iidp/internal/platformstate"
)

// The store is tested with Kubernetes objects written the way the API
// server sends them, decoded into platformstate's structs as the
// informers' transforms do, and a clock the tests move: nothing sleeps
// for a rule's timeout.

// t0 is 12:00 in Oslo.
var t0 = time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)

type obj = map[string]any

// clock is the store's clock.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newStore(t *testing.T) (*Store, *clock) {
	t.Helper()
	c := &clock{t: t0}
	return NewStore(c.now, slog.New(slog.NewTextHandler(io.Discard, nil))), c
}

// at is t0 plus d, as the API server writes a time.
func at(d time.Duration) string { return t0.Add(d).Format(time.RFC3339) }

func decode[T any](t *testing.T, from obj) T {
	t.Helper()
	data, err := json.Marshal(from)
	if err != nil {
		t.Fatal(err)
	}
	var out T
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("decoding %s: %v", data, err)
	}
	return out
}

// put decodes each object by its kind and puts it in the store, as its
// informer would.
func put(t *testing.T, s *Store, objects ...obj) {
	t.Helper()
	for _, o := range objects {
		m := o["metadata"].(obj)
		key := m["namespace"].(string) + "/" + m["name"].(string)
		switch o["kind"] {
		case "Application":
			s.Put("applications", key, decode[platformstate.ArgoCDApplication](t, o))
		case "Deployment":
			s.Put("deployments", key, decode[platformstate.Deployment](t, o))
		case "Pod":
			s.Put("pods", key, decode[platformstate.Pod](t, o))
		case "Job":
			s.Put("jobs", key, decode[platformstate.Job](t, o))
		case "Certificate":
			s.Put("certificates", key, decode[platformstate.Certificate](t, o))
		case "Ingress":
			s.Put("ingresses", key, decode[platformstate.Ingress](t, o))
		case "Event":
			s.Put("events", key, decode[platformstate.Event](t, o))
		case "Node":
			s.Put("nodes", key, decode[platformstate.Node](t, o))
		default:
			t.Fatalf("put: no informer for %v", o["kind"])
		}
	}
}

const commit1 = "1111111111111111111111111111111111111111"

// argoApp is the ArgoCD Application of app's Environment env: Synced and
// Healthy, having synced commit1 an hour before t0.
func argoApp(app, env string) obj {
	return obj{
		"apiVersion": "argoproj.io/v1alpha1", "kind": "Application",
		"metadata": obj{
			"name": app + "-" + env, "namespace": "argocd", "creationTimestamp": at(-24 * time.Hour),
			"labels": obj{"iidp.itema.no/application": app, "iidp.itema.no/environment": env},
		},
		"spec": obj{"destination": obj{"namespace": app + "-" + env}},
		"status": obj{
			"sync":         obj{"status": "Synced", "revisions": []any{"0.4.0", commit1}},
			"health":       obj{"status": "Healthy"},
			"reconciledAt": at(-time.Hour),
			"operationState": obj{
				"phase": "Succeeded", "startedAt": at(-time.Hour - time.Minute), "finishedAt": at(-time.Hour),
				"operation":  obj{"sync": obj{"revisions": []any{"0.4.0", commit1}}},
				"syncResult": obj{"revisions": []any{"0.4.0", commit1}},
			},
			"history": []any{obj{"id": 1, "revisions": []any{"0.4.0", commit1}, "deployedAt": at(-time.Hour)}},
		},
	}
}

// component is a Platform component's ArgoCD Application, which
// platform-components manages.
func component(name string) obj {
	return obj{
		"apiVersion": "argoproj.io/v1alpha1", "kind": "Application",
		"metadata": obj{
			"name": name, "namespace": "argocd", "creationTimestamp": at(-30 * 24 * time.Hour),
			"annotations": obj{"argocd.argoproj.io/tracking-id": "platform-components:argoproj.io/Application:argocd/" + name},
		},
		"spec":   obj{"destination": obj{"namespace": name}},
		"status": obj{"sync": obj{"status": "Synced"}, "health": obj{"status": "Healthy"}},
	}
}

// deployment is a Deployment in namespace ns running tag, whose rollout
// has finished, selecting the pods labelled instance=name.
func deployment(ns, name, tag string, labels, annotations obj) obj {
	return obj{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": obj{"name": name, "namespace": ns, "generation": 2, "creationTimestamp": at(-24 * time.Hour),
			"labels": labels, "annotations": annotations},
		"spec": obj{
			"replicas": 1,
			"selector": obj{"matchLabels": obj{"app.kubernetes.io/instance": name}},
			"template": obj{"spec": obj{"containers": []any{obj{"name": name, "image": "ghcr.io/itema-as/" + name + ":" + tag}}}},
		},
		"status": obj{
			"observedGeneration": 2, "replicas": 1, "updatedReplicas": 1, "readyReplicas": 1, "availableReplicas": 1,
			"conditions": []any{
				obj{"type": "Available", "status": "True", "lastTransitionTime": at(-time.Hour)},
				obj{"type": "Progressing", "status": "True", "reason": "NewReplicaSetAvailable", "lastTransitionTime": at(-time.Hour)},
			},
		},
	}
}

// envDeployment is app env's workload.
func envDeployment(app, env, tag string) obj {
	return deployment(app+"-"+env, app, tag, obj{"iidp.itema.no/application": app, "iidp.itema.no/environment": env}, nil)
}

// pod is a pod in ns that deployment name selects, running tag, ready or
// not since since.
func pod(ns, name, deployment, tag string, ready bool, since time.Duration) obj {
	status := "False"
	if ready {
		status = "True"
	}
	return obj{
		"apiVersion": "v1", "kind": "Pod",
		"metadata": obj{"name": name, "namespace": ns, "creationTimestamp": at(-24 * time.Hour),
			"labels": obj{"app.kubernetes.io/instance": deployment}},
		"spec": obj{"containers": []any{obj{"name": deployment, "image": "ghcr.io/itema-as/" + deployment + ":" + tag}}},
		"status": obj{
			"phase": "Running",
			"conditions": []any{
				obj{"type": "PodScheduled", "status": "True"},
				obj{"type": "Ready", "status": status, "lastTransitionTime": at(since)},
			},
			"containerStatuses": []any{obj{"name": deployment, "ready": ready, "restartCount": 0, "state": obj{"running": obj{}}}},
		},
	}
}

// gateEvent is the Deploy gate's Event about app env at d from t0.
func gateEvent(app, env, reason, kind, tag, commit, note string, d time.Duration) obj {
	annotations := obj{"iidp.itema.no/application": app, "iidp.itema.no/environment": env, "iidp.itema.no/tag": tag, "iidp.itema.no/kind": kind}
	if commit != "" {
		annotations["iidp.itema.no/commit"] = commit
	}
	typ := "Normal"
	if reason == platformstate.ReasonDeployRefused {
		typ = "Warning"
	}
	return obj{
		"kind":     "Event",
		"metadata": obj{"name": app + "-" + env + "." + tag, "namespace": "argocd", "creationTimestamp": at(d), "annotations": annotations},
		"reason":   reason, "note": note, "type": typ, "eventTime": t0.Add(d).Format("2006-01-02T15:04:05.000000Z07:00"),
		"regarding": obj{"kind": "Application", "namespace": "argocd", "name": app + "-" + env},
	}
}

// warning is a Warning Event about a pod in ns at d from t0.
func warning(ns, name, reason, note string, d time.Duration) obj {
	return obj{
		"kind":     "Event",
		"metadata": obj{"name": name + "." + reason, "namespace": ns, "creationTimestamp": at(d)},
		"reason":   reason, "note": note, "type": "Warning", "deprecatedLastTimestamp": at(d),
		"regarding": obj{"kind": "Pod", "namespace": ns, "name": name},
	}
}

// servingShop is shop's prod on the cluster, serving 1.0.0.
func servingShop() []obj {
	return []obj{
		argoApp("shop", "prod"),
		envDeployment("shop", "prod", "1.0.0"),
		pod("shop-prod", "shop-a", "shop", "1.0.0", true, -time.Hour),
	}
}
