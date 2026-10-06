package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/tools/cache"

	"github.com/Itema-as/iidp/internal/platformstate"
)

type obj = map[string]any

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

// fat adds what the API server sends with every object and Argus never
// reads: managedFields, a last-applied annotation as large as the object,
// and ArgoCD's and Helm's bookkeeping.
func fat(o obj) *unstructured.Unstructured {
	m := o["metadata"].(obj)
	m["uid"], m["resourceVersion"] = "0b1c", "42"
	m["managedFields"] = []any{obj{"manager": "argocd-controller", "operation": "Apply", "fieldsType": "FieldsV1",
		"fieldsV1": obj{"f:spec": obj{"f:template": obj{"f:spec": obj{"f:containers": obj{}}}}}}}
	annotations, _ := m["annotations"].(obj)
	if annotations == nil {
		annotations = obj{}
	}
	annotations["kubectl.kubernetes.io/last-applied-configuration"] = strings.Repeat("x", 4096)
	annotations["meta.helm.sh/release-name"] = "shop"
	m["annotations"] = annotations
	return &unstructured.Unstructured{Object: o}
}

// podSpec is a full pod spec, of which Argus keeps the containers' names
// and images.
func podSpec(image string) obj {
	return obj{
		"serviceAccountName": "default", "terminationGracePeriodSeconds": 35,
		"securityContext": obj{"runAsNonRoot": true, "seccompProfile": obj{"type": "RuntimeDefault"}},
		"volumes":         []any{obj{"name": "tmp", "emptyDir": obj{}}},
		"tolerations":     []any{obj{"key": "node.kubernetes.io/not-ready", "operator": "Exists", "effect": "NoExecute"}},
		"containers": []any{obj{"name": "shop", "image": image,
			"env":            []any{obj{"name": "DATABASE_URL", "valueFrom": obj{"secretKeyRef": obj{"name": "shop-db-app", "key": "uri"}}}},
			"resources":      obj{"requests": obj{"cpu": "250m", "memory": "256Mi"}, "limits": obj{"memory": "256Mi"}},
			"readinessProbe": obj{"httpGet": obj{"path": "/", "port": 8080}},
			"lifecycle":      obj{"preStop": obj{"sleep": obj{"seconds": 5}}},
		}},
	}
}

// Each informer's transform keeps what the model needs, as platformstate's
// cut-down struct, and drops managedFields, full specs, status the model
// does not read, and every annotation but its own.
func TestTransformsKeepOnlyWhatTheModelReads(t *testing.T) {
	iidpLabels := obj{"iidp.itema.no/application": "shop", "iidp.itema.no/environment": "prod", "app.kubernetes.io/instance": "shop"}
	for _, tc := range []struct {
		source string
		in     *unstructured.Unstructured
		want   any    // the kept struct's type
		keeps  string // JSON the kept object must contain
		drops  []string
	}{
		{"applications", fat(obj{"apiVersion": "argoproj.io/v1alpha1", "kind": "Application",
			"metadata": obj{"name": "shop-prod", "namespace": "argocd", "labels": obj{"iidp.itema.no/application": "shop"},
				"annotations": obj{"argocd.argoproj.io/tracking-id": "applications:argoproj.io/Application:argocd/shop-prod"}},
			"spec": obj{"project": "default", "destination": obj{"server": "https://kubernetes.default.svc", "namespace": "shop-prod"},
				"sources": []any{obj{"repoURL": "ghcr.io/itema-as/charts", "chart": "application", "targetRevision": "0.4.0",
					"helm": obj{"valueFiles": []any{"$values/applications/shop/prod/values.yaml"}}}},
				"syncPolicy": obj{"automated": obj{"prune": true}}},
			"status": obj{"sync": obj{"status": "Synced", "revisions": []any{"0.4.0", "abc"}, "comparedTo": obj{"source": obj{"repoURL": "x"}}},
				"health": obj{"status": "Healthy"}, "reconciledAt": "2026-09-28T09:00:00Z",
				"resources": []any{obj{"kind": "Deployment", "name": "shop", "status": "Synced", "health": obj{"status": "Healthy"}}},
				"history":   []any{obj{"id": 1, "revisions": []any{"0.4.0", "abc"}, "deployedAt": "2026-09-28T09:00:00Z", "sources": []any{obj{"repoURL": "x"}}}},
				"summary":   obj{"externalURLs": []any{"https://shop.app.itma.no"}, "images": []any{"ghcr.io/itema-as/shop:1.0.0"}}}}),
			platformstate.ArgoCDApplication{}, `"history":[{"id":1,"revision":"","revisions":["0.4.0","abc"],"deployedAt":"2026-09-28T09:00:00Z"}]`,
			[]string{"resources", "comparedTo", "syncPolicy", "valueFiles", "repoURL", "last-applied", "managedFields"}},
		{"deployments", fat(obj{"apiVersion": "apps/v1", "kind": "Deployment",
			"metadata": obj{"name": "shop", "namespace": "shop-prod", "labels": iidpLabels, "generation": 3},
			"spec": obj{"replicas": 1, "revisionHistoryLimit": 10, "selector": obj{"matchLabels": obj{"app.kubernetes.io/instance": "shop"}},
				"strategy": obj{"type": "RollingUpdate"}, "template": obj{"metadata": obj{"labels": iidpLabels}, "spec": podSpec("ghcr.io/itema-as/shop:1.0.0")}},
			"status": obj{"observedGeneration": 3, "replicas": 1, "updatedReplicas": 1, "readyReplicas": 1, "availableReplicas": 1,
				"conditions": []any{obj{"type": "Available", "status": "True", "lastUpdateTime": "2026-09-28T09:00:00Z"}}}}),
			platformstate.Deployment{}, `"containers":[{"name":"shop","image":"ghcr.io/itema-as/shop:1.0.0"}]`,
			[]string{"volumes", "tolerations", "DATABASE_URL", "resources", "readinessProbe", "revisionHistoryLimit", "strategy", "lastUpdateTime", "last-applied", "helm", "managedFields"}},
		{"pods", fat(obj{"apiVersion": "v1", "kind": "Pod",
			"metadata": obj{"name": "shop-a", "namespace": "shop-prod", "labels": iidpLabels,
				"ownerReferences": []any{obj{"kind": "ReplicaSet", "name": "shop-5d8"}}},
			"spec": podSpec("ghcr.io/itema-as/shop:1.0.0"),
			"status": obj{"phase": "Running", "podIP": "10.42.0.9", "hostIP": "10.0.0.2", "qosClass": "Guaranteed",
				"conditions": []any{obj{"type": "Ready", "status": "True", "lastTransitionTime": "2026-09-28T09:00:00Z", "lastProbeTime": nil}},
				"containerStatuses": []any{obj{"name": "shop", "image": "ghcr.io/itema-as/shop:1.0.0", "imageID": "ghcr.io/itema-as/shop@sha256:1",
					"containerID": "containerd://1", "ready": true, "restartCount": 2, "started": true,
					"state": obj{"running": obj{"startedAt": "2026-09-28T09:00:00Z"}}, "lastState": obj{"terminated": obj{"reason": "OOMKilled", "exitCode": 137}}}}}}),
			platformstate.Pod{}, `"lastState":{"waiting":null,"running":null,"terminated":{"reason":"OOMKilled","exitCode":137}}`,
			[]string{"podIP", "hostIP", "qosClass", "imageID", "containerID", "ownerReferences", "volumes", "DATABASE_URL", "last-applied", "managedFields"}},
		{"jobs", fat(obj{"apiVersion": "batch/v1", "kind": "Job",
			"metadata": obj{"name": "shop-migrate", "namespace": "shop-prod", "labels": obj{"iidp.itema.no/application": "shop", "app.kubernetes.io/component": "migration"}},
			"spec":     obj{"backoffLimit": 0, "template": obj{"spec": podSpec("ghcr.io/itema-as/shop:1.0.1")}},
			"status":   obj{"startTime": "2026-09-28T09:00:00Z", "failed": 1, "uncountedTerminatedPods": obj{}, "conditions": []any{obj{"type": "Failed", "status": "True"}}}}),
			platformstate.Job{}, `"image":"ghcr.io/itema-as/shop:1.0.1"`,
			[]string{"backoffLimit", "uncountedTerminatedPods", "volumes", "last-applied", "managedFields"}},
		{"cronjobs", fat(obj{"apiVersion": "batch/v1", "kind": "CronJob",
			"metadata": obj{"name": "shop-report", "namespace": "shop-prod", "labels": obj{"iidp.itema.no/task": "report", "app.kubernetes.io/component": "scheduled-task"}},
			"spec": obj{"schedule": "0 3 * * *", "timeZone": "Europe/Oslo", "concurrencyPolicy": "Forbid",
				"jobTemplate": obj{"spec": obj{"template": obj{"spec": podSpec("ghcr.io/itema-as/shop:1.0.0")}}}},
			"status": obj{"lastScheduleTime": "2026-09-28T01:00:00Z", "active": []any{obj{"name": "shop-report-1"}}}}),
			platformstate.CronJob{}, `"schedule":"0 3 * * *"`,
			[]string{"concurrencyPolicy", "jobTemplate", "active", "volumes", "last-applied", "managedFields"}},
		{"clusters", fat(obj{"apiVersion": "postgresql.cnpg.io/v1", "kind": "Cluster",
			"metadata": obj{"name": "shop-db", "namespace": "shop-prod", "labels": iidpLabels},
			"spec":     obj{"instances": 1, "storage": obj{"size": "1Gi"}, "plugins": []any{obj{"name": "barman-cloud.cloudnative-pg.io"}}},
			"status": obj{"phase": "Cluster in healthy state", "currentPrimary": "shop-db-1", "certificates": obj{"expirations": obj{"shop-db-ca": "2026-12-01"}},
				"conditions": []any{obj{"type": "LastBackupSucceeded", "status": "False", "message": "exit status 2"}}}}),
			platformstate.PostgresCluster{}, `"phase":"Cluster in healthy state"`,
			[]string{"storage", "plugins", "currentPrimary", "expirations", "last-applied", "managedFields"}},
		// The chart's database access levels stay, for the detail card.
		{"clusters", fat(obj{"apiVersion": "postgresql.cnpg.io/v1", "kind": "Cluster",
			"metadata": obj{"name": "shop-db", "namespace": "shop-prod", "labels": iidpLabels,
				"annotations": obj{"iidp.itema.no/db-access-read-write": "push", "iidp.itema.no/db-access-read-only": "none"}},
			"spec": obj{"instances": 1, "managed": obj{"roles": []any{obj{"name": "shop_write", "passwordSecret": obj{"name": "shop-db-write"}}}}}}),
			platformstate.PostgresCluster{}, `"iidp.itema.no/db-access-read-write":"push"`,
			[]string{"managed", "shop_write", "last-applied", "managedFields"}},
		{"certificates", fat(obj{"apiVersion": "cert-manager.io/v1", "kind": "Certificate",
			"metadata": obj{"name": "shop-www-tls", "namespace": "shop-prod"},
			"spec":     obj{"dnsNames": []any{"www.shop.example"}, "secretName": "shop-www-tls", "issuerRef": obj{"name": "letsencrypt-http01"}},
			"status":   obj{"notAfter": "2026-12-01T00:00:00Z", "renewalTime": "2026-11-01T00:00:00Z", "revision": 3}}),
			platformstate.Certificate{}, `"dnsNames":["www.shop.example"]`,
			[]string{"secretName", "issuerRef", "renewalTime", "last-applied", "managedFields"}},
		{"ingresses", fat(obj{"apiVersion": "networking.k8s.io/v1", "kind": "Ingress",
			"metadata": obj{"name": "shop", "namespace": "shop-prod", "labels": iidpLabels,
				"annotations": obj{"traefik.ingress.kubernetes.io/router.middlewares": "oauth2-proxy-itema-login-auth@kubernetescrd", "traefik.ingress.kubernetes.io/router.entrypoints": "websecure"}},
			"spec":   obj{"ingressClassName": "traefik", "rules": []any{obj{"host": "shop.app.itma.no"}}, "tls": []any{obj{"hosts": []any{"shop.app.itma.no"}}}},
			"status": obj{"loadBalancer": obj{"ingress": []any{obj{"ip": "10.0.0.2"}}}}}),
			platformstate.Ingress{}, `"annotations":{"traefik.ingress.kubernetes.io/router.middlewares":"oauth2-proxy-itema-login-auth@kubernetescrd"}`,
			[]string{"entrypoints", "ingressClassName", "shop.app.itma.no", "loadBalancer", "last-applied", "managedFields"}},
		{"events-argocd", fat(obj{"apiVersion": "v1", "kind": "Event",
			"metadata": obj{"name": "shop-prod.18a", "namespace": "argocd", "creationTimestamp": "2026-09-28T09:00:00Z",
				"annotations": obj{"iidp.itema.no/application": "shop", "iidp.itema.no/environment": "prod", "iidp.itema.no/tag": "1.0.1", "iidp.itema.no/commit": "abc"}},
			"reason": "DeployAccepted", "message": "Deploy shop prod 1.0.1 accepted", "type": "Normal", "action": "Deploy",
			"eventTime": "2026-09-28T09:00:00.123456Z", "reportingComponent": "iidp.itema.no/deploy-gate", "reportingInstance": "iidp-deploy-gate-1",
			"involvedObject": obj{"apiVersion": "argoproj.io/v1alpha1", "kind": "Application", "namespace": "argocd", "name": "shop-prod"},
			"source":         obj{"component": "iidp.itema.no/deploy-gate"}, "count": 1}),
			platformstate.Event{}, `"note":"Deploy shop prod 1.0.1 accepted","type":"Normal","eventTime":"2026-09-28T09:00:00.123456Z","deprecatedLastTimestamp":null,"regarding":{"kind":"Application","namespace":"argocd","name":"shop-prod"}`,
			[]string{"reportingInstance", "reportingComponent", "action", "apiVersion", "source", "last-applied", "managedFields"}},
		// The kubelet's Events have no eventTime, only the older
		// timestamps.
		{"events-warnings", fat(obj{"apiVersion": "v1", "kind": "Event",
			"metadata": obj{"name": "shop-a.17f", "namespace": "shop-prod", "creationTimestamp": "2026-09-28T09:00:00Z"},
			"reason":   "BackOff", "message": "Back-off restarting failed container shop", "type": "Warning", "count": 12,
			"eventTime": nil, "firstTimestamp": "2026-09-28T08:00:00Z", "lastTimestamp": "2026-09-28T09:00:00Z",
			"involvedObject": obj{"apiVersion": "v1", "kind": "Pod", "namespace": "shop-prod", "name": "shop-a", "fieldPath": "spec.containers{shop}"},
			"source":         obj{"component": "kubelet", "host": "iidp"}}),
			platformstate.Event{}, `"note":"Back-off restarting failed container shop","type":"Warning","eventTime":null,"deprecatedLastTimestamp":"2026-09-28T09:00:00Z","regarding":{"kind":"Pod","namespace":"shop-prod","name":"shop-a"}`,
			[]string{"firstTimestamp", "count", "fieldPath", "kubelet", "last-applied", "managedFields"}},
		{"nodes", fat(obj{"apiVersion": "v1", "kind": "Node",
			"metadata": obj{"name": "iidp", "labels": obj{"node-role.kubernetes.io/control-plane": "true"}},
			"spec":     obj{"podCIDR": "10.42.0.0/24", "providerID": "k3s://iidp"},
			"status": obj{"capacity": obj{"cpu": "4"}, "images": []any{obj{"names": []any{"ghcr.io/itema-as/shop:1.0.0"}, "sizeBytes": 1}},
				"nodeInfo":   obj{"kubeletVersion": "v1.36.4+k3s1", "osImage": "Ubuntu 24.04", "containerRuntimeVersion": "containerd://2.0"},
				"conditions": []any{obj{"type": "Ready", "status": "True", "lastHeartbeatTime": "2026-09-28T09:00:00Z", "lastTransitionTime": "2026-09-01T00:00:00Z"}}}}),
			platformstate.Node{}, `"conditions":[{"type":"Ready","status":"True","reason":"","message":"","lastTransitionTime":"2026-09-01T00:00:00Z"}],"nodeInfo":{"kubeletVersion":"v1.36.4+k3s1"}`,
			[]string{"podCIDR", "providerID", "capacity", "images", "osImage", "containerRuntimeVersion", "lastHeartbeatTime", "last-applied", "managedFields"}},
	} {
		t.Run(tc.source, func(t *testing.T) {
			src := sourceNamed(t, tc.source)
			out, err := transform(src, discard)(tc.in)
			if err != nil {
				t.Fatal(err)
			}
			k, ok := out.(*kept)
			if !ok {
				t.Fatalf("transform gave %T, want *kept", out)
			}
			if k.Name != tc.in.GetName() || k.Namespace != tc.in.GetNamespace() || k.ResourceVersion != "42" || len(k.ManagedFields) != 0 || len(k.Annotations) != 0 || len(k.Labels) != 0 {
				t.Errorf("kept metadata = %+v, want only the name, namespace, uid and resourceVersion", k.ObjectMeta)
			}
			if reflect.TypeOf(k.Object) != reflect.TypeOf(tc.want) {
				t.Fatalf("kept %T, want %T", k.Object, tc.want)
			}
			data, err := json.Marshal(k.Object)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(data), tc.keeps) {
				t.Errorf("kept %s\nwant it to contain %s", data, tc.keeps)
			}
			for _, d := range tc.drops {
				if strings.Contains(string(data), d) {
					t.Errorf("kept %s\nwant %q dropped", data, d)
				}
			}
			// Idempotent, as client-go asks, and a tombstone's object is
			// transformed too.
			if again, _ := transform(src, discard)(k); again != k {
				t.Errorf("transforming a kept object gave %v, want it unchanged", again)
			}
			tomb, _ := transform(src, discard)(cache.DeletedFinalStateUnknown{Key: "k", Obj: tc.in})
			if d, ok := tomb.(cache.DeletedFinalStateUnknown); !ok || reflect.TypeOf(d.Obj.(*kept).Object) != reflect.TypeOf(tc.want) {
				t.Errorf("tombstone = %#v", tomb)
			}
		})
	}
}

// An object that does not decode is kept empty, not an error that would
// stop its informer.
func TestAnObjectThatDoesNotDecodeIsLeftOut(t *testing.T) {
	bad := &unstructured.Unstructured{Object: obj{"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": obj{"name": "odd", "namespace": "shop-prod"}, "spec": obj{"replicas": "one"}}}
	out, err := transform(sourceNamed(t, "deployments"), discard)(bad)
	if err != nil {
		t.Fatal(err)
	}
	if k := out.(*kept); k.Object != nil || k.Name != "odd" {
		t.Errorf("kept = %+v, want the name and no object", k)
	}
}

// Every source's resource is one the ClusterRole grants, and nothing is
// read that it does not (bootstrap/bootstrap_test.go checks the ClusterRole).
func TestSourcesReadOnlyTheGrantedResources(t *testing.T) {
	granted := map[string]bool{
		"argoproj.io/applications": true, "apps/deployments": true, "/pods": true, "batch/jobs": true, "batch/cronjobs": true,
		"/events": true, "networking.k8s.io/ingresses": true, "postgresql.cnpg.io/clusters": true, "cert-manager.io/certificates": true, "/nodes": true,
	}
	seen := map[string]bool{}
	for _, src := range sources {
		r := src.gvr.Group + "/" + src.gvr.Resource
		if !granted[r] {
			t.Errorf("source %s reads %s, which the ClusterRole does not grant", src.name, r)
		}
		seen[r] = true
		if src.namespace == "" && src.labels == "" && src.fields == "" && r != "cert-manager.io/certificates" && r != "/nodes" {
			t.Errorf("source %s reads every %s in the cluster; narrow it by label, namespace or field", src.name, r)
		}
	}
	for r := range granted {
		if !seen[r] {
			t.Errorf("the ClusterRole grants %s, which no source reads", r)
		}
	}
}

func sourceNamed(t *testing.T, name string) source {
	t.Helper()
	for _, s := range sources {
		if s.name == name {
			return s
		}
	}
	t.Fatalf("no source %s", name)
	return source{}
}
