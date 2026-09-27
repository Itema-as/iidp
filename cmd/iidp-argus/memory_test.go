package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	krt "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"

	"github.com/Itema-as/iidp/internal/argus"
)

// TestMemoryWithThirtyEnvironments measures what Argus holds for a
// synthetic Platform of 15 Applications with prod and staging each: 30
// Environments with their Deployments, pods, Jobs, CronJobs, Postgres,
// certificates, Ingresses and Events, the Platform components' own
// workloads in argocd and kube-system, and the node. Every object is as
// fat as the API server's: managedFields, full pod specs, ArgoCD's
// resource list and history.
//
// The real dynamic client reads them over HTTP from apiServer, which
// serves pre-encoded JSON the way the API server does: list, watch, and
// the streaming watch-list client-go 0.36 tries first. It reports the heap
// Argus's informers and model hold once synced, the highest the heap went
// on the way, and the same for the informers without transforms. The
// implementation note records the figures
// (docs/implementation-notes/118-argus-backend.md). It only runs with
// IIDP_ARGUS_MEASURE=1.
func TestMemoryWithThirtyEnvironments(t *testing.T) {
	if os.Getenv("IIDP_ARGUS_MEASURE") != "1" {
		t.Skip("set IIDP_ARGUS_MEASURE=1 to measure")
	}
	objects := syntheticPlatform(15)
	count := map[string]int{}
	for _, o := range objects {
		count[o.(*unstructured.Unstructured).GetKind()]++
	}
	srv, requests := apiServer(t, objects)
	objects = nil
	t.Logf("synthetic Platform: %v", count)

	measure := func(name string, start func(ctx context.Context, client dynamic.Interface) func()) uint64 {
		client, err := dynamic.NewForConfig(&rest.Config{Host: srv.URL, QPS: 1000, Burst: 1000})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		before := heap()
		peak, stop := samplePeak()
		keep := start(ctx, client)
		stop()
		after := heap()
		keep()
		t.Logf("%s: the heap in use grew by %.1f MiB once synced, and by at most %.1f MiB on the way", name, mib(after-before), mib(*peak-before))
		return after - before
	}

	stripped := measure("informers with transforms, the store and a snapshot", func(ctx context.Context, client dynamic.Interface) func() {
		store := argus.NewStore(time.Now, discard)
		informers := startInformers(ctx, client, store, discard)
		waitForSync(ctx, informers, time.Minute, discard)
		store.Seed()
		store.Recompute()
		snapshot := store.Snapshot()
		// The measurement is of the whole Platform, read and interpreted.
		envs, caps := 0, 0
		for _, a := range snapshot.Applications {
			for _, e := range a.Environments {
				envs++
				caps += len(e.Capabilities)
			}
		}
		if len(snapshot.Applications) != 15 || envs != 30 || caps != 30*4 || len(snapshot.Components) != 13 {
			t.Errorf("the model has %d Applications, %d Environments, %d Capabilities and %d components; want 15, 30, 120 and 13",
				len(snapshot.Applications), envs, caps, len(snapshot.Components))
		}
		return func() { runtime.KeepAlive(informers); runtime.KeepAlive(snapshot) }
	})
	unstripped := measure("the same informers without transforms", func(ctx context.Context, client dynamic.Interface) func() {
		var informers []cache.SharedIndexInformer
		for _, src := range sources {
			inf := newInformer(client, src)
			go inf.RunWithContext(ctx)
			informers = append(informers, inf)
		}
		waitForSync(ctx, informers, time.Minute, discard)
		return func() { runtime.KeepAlive(informers) }
	})
	t.Logf("requests: %v", requests())
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	t.Logf("this test process: %.1f MiB of heap in use, %.1f MiB obtained from the OS (Sys), the stand-in API server's JSON included", mib(heap()), mib(m.Sys))
	if stripped >= unstripped {
		t.Errorf("the transforms saved nothing: %d bytes with, %d without", stripped, unstripped)
	}
}

// apiServer serves objects as the Kubernetes API does, enough for
// informers: a list or a watch of a resource, cluster-wide or in one
// namespace, narrowed by an "exists" label selector and by the one field
// selector Argus uses. A watch that asks for initial events (a watch-list)
// gets them, then the bookmark that ends them; any watch then stays open
// with nothing more to say. requests counts the requests by kind.
func apiServer(t *testing.T, objects []krt.Object) (srv *httptest.Server, requests func() map[string]int) {
	t.Helper()
	var mu sync.Mutex
	counts := map[string]int{}
	type stored struct {
		namespace, typ string
		labels         map[string]string
		json           []byte
	}
	byResource := map[string][]stored{}
	kinds := map[string]string{}
	for _, o := range objects {
		u := o.(*unstructured.Unstructured)
		resource := strings.ToLower(u.GetKind()) + "s"
		if u.GetKind() == "Ingress" {
			resource = "ingresses"
		}
		data, err := u.MarshalJSON()
		if err != nil {
			t.Fatal(err)
		}
		typ, _, _ := unstructured.NestedString(u.Object, "type")
		byResource[resource] = append(byResource[resource], stored{u.GetNamespace(), typ, u.GetLabels(), data})
		kinds[resource] = u.GetAPIVersion() + " " + u.GetKind()
	}
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		kind := "list"
		switch {
		case r.URL.Query().Get("sendInitialEvents") == "true":
			kind = "watch-list"
		case r.URL.Query().Get("watch") == "true":
			kind = "watch"
		}
		mu.Lock()
		counts[kind]++
		mu.Unlock()
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		resource, namespace := parts[len(parts)-1], ""
		if len(parts) >= 3 && parts[len(parts)-3] == "namespaces" {
			namespace = parts[len(parts)-2]
		}
		q := r.URL.Query()
		label, fields := q.Get("labelSelector"), q.Get("fieldSelector")
		var items [][]byte
		for _, o := range byResource[resource] {
			if namespace != "" && o.namespace != namespace {
				continue
			}
			if _, ok := o.labels[label]; label != "" && !ok {
				continue
			}
			if fields != "" && (o.typ != "Warning" || o.namespace == "argocd") {
				continue
			}
			items = append(items, o.json)
		}
		apiVersion, kind, _ := strings.Cut(kinds[resource], " ")
		w.Header().Set("Content-Type", "application/json")
		if q.Get("watch") != "true" {
			fmt.Fprintf(w, `{"apiVersion":%q,"kind":"%sList","metadata":{"resourceVersion":"1"},"items":[`, apiVersion, kind)
			for i, item := range items {
				if i > 0 {
					_, _ = w.Write([]byte(","))
				}
				_, _ = w.Write(item)
			}
			_, _ = w.Write([]byte("]}"))
			return
		}
		if q.Get("sendInitialEvents") == "true" {
			for _, item := range items {
				fmt.Fprintf(w, "{\"type\":\"ADDED\",\"object\":%s}\n", item)
			}
			fmt.Fprintf(w, "{\"type\":\"BOOKMARK\",\"object\":{\"apiVersion\":%q,\"kind\":%q,\"metadata\":{\"resourceVersion\":\"1\",\"annotations\":{\"k8s.io/initial-events-end\":\"true\"}}}}\n", apiVersion, kind)
		}
		_ = http.NewResponseController(w).Flush()
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	return srv, func() map[string]int {
		mu.Lock()
		defer mu.Unlock()
		out := map[string]int{}
		for k, v := range counts {
			out[k] = v
		}
		return out
	}
}

// heap is the heap in use after a full collection.
func heap() uint64 {
	for i := 0; i < 3; i++ {
		runtime.GC()
	}
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapInuse
}

// samplePeak samples the heap in use every millisecond until stopped, and
// keeps the highest.
func samplePeak() (*uint64, func()) {
	var peak uint64
	done, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		var m runtime.MemStats
		for {
			runtime.ReadMemStats(&m)
			if m.HeapInuse > peak {
				peak = m.HeapInuse
			}
			select {
			case <-done:
				return
			case <-time.After(time.Millisecond):
			}
		}
	}()
	return &peak, func() { close(done); <-stopped }
}

func mib(b uint64) float64 { return float64(b) / (1 << 20) }

// managedFields is a managedFields list the size a real object carries.
func managedFields() []any {
	fields := obj{}
	for i := 0; i < 40; i++ {
		fields[fmt.Sprintf("f:field%02d", i)] = obj{"f:nested": obj{}, "f:other": obj{}}
	}
	return []any{
		obj{"manager": "argocd-controller", "operation": "Apply", "apiVersion": "v1", "time": ts(-time.Hour), "fieldsType": "FieldsV1", "fieldsV1": fields},
		obj{"manager": "k3s", "operation": "Update", "apiVersion": "v1", "time": ts(-time.Hour), "fieldsType": "FieldsV1", "fieldsV1": fields, "subresource": "status"},
	}
}

func meta(name, ns string, labels obj, annotations obj) obj {
	m := obj{"name": name, "namespace": ns, "uid": name + "-uid", "resourceVersion": "12345", "creationTimestamp": ts(-24 * time.Hour),
		"labels": labels, "managedFields": managedFields()}
	if annotations == nil {
		annotations = obj{}
	}
	if _, ok := annotations["argocd.argoproj.io/tracking-id"]; !ok {
		annotations["argocd.argoproj.io/tracking-id"] = "applications:apps/Deployment:" + ns + "/" + name
	}
	m["annotations"] = annotations
	return m
}

func fullPodSpec(app, image string) obj {
	return obj{
		"serviceAccountName": "default", "terminationGracePeriodSeconds": 35, "dnsPolicy": "ClusterFirst", "restartPolicy": "Always",
		"securityContext": obj{"runAsNonRoot": true, "seccompProfile": obj{"type": "RuntimeDefault"}, "fsGroup": 1000},
		"volumes":         []any{obj{"name": "tmp", "emptyDir": obj{}}, obj{"name": "kube-api-access", "projected": obj{"sources": []any{obj{"serviceAccountToken": obj{"path": "token"}}}}}},
		"tolerations": []any{obj{"key": "node.kubernetes.io/not-ready", "operator": "Exists", "effect": "NoExecute", "tolerationSeconds": 300},
			obj{"key": "node.kubernetes.io/unreachable", "operator": "Exists", "effect": "NoExecute", "tolerationSeconds": 300}},
		"containers": []any{obj{"name": app, "image": image, "imagePullPolicy": "IfNotPresent",
			"ports": []any{obj{"name": "http", "containerPort": 8080, "protocol": "TCP"}},
			"env": []any{obj{"name": "PORT", "value": "8080"}, obj{"name": "DATABASE_URL", "valueFrom": obj{"secretKeyRef": obj{"name": app + "-db-app", "key": "uri"}}},
				obj{"name": "NODE_ENV", "value": "production"}, obj{"name": "OTEL_SERVICE_NAME", "value": app}},
			"resources":                obj{"requests": obj{"cpu": "250m", "memory": "256Mi"}, "limits": obj{"memory": "256Mi"}},
			"readinessProbe":           obj{"httpGet": obj{"path": "/", "port": "http"}, "periodSeconds": 10},
			"livenessProbe":            obj{"httpGet": obj{"path": "/", "port": "http"}, "periodSeconds": 10},
			"lifecycle":                obj{"preStop": obj{"sleep": obj{"seconds": 5}}},
			"securityContext":          obj{"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true, "capabilities": obj{"drop": []any{"ALL"}}},
			"volumeMounts":             []any{obj{"name": "tmp", "mountPath": "/tmp"}},
			"terminationMessagePath":   "/dev/termination-log",
			"terminationMessagePolicy": "File"}},
	}
}

// syntheticPlatform is apps Applications with prod and staging, and the
// Platform's own objects.
func syntheticPlatform(apps int) []krt.Object {
	var out []krt.Object
	add := func(o obj) { out = append(out, u(o)) }
	sha := strings.Repeat("a", 40)
	for i := 0; i < apps; i++ {
		app := fmt.Sprintf("app%02d", i)
		for _, env := range []string{"prod", "staging"} {
			ns := app + "-" + env
			labels := obj{"iidp.itema.no/application": app, "iidp.itema.no/environment": env, "app.kubernetes.io/instance": app,
				"app.kubernetes.io/name": app, "app.kubernetes.io/managed-by": "Helm", "helm.sh/chart": "application-0.4.0"}
			image := "ghcr.io/itema-as/" + app + ":1.0.0"
			var history, resources []any
			for h := 0; h < 10; h++ {
				history = append(history, obj{"id": h, "revisions": []any{"0.4.0", sha}, "deployedAt": ts(-time.Duration(h) * time.Hour),
					"deployStartedAt": ts(-time.Duration(h) * time.Hour), "initiatedBy": obj{"automated": true},
					"sources": []any{obj{"repoURL": "oci://ghcr.io/itema-as/charts", "chart": "application", "targetRevision": "0.4.0"}, obj{"repoURL": "https://github.com/Itema-as/iidp-platform.git", "ref": "values"}}})
			}
			for _, k := range []string{"Service", "Deployment", "Ingress", "Cluster", "ObjectStore", "ScheduledBackup", "CronJob", "ConfigMap", "ServiceAccount", "Role", "RoleBinding", "Secret"} {
				resources = append(resources, obj{"kind": k, "name": app, "namespace": ns, "version": "v1", "status": "Synced", "health": obj{"status": "Healthy"}})
			}
			add(obj{"apiVersion": "argoproj.io/v1alpha1", "kind": "Application",
				"metadata": meta(ns, "argocd", obj{"iidp.itema.no/application": app, "iidp.itema.no/environment": env}, nil),
				"spec": obj{"project": "default", "destination": obj{"server": "https://kubernetes.default.svc", "namespace": ns},
					"sources": []any{obj{"repoURL": "oci://ghcr.io/itema-as/charts", "chart": "application", "targetRevision": "0.4.0", "helm": obj{"valueFiles": []any{"$values/applications/" + app + "/" + env + "/values.yaml"}}},
						obj{"repoURL": "https://github.com/Itema-as/iidp-platform.git", "targetRevision": "HEAD", "ref": "values"}},
					"syncPolicy": obj{"automated": obj{"prune": true, "selfHeal": true}, "syncOptions": []any{"CreateNamespace=true", "ServerSideApply=true"}}},
				"status": obj{"sync": obj{"status": "Synced", "revisions": []any{"0.4.0", sha}, "comparedTo": obj{"destination": obj{"namespace": ns}}},
					"health": obj{"status": "Healthy", "lastTransitionTime": ts(-time.Hour)}, "reconciledAt": ts(-time.Minute),
					"operationState": obj{"phase": "Succeeded", "message": "successfully synced (all tasks run)", "startedAt": ts(-time.Hour), "finishedAt": ts(-time.Hour),
						"operation":  obj{"sync": obj{"revisions": []any{"0.4.0", sha}}, "initiatedBy": obj{"automated": true}},
						"syncResult": obj{"revisions": []any{"0.4.0", sha}, "resources": resources}},
					"history": history, "resources": resources,
					"summary": obj{"externalURLs": []any{"https://" + app + ".app.itma.no"}, "images": []any{image}}}})
			add(obj{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": meta(app, ns, labels, nil),
				"spec": obj{"replicas": 1, "selector": obj{"matchLabels": obj{"app.kubernetes.io/instance": app}},
					"template": obj{"metadata": obj{"labels": labels}, "spec": fullPodSpec(app, image)}},
				"status": obj{"observedGeneration": 1, "replicas": 1, "updatedReplicas": 1, "readyReplicas": 1, "availableReplicas": 1,
					"conditions": []any{obj{"type": "Available", "status": "True", "lastUpdateTime": ts(-time.Hour), "lastTransitionTime": ts(-time.Hour), "reason": "MinimumReplicasAvailable", "message": "Deployment has minimum availability."},
						obj{"type": "Progressing", "status": "True", "lastUpdateTime": ts(-time.Hour), "lastTransitionTime": ts(-time.Hour), "reason": "NewReplicaSetAvailable", "message": "ReplicaSet has successfully progressed."}}}})
			for p := 0; p < 2; p++ {
				add(podObject(fmt.Sprintf("%s-%d", app, p), ns, labels, fullPodSpec(app, image), image))
			}
			for _, j := range []string{"migrate", "report-1", "report-2", "report-3"} {
				component := "scheduled-task"
				if j == "migrate" {
					component = "migration"
				}
				jl := obj{"iidp.itema.no/application": app, "iidp.itema.no/environment": env, "app.kubernetes.io/component": component, "iidp.itema.no/task": "report"}
				add(obj{"apiVersion": "batch/v1", "kind": "Job", "metadata": meta(app+"-"+j, ns, jl, nil),
					"spec":   obj{"backoffLimit": 0, "template": obj{"metadata": obj{"labels": jl}, "spec": fullPodSpec(app, image)}},
					"status": obj{"startTime": ts(-time.Hour), "completionTime": ts(-time.Hour), "succeeded": 1, "conditions": []any{obj{"type": "Complete", "status": "True", "lastTransitionTime": ts(-time.Hour)}}}})
				add(podObject(app+"-"+j+"-x", ns, jl, fullPodSpec(app, image), image))
			}
			cl := obj{"iidp.itema.no/application": app, "iidp.itema.no/environment": env, "app.kubernetes.io/component": "scheduled-task", "iidp.itema.no/task": "report"}
			add(obj{"apiVersion": "batch/v1", "kind": "CronJob", "metadata": meta(app+"-report", ns, cl, nil),
				"spec":   obj{"schedule": "0 3 * * *", "timeZone": "Europe/Oslo", "jobTemplate": obj{"spec": obj{"template": obj{"spec": fullPodSpec(app, image)}}}},
				"status": obj{"lastScheduleTime": ts(-time.Hour), "lastSuccessfulTime": ts(-time.Hour)}})
			add(obj{"apiVersion": "postgresql.cnpg.io/v1", "kind": "Cluster", "metadata": meta(app+"-db", ns, labels, nil),
				"spec": obj{"instances": 1, "storage": obj{"size": "1Gi"}, "plugins": []any{obj{"name": "barman-cloud.cloudnative-pg.io", "parameters": obj{"barmanObjectName": app}}}},
				"status": obj{"phase": "Cluster in healthy state", "instances": 1, "readyInstances": 1, "currentPrimary": app + "-db-1",
					"certificates": obj{"expirations": obj{app + "-db-ca": ts(90 * 24 * time.Hour), app + "-db-server": ts(90 * 24 * time.Hour)}},
					"conditions": []any{obj{"type": "Ready", "status": "True", "reason": "ClusterIsReady", "message": "Cluster is Ready", "lastTransitionTime": ts(-time.Hour)},
						obj{"type": "ContinuousArchiving", "status": "True", "lastTransitionTime": ts(-time.Hour)}, obj{"type": "LastBackupSucceeded", "status": "True", "lastTransitionTime": ts(-time.Hour)}}}})
			add(podObject(app+"-db-1", ns, labels, fullPodSpec(app, "ghcr.io/cloudnative-pg/postgresql:17"), "ghcr.io/cloudnative-pg/postgresql:17"))
			add(obj{"apiVersion": "cert-manager.io/v1", "kind": "Certificate", "metadata": meta(app+"-www-tls", ns, labels, nil),
				"spec":   obj{"dnsNames": []any{"www." + app + ".example"}, "secretName": app + "-www-tls", "issuerRef": obj{"name": "letsencrypt-http01", "kind": "ClusterIssuer"}},
				"status": obj{"notAfter": ts(60 * 24 * time.Hour), "renewalTime": ts(30 * 24 * time.Hour), "conditions": []any{obj{"type": "Ready", "status": "True", "lastTransitionTime": ts(-time.Hour)}}}})
			for _, ing := range []string{app, app + "-http01"} {
				add(obj{"apiVersion": "networking.k8s.io/v1", "kind": "Ingress",
					"metadata": meta(ing, ns, labels, obj{"traefik.ingress.kubernetes.io/router.middlewares": "oauth2-proxy-itema-login-auth@kubernetescrd", "traefik.ingress.kubernetes.io/router.entrypoints": "websecure"}),
					"spec":     obj{"ingressClassName": "traefik", "rules": []any{obj{"host": app + ".app.itma.no"}}, "tls": []any{obj{"hosts": []any{app + ".app.itma.no"}}}}})
			}
			// ArgoCD's own Events about the Environment in the last hour,
			// and the Deploy gate's.
			for e := 0; e < 8; e++ {
				add(eventObject(fmt.Sprintf("%s.%d", ns, e), "argocd", "Normal", "ResourceUpdated", "Updated sync status: OutOfSync -> Synced", "Application", ns))
			}
			add(eventObject(ns+".gate", "argocd", "Normal", "DeployAccepted", "Deploy "+app+" "+env+" 1.0.0 accepted", "Application", ns))
			add(eventObject(ns+".warn", ns, "Warning", "Unhealthy", "Readiness probe failed: connection refused", "Pod", app+"-0"))
		}
	}
	// The Platform's own workloads.
	for _, name := range []string{"argocd-server", "argocd-repo-server", "argocd-redis", "argocd-dex-server", "argocd-applicationset-controller", "argocd-notifications-controller", "iidp-deploy-gate"} {
		l := obj{"app.kubernetes.io/instance": name, "app.kubernetes.io/part-of": "argocd"}
		add(obj{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": meta(name, "argocd", l, nil),
			"spec":   obj{"replicas": 1, "selector": obj{"matchLabels": l}, "template": obj{"metadata": obj{"labels": l}, "spec": fullPodSpec(name, "quay.io/argoproj/argocd:v3.5.3")}},
			"status": obj{"replicas": 1, "readyReplicas": 1, "updatedReplicas": 1, "availableReplicas": 1}})
		add(podObject(name+"-1", "argocd", l, fullPodSpec(name, "quay.io/argoproj/argocd:v3.5.3"), "quay.io/argoproj/argocd:v3.5.3"))
	}
	for _, name := range []string{"traefik", "coredns", "metrics-server", "local-path-provisioner"} {
		l := obj{"app.kubernetes.io/instance": name}
		add(obj{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": meta(name, "kube-system", l, nil),
			"spec":   obj{"replicas": 1, "selector": obj{"matchLabels": l}, "template": obj{"metadata": obj{"labels": l}, "spec": fullPodSpec(name, "rancher/"+name+":1")}},
			"status": obj{"replicas": 1, "readyReplicas": 1, "updatedReplicas": 1, "availableReplicas": 1}})
		add(podObject(name+"-1", "kube-system", l, fullPodSpec(name, "rancher/"+name+":1"), "rancher/"+name+":1"))
	}
	for _, name := range []string{"argocd", "cert-manager", "external-dns", "cloudnative-pg", "cnpg-barman-cloud", "monitoring", "oauth2-proxy", "guardrails", "platform-tls", "deploy-gate", "argus"} {
		add(obj{"apiVersion": "argoproj.io/v1alpha1", "kind": "Application",
			"metadata": meta(name, "argocd", nil, obj{"argocd.argoproj.io/tracking-id": "platform-components:argoproj.io/Application:argocd/" + name}),
			"spec":     obj{"destination": obj{"namespace": name}}, "status": obj{"sync": obj{"status": "Synced"}, "health": obj{"status": "Healthy"}}})
	}
	var images []any
	for i := 0; i < 80; i++ {
		images = append(images, obj{"names": []any{fmt.Sprintf("ghcr.io/itema-as/app%02d@sha256:%s", i, strings.Repeat("b", 64)), fmt.Sprintf("ghcr.io/itema-as/app%02d:1.0.0", i)}, "sizeBytes": 123456789})
	}
	add(obj{"apiVersion": "v1", "kind": "Node", "metadata": meta("iidp", "", obj{"kubernetes.io/hostname": "iidp"}, nil),
		"status": obj{"images": images, "nodeInfo": obj{"kubeletVersion": "v1.36.4+k3s1"},
			"conditions": []any{obj{"type": "Ready", "status": "True", "lastHeartbeatTime": ts(0), "lastTransitionTime": ts(-24 * time.Hour)}}}})
	return out
}

func podObject(name, ns string, labels obj, spec obj, image string) obj {
	return obj{"apiVersion": "v1", "kind": "Pod", "metadata": meta(name, ns, labels, nil), "spec": spec,
		"status": obj{"phase": "Running", "podIP": "10.42.0.9", "hostIP": "10.0.0.2", "qosClass": "Guaranteed", "startTime": ts(-time.Hour),
			"conditions": []any{obj{"type": "Initialized", "status": "True", "lastTransitionTime": ts(-time.Hour)}, obj{"type": "Ready", "status": "True", "lastTransitionTime": ts(-time.Hour)},
				obj{"type": "ContainersReady", "status": "True", "lastTransitionTime": ts(-time.Hour)}, obj{"type": "PodScheduled", "status": "True", "lastTransitionTime": ts(-time.Hour)}},
			"containerStatuses": []any{obj{"name": "c", "image": image, "imageID": image + "@sha256:" + strings.Repeat("c", 64), "containerID": "containerd://" + strings.Repeat("d", 64),
				"ready": true, "started": true, "restartCount": 0, "state": obj{"running": obj{"startedAt": ts(-time.Hour)}}}}}}
}

func eventObject(name, ns, typ, reason, message, kind, about string) obj {
	return obj{"apiVersion": "v1", "kind": "Event", "metadata": meta(name, ns, nil, obj{"iidp.itema.no/application": "x"}),
		"reason": reason, "message": message, "type": typ, "count": 1, "firstTimestamp": ts(-time.Minute), "lastTimestamp": ts(-time.Minute),
		"source": obj{"component": "argocd-application-controller"}, "reportingComponent": "argocd-application-controller",
		"involvedObject": obj{"kind": kind, "namespace": ns, "name": about, "apiVersion": "v1", "uid": "u", "resourceVersion": "1"}}
}
