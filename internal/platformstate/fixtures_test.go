package platformstate_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Itema-as/iidp/internal/platformstate"
)

// The interpretation is tested from Kubernetes objects written the way the
// API server sends them, as maps decoded into platformstate's structs, so
// the fixtures also check that the structs read the API's own JSON names.
// Every time is relative to now, the injected clock: nothing sleeps.

var now = time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)

// ago is now minus d, as the API server writes a time.
func ago(d time.Duration) string {
	return now.Add(-d).Format(time.RFC3339)
}

var (
	c1 = strings.Repeat("1", 40)
	c2 = strings.Repeat("2", 40)
	c3 = strings.Repeat("3", 40)
	// chartRev is the chart source's revision in a multi-source
	// Application's revisions, next to the Platform repository's commit.
	chartRev = "0.4.0"
)

func decodeInto(t *testing.T, from any, into any) {
	t.Helper()
	data, err := json.Marshal(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, into); err != nil {
		t.Fatalf("decoding %s: %v", data, err)
	}
}

type obj = map[string]any

// fixture is one Environment of shop on the cluster.
type fixture struct {
	name        string
	app         obj
	deployments []obj
	pods        []obj
	jobs        []obj
	cronJobs    []obj
	clusters    []obj
	certs       []obj
	ingresses   []obj
	events      []obj
	outOfSync   *time.Time
}

// env is prod of shop: its ArgoCD Application Synced and Healthy since an
// hour ago, having synced c1, with the Deployment serving 1.0.0.
func env() *fixture {
	return &fixture{
		name:        "prod",
		app:         argoApp("prod", "Synced", "Healthy"),
		deployments: []obj{servedDeployment("1.0.0")},
		pods:        []obj{readyPod("shop-a", "1.0.0")},
	}
}

func (f *fixture) objects(t *testing.T) platformstate.Objects {
	t.Helper()
	var o platformstate.Objects
	decodeInto(t, f.app, &o.ArgoCD)
	for _, list := range []struct {
		from []obj
		into any
	}{
		{f.deployments, &o.Deployments}, {f.pods, &o.Pods}, {f.jobs, &o.Jobs}, {f.cronJobs, &o.CronJobs},
		{f.clusters, &o.PostgresClusters}, {f.certs, &o.Certificates}, {f.ingresses, &o.Ingresses}, {f.events, &o.Events},
	} {
		if len(list.from) > 0 {
			decodeInto(t, list.from, list.into)
		}
	}
	o.OutOfSyncSince = f.outOfSync
	return o
}

// state interprets the fixture at now.
func (f *fixture) state(t *testing.T) platformstate.EnvironmentState {
	t.Helper()
	app := platformstate.ApplicationOf("shop", []platformstate.Objects{f.objects(t)}, now)
	if len(app.Environments) != 1 {
		t.Fatalf("Environments = %+v", app.Environments)
	}
	return app.Environments[0]
}

func argoApp(environment, sync, health string) obj {
	return obj{
		"metadata": obj{
			"name": "shop-" + environment, "namespace": "argocd", "creationTimestamp": ago(24 * time.Hour),
			"labels": obj{"iidp.itema.no/application": "shop", "iidp.itema.no/environment": environment},
		},
		"spec": obj{"destination": obj{"namespace": "shop-" + environment}},
		"status": obj{
			"sync":         obj{"status": sync, "revisions": []any{chartRev, c1}},
			"health":       obj{"status": health},
			"reconciledAt": ago(time.Hour),
			"operationState": obj{
				"phase": "Succeeded", "message": "successfully synced (all tasks run)",
				"startedAt": ago(time.Hour + time.Minute), "finishedAt": ago(time.Hour),
				"operation":  obj{"sync": obj{"revisions": []any{chartRev, c1}}},
				"syncResult": obj{"revisions": []any{chartRev, c1}},
			},
			"history": []any{obj{"id": 1, "revisions": []any{chartRev, c1}, "deployedAt": ago(time.Hour)}},
		},
	}
}

func status(app obj) obj { return app["status"].(obj) }

// syncing makes ArgoCD's last operation a sync of commit in phase,
// started started ago; a finished phase finished at once.
func syncing(app obj, commit, phase, message string, started time.Duration) {
	op := obj{
		"phase": phase, "message": message, "startedAt": ago(started),
		"operation":  obj{"sync": obj{"revisions": []any{chartRev, commit}}},
		"syncResult": obj{"revisions": []any{chartRev, commit}},
	}
	if phase != "Running" && phase != "Terminating" {
		op["finishedAt"] = ago(started)
	}
	status(app)["operationState"] = op
}

// synced adds commit to ArgoCD's history, deployed d ago.
func synced(app obj, commit string, d time.Duration) {
	s := status(app)
	s["history"] = append(s["history"].([]any), obj{"id": len(s["history"].([]any)) + 1, "revisions": []any{chartRev, commit}, "deployedAt": ago(d)})
}

// looked makes ArgoCD compare against commit, d ago.
func looked(app obj, commit, sync string, d time.Duration) {
	s := status(app)
	s["sync"] = obj{"status": sync, "revisions": []any{chartRev, commit}}
	s["reconciledAt"] = ago(d)
}

func argoCondition(app obj, t, message string) {
	s := status(app)
	conditions, _ := s["conditions"].([]any)
	s["conditions"] = append(conditions, obj{"type": t, "message": message, "lastTransitionTime": ago(time.Minute)})
}

func deployment(tag string, generation, observed, replicas, updated, ready int, prog obj, available bool, availableSince time.Duration) obj {
	availableStatus := "True"
	if !available {
		availableStatus = "False"
	}
	return obj{
		"metadata": obj{"name": "shop", "namespace": "shop-prod", "generation": generation, "creationTimestamp": ago(24 * time.Hour),
			"labels": obj{"iidp.itema.no/application": "shop"}},
		"spec": obj{
			"replicas": 1,
			"selector": obj{"matchLabels": obj{"app.kubernetes.io/instance": "shop"}},
			"template": obj{"spec": obj{"containers": []any{obj{"name": "shop", "image": "ghcr.io/itema-as/shop:" + tag}}}},
		},
		"status": obj{
			"observedGeneration": observed, "replicas": replicas, "updatedReplicas": updated,
			"readyReplicas": ready, "availableReplicas": ready,
			"conditions": []any{
				obj{"type": "Available", "status": availableStatus, "lastTransitionTime": ago(availableSince)},
				prog,
			},
		},
	}
}

func progressing(status, reason string) obj {
	return obj{"type": "Progressing", "status": status, "reason": reason, "lastTransitionTime": ago(time.Minute)}
}

// servedDeployment has finished rolling tag out.
func servedDeployment(tag string) obj {
	return deployment(tag, 3, 3, 1, 1, 1, progressing("True", "NewReplicaSetAvailable"), true, time.Hour)
}

// rollingDeployment is rolling tag out, with a pod of the previous
// ReplicaSet still there.
func rollingDeployment(tag string) obj {
	return deployment(tag, 4, 4, 2, 1, 1, progressing("True", "ReplicaSetUpdated"), true, time.Hour)
}

// deadlineDeployment is a rollout of tag past its progress deadline.
func deadlineDeployment(tag string) obj {
	return deployment(tag, 4, 4, 2, 1, 1, progressing("False", "ProgressDeadlineExceeded"), true, time.Hour)
}

// firstDeployment is the first rollout of tag, created d ago, with
// nothing serving yet.
func firstDeployment(tag string, d time.Duration) obj {
	dep := deployment(tag, 1, 1, 1, 1, 0, progressing("True", "ReplicaSetUpdated"), false, d)
	dep["metadata"].(obj)["creationTimestamp"] = ago(d)
	return dep
}

func pod(name, tag string, ready bool, since time.Duration) obj {
	readyStatus := "False"
	if ready {
		readyStatus = "True"
	}
	return obj{
		"metadata": obj{"name": name, "namespace": "shop-prod", "creationTimestamp": ago(since),
			"labels": obj{"app.kubernetes.io/instance": "shop", "iidp.itema.no/application": "shop"}},
		"spec": obj{"containers": []any{obj{"name": "shop", "image": "ghcr.io/itema-as/shop:" + tag}}},
		"status": obj{
			"phase": "Running",
			"conditions": []any{
				obj{"type": "PodScheduled", "status": "True"},
				obj{"type": "Ready", "status": readyStatus, "lastTransitionTime": ago(since)},
			},
			"containerStatuses": []any{obj{"name": "shop", "image": "ghcr.io/itema-as/shop:" + tag, "ready": ready, "restartCount": 0,
				"state": obj{"running": obj{"startedAt": ago(since)}}}},
		},
	}
}

func readyPod(name, tag string) obj { return pod(name, tag, true, time.Hour) }

// notReadyPod has not been ready for d.
func notReadyPod(name, tag string, d time.Duration) obj { return pod(name, tag, false, d) }

// waitingPod is a pod created 20 s ago whose container waits for reason.
func waitingPod(name, tag, reason string) obj {
	p := pod(name, tag, false, 20*time.Second)
	status(p)["containerStatuses"].([]any)[0].(obj)["state"] = obj{"waiting": obj{"reason": reason}}
	return p
}

func oomKilledPod(name, tag string) obj {
	p := pod(name, tag, false, 10*time.Second)
	cs := status(p)["containerStatuses"].([]any)[0].(obj)
	cs["restartCount"] = 1
	cs["lastState"] = obj{"terminated": obj{"reason": "OOMKilled", "exitCode": 137}}
	return p
}

func unschedulablePod(name, tag string) obj {
	p := pod(name, tag, false, 10*time.Second)
	s := status(p)
	s["phase"] = "Pending"
	s["conditions"] = []any{obj{"type": "PodScheduled", "status": "False", "reason": "Unschedulable", "message": "0/1 nodes are available: 1 Insufficient memory."}}
	delete(s, "containerStatuses")
	return p
}

func unknownPod(name, tag string) obj {
	p := readyPod(name, tag)
	s := status(p)
	s["phase"] = "Unknown"
	s["conditions"] = []any{obj{"type": "Ready", "status": "Unknown", "lastTransitionTime": ago(time.Minute)}}
	return p
}

// job is one of the chart's Jobs of component running tag, created d
// ago; condition is Complete, Failed, or "" while it runs.
func job(component, tag string, d time.Duration, condition string) obj {
	st := obj{"startTime": ago(d)}
	if condition != "" {
		st["conditions"] = []any{obj{"type": condition, "status": "True", "lastTransitionTime": ago(d - time.Second)}}
	}
	return obj{
		"metadata": obj{"name": "shop-" + component, "namespace": "shop-prod", "creationTimestamp": ago(d),
			"labels": obj{"iidp.itema.no/application": "shop", "app.kubernetes.io/component": component}},
		"spec":   obj{"template": obj{"spec": obj{"containers": []any{obj{"name": component, "image": "ghcr.io/itema-as/shop:" + tag}}}}},
		"status": st,
	}
}

func migration(tag string, d time.Duration, condition string) obj {
	return job("migration", tag, d, condition)
}

// jobPod is a pod named name of the Job jobName, in phase, created a
// minute ago, labelled as the Job controller labels it.
func jobPod(name, jobName, phase string) obj {
	p := pod(name, "1.0.0", phase == "Running", time.Minute)
	p["metadata"].(obj)["labels"] = obj{"iidp.itema.no/application": "shop", "batch.kubernetes.io/job-name": jobName, "job-name": jobName}
	s := status(p)
	s["phase"] = phase
	if phase == "Pending" {
		s["conditions"] = []any{obj{"type": "PodScheduled", "status": "True"}}
		s["containerStatuses"] = []any{obj{"name": "shop", "ready": false, "state": obj{"waiting": obj{"reason": "ContainerCreating"}}}}
	}
	return p
}

// unschedulableJobPod is a pod of the Job jobName that the scheduler
// cannot place on the node.
func unschedulableJobPod(name, jobName string) obj {
	p := jobPod(name, jobName, "Pending")
	s := status(p)
	s["conditions"] = []any{obj{"type": "PodScheduled", "status": "False", "reason": "Unschedulable",
		"message": "0/1 nodes are available: 1 Insufficient cpu.", "lastTransitionTime": ago(time.Minute)}}
	delete(s, "containerStatuses")
	return p
}

// previewEnv is shop's Preview Environment pr-2, which its ApplicationSet
// made the given time ago with the image tag sha-new, with nothing of its
// own running yet.
func previewEnv(created time.Duration) *fixture {
	f := env()
	f.name = "pr-2"
	f.app = argoApp("pr-2", "OutOfSync", "Missing")
	f.app["metadata"].(obj)["creationTimestamp"] = ago(created)
	delete(f.app["metadata"].(obj)["labels"].(obj), "iidp.itema.no/environment")
	f.app["spec"].(obj)["sources"] = []any{
		obj{"repoURL": "https://github.com/Itema-as/iidp-platform.git", "ref": "values"},
		obj{"chart": "application", "helm": obj{"valueFiles": []any{"$values/applications/shop/staging/values.yaml"},
			"valuesObject": obj{"environment": "pr-2", "image": obj{"tag": "sha-new"}, "size": "small"}}},
	}
	delete(status(f.app), "history")
	f.deployments, f.pods = nil, nil
	return f
}

// gateEvent is the Deploy gate's Event, d ago, about prod.
func gateEvent(reason, tag, commit, kind, note string, d time.Duration) obj {
	annotations := obj{
		"iidp.itema.no/application": "shop", "iidp.itema.no/environment": "prod",
		"iidp.itema.no/tag": tag, "iidp.itema.no/kind": kind,
	}
	if commit != "" {
		annotations["iidp.itema.no/commit"] = commit
	}
	if reason == "DeployRefused" {
		annotations["iidp.itema.no/refusal"] = "422"
	}
	return obj{
		"metadata":  obj{"name": "shop-prod." + tag, "namespace": "argocd", "creationTimestamp": ago(d), "annotations": annotations},
		"eventTime": now.Add(-d).Format("2006-01-02T15:04:05.000000Z07:00"),
		"reason":    reason, "note": note, "type": "Normal",
		"action":              "Deploy",
		"reportingController": "iidp.itema.no/deploy-gate",
		"regarding":           obj{"kind": "Application", "namespace": "argocd", "name": "shop-prod", "apiVersion": "argoproj.io/v1alpha1"},
	}
}

func accepted(tag, commit string, d time.Duration) obj {
	return gateEvent("DeployAccepted", tag, commit, "deploy", "Deploy shop prod "+tag+" accepted", d)
}

func promoted(tag, commit string, d time.Duration) obj {
	return gateEvent("DeployAccepted", tag, commit, "promote", "Promote shop prod "+tag+" accepted", d)
}

func refused(tag, reason string, d time.Duration) obj {
	return gateEvent("DeployRefused", tag, "", "deploy", "Deploy shop prod refused: "+reason, d)
}

func pgCluster(phase string, conditions ...obj) obj {
	list := []any{}
	for _, c := range conditions {
		list = append(list, c)
	}
	return obj{
		"apiVersion": "postgresql.cnpg.io/v1", "kind": "Cluster",
		"metadata": obj{"name": "shop-db", "namespace": "shop-prod", "creationTimestamp": ago(24 * time.Hour)},
		"spec":     obj{"instances": 1},
		"status":   obj{"phase": phase, "instances": 1, "readyInstances": 1, "conditions": list},
	}
}

func cond(t, status, reason, message string, d time.Duration) obj {
	return obj{"type": t, "status": status, "reason": reason, "message": message, "lastTransitionTime": ago(d)}
}

func certificate(host string, notAfter *time.Time, conditions ...obj) obj {
	list := []any{}
	for _, c := range conditions {
		list = append(list, c)
	}
	st := obj{"conditions": list}
	if notAfter != nil {
		st["notAfter"] = notAfter.Format(time.RFC3339)
	}
	return obj{
		"apiVersion": "cert-manager.io/v1", "kind": "Certificate",
		"metadata": obj{"name": "shop-www", "namespace": "shop-prod", "creationTimestamp": ago(24 * time.Hour)},
		"spec":     obj{"dnsNames": []any{host}, "secretName": "shop-www-tls"},
		"status":   st,
	}
}
