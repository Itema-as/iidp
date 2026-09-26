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
