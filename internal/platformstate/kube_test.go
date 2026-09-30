package platformstate_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Itema-as/iidp/internal/platformstate"
)

// Kube is the Deploy gate's whole Kubernetes client, so it is tested
// against a stand-in API server: the bearer token, the label selector and
// the decoding of a list, and a refusal.
func TestKubeListsWithTheTokenAndTheSelector(t *testing.T) {
	var gotAuth, gotSelector string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotSelector = r.Header.Get("Authorization"), r.URL.Query().Get("labelSelector")
		switch r.URL.Path {
		case "/apis/argoproj.io/v1alpha1/namespaces/argocd/applications":
			_, _ = w.Write([]byte(`{"kind":"ApplicationList","items":[{"metadata":{"name":"shop-prod","labels":{"iidp.itema.no/application":"shop","iidp.itema.no/environment":"prod"}},
				"spec":{"destination":{"namespace":"shop-prod"}},"status":{"sync":{"status":"Synced"},"health":{"status":"Healthy"}}}]}`))
		case "/apis/batch/v1/namespaces/shop-prod/cronjobs":
			http.Error(w, `{"kind":"Status","reason":"Forbidden"}`, http.StatusForbidden)
		default:
			_, _ = w.Write([]byte(`{"items":null}`))
		}
	}))
	defer api.Close()
	tokens := 0
	kube := &platformstate.Kube{BaseURL: api.URL, Token: func() (string, error) { tokens++; return "sa-token", nil }}

	var apps []platformstate.ArgoCDApplication
	if err := kube.List(context.Background(), "/apis/argoproj.io/v1alpha1/namespaces/argocd/applications", "iidp.itema.no/application=shop", &apps); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer sa-token" || gotSelector != "iidp.itema.no/application=shop" {
		t.Errorf("Authorization = %q, labelSelector = %q", gotAuth, gotSelector)
	}
	if len(apps) != 1 || apps[0].Metadata.Name != "shop-prod" || apps[0].Status.Sync.Status != "Synced" {
		t.Errorf("apps = %+v", apps)
	}

	// Read lists everything for the Environment and reports a refusal.
	_, err := platformstate.Read(context.Background(), kube, "shop")
	var refusal *platformstate.StatusError
	if !errors.As(err, &refusal) || refusal.Status != http.StatusForbidden || !strings.Contains(err.Error(), "cronjobs") {
		t.Errorf("Read = %v, want the API server's 403 for the CronJobs", err)
	}
	if tokens < 4 {
		t.Errorf("the token was read %d times, want once per request: the kubelet rotates it", tokens)
	}
}

// Read, the List path iidp app status takes, interprets what it lists:
// an Environment ArgoCD has had for years with nothing deployed is
// Unreleased, and one crash-looping is Degraded.
func TestReadInterpretsEachEnvironment(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/apis/argoproj.io/v1alpha1/namespaces/argocd/applications":
			_, _ = w.Write([]byte(`{"items":[
				{"metadata":{"name":"shop-prod","creationTimestamp":"2020-01-01T00:00:00Z","labels":{"iidp.itema.no/application":"shop","iidp.itema.no/environment":"prod"}},
				 "spec":{"destination":{"namespace":"shop-prod"}},"status":{"sync":{"status":"Synced"},"health":{"status":"Healthy"}}},
				{"metadata":{"name":"shop-staging","creationTimestamp":"2020-01-01T00:00:00Z","labels":{"iidp.itema.no/application":"shop","iidp.itema.no/environment":"staging"}},
				 "spec":{"destination":{"namespace":"shop-staging"}},"status":{"sync":{"status":"Synced"},"health":{"status":"Healthy"}}}]}`))
		case "/apis/apps/v1/namespaces/shop-prod/deployments":
			_, _ = w.Write([]byte(`{"items":[{"metadata":{"name":"shop","generation":2},"spec":{"selector":{"matchLabels":{"app":"shop"}},
				"template":{"spec":{"containers":[{"name":"shop","image":"ghcr.io/itema-as/shop:1.0.0"}]}}},
				"status":{"observedGeneration":2,"replicas":1,"updatedReplicas":1,"conditions":[{"type":"Progressing","status":"True","reason":"NewReplicaSetAvailable"}]}}]}`))
		case "/api/v1/namespaces/shop-prod/pods":
			_, _ = w.Write([]byte(`{"items":[{"metadata":{"name":"shop-a","labels":{"app":"shop"}},"spec":{"containers":[{"name":"shop","image":"ghcr.io/itema-as/shop:1.0.0"}]},
				"status":{"phase":"Running","conditions":[{"type":"Ready","status":"False"}],
				"containerStatuses":[{"name":"shop","ready":false,"restartCount":5,"state":{"waiting":{"reason":"CrashLoopBackOff"}}}]}}]}`))
		default:
			_, _ = w.Write([]byte(`{"items":[]}`))
		}
	}))
	defer api.Close()

	envs, err := platformstate.Read(context.Background(), &platformstate.Kube{BaseURL: api.URL}, "shop")
	if err != nil {
		t.Fatal(err)
	}
	prod, staging := envs[0], envs[1]
	if prod.Condition == nil || prod.Condition.State != platformstate.Degraded || prod.Activity != nil {
		t.Errorf("prod = %+v, %+v; want Degraded with nothing changing", prod.Condition, prod.Activity)
	}
	if staging.Condition == nil || staging.Condition.State != platformstate.Healthy || staging.Activity == nil || staging.Activity.State != platformstate.Unreleased {
		t.Errorf("staging = %+v, %+v; want Healthy and Unreleased", staging.Condition, staging.Activity)
	}
}

// Read lists the pods by the application label, not the Deployment's
// selector, so it sees a Job's pods, even before there is a Deployment:
// hello-pr-2 in #132, waiting on a migration whose pod cannot be
// scheduled.
func TestReadSeesTheJobsPods(t *testing.T) {
	var podSelector string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/apis/argoproj.io/v1alpha1/namespaces/argocd/applications":
			_, _ = w.Write([]byte(`{"items":[
				{"metadata":{"name":"shop-pr-2","creationTimestamp":"2020-01-01T00:00:00Z","labels":{"iidp.itema.no/application":"shop"}},
				 "spec":{"destination":{"namespace":"shop-pr-2"},"sources":[{"helm":{"valuesObject":{"image":{"tag":"sha-new"}}}}]},
				 "status":{"sync":{"status":"OutOfSync"},"health":{"status":"Missing"},
				  "operationState":{"phase":"Running","startedAt":"2020-01-01T00:01:00Z"}}}]}`))
		case "/apis/batch/v1/namespaces/shop-pr-2/jobs":
			_, _ = w.Write([]byte(`{"items":[{"metadata":{"name":"shop-migrate","creationTimestamp":"2020-01-01T00:02:00Z",
				"labels":{"iidp.itema.no/application":"shop","app.kubernetes.io/component":"migration"}},
				"spec":{"template":{"spec":{"containers":[{"name":"migrate","image":"ghcr.io/itema-as/shop:sha-new"}]}}},
				"status":{"startTime":"2020-01-01T00:02:00Z"}}]}`))
		case "/api/v1/namespaces/shop-pr-2/pods":
			podSelector = r.URL.Query().Get("labelSelector")
			_, _ = w.Write([]byte(`{"items":[{"metadata":{"name":"shop-migrate-x7k2p","labels":{"iidp.itema.no/application":"shop",
				"app.kubernetes.io/component":"migration","batch.kubernetes.io/job-name":"shop-migrate","job-name":"shop-migrate"}},
				"status":{"phase":"Pending","conditions":[{"type":"PodScheduled","status":"False","reason":"Unschedulable",
				"message":"0/1 nodes are available: 1 Insufficient cpu."}]}}]}`))
		default:
			_, _ = w.Write([]byte(`{"items":[]}`))
		}
	}))
	defer api.Close()

	envs, err := platformstate.Read(context.Background(), &platformstate.Kube{BaseURL: api.URL}, "shop")
	if err != nil {
		t.Fatal(err)
	}
	if podSelector != "iidp.itema.no/application=shop" {
		t.Errorf("pods listed by %q, want the application label", podSelector)
	}
	pr := envs[0]
	if a := pr.Activity; a == nil || a.State != platformstate.Arriving || !a.Stuck || a.Reason != "the migration's Pod shop-migrate-x7k2p is Unschedulable: 0/1 nodes are available: 1 Insufficient cpu." {
		t.Errorf("Activity = %+v, want Arriving, stuck on the migration's pod", a)
	}
	if pr.Migration == nil || pr.Migration.Result != platformstate.RunPending {
		t.Errorf("Migration = %+v, want pending", pr.Migration)
	}
}

func TestEnvironmentsAreSortedProdStagingThenPreviewsByNumber(t *testing.T) {
	envs := []platformstate.Environment{{Name: "pr-10"}, {Name: "staging"}, {Name: "pr-9"}, {Name: "prod"}, {Name: "pr-9a"}}
	platformstate.SortEnvironments(envs)
	var names []string
	for _, e := range envs {
		names = append(names, e.Name)
	}
	if got := strings.Join(names, " "); got != "prod staging pr-9 pr-10 pr-9a" {
		t.Errorf("order = %s", got)
	}
}
