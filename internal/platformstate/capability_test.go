package platformstate_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Itema-as/iidp/internal/platformstate"
)

func capability(t *testing.T, got platformstate.EnvironmentState, typ string) platformstate.Capability {
	t.Helper()
	for _, c := range got.Capabilities {
		if c.Type == typ {
			return c
		}
	}
	t.Fatalf("no %s Capability in %+v", typ, got.Capabilities)
	return platformstate.Capability{}
}

// A failing backup puts up the Warning flag on the Postgres Capability. It
// is not Degraded, and the Environment, which judges only its own
// workload, stays Healthy.
func TestFailingBackupIsAWarningNotDegraded(t *testing.T) {
	f := env()
	f.clusters = []obj{pgCluster("Cluster in healthy state",
		cond("Ready", "True", "ClusterIsReady", "Cluster is Ready", time.Hour),
		cond("ContinuousArchiving", "True", "ContinuousArchivingSuccess", "", time.Hour),
		cond("LastBackupSucceeded", "False", "LastBackupFailed", "exit status 2", 10*time.Minute),
	)}
	got := f.state(t)
	want{condition: "Healthy"}.check(t, got)
	pg := capability(t, got, platformstate.CapabilityPostgres)
	if pg.Name != "shop-db" || pg.Condition.State != platformstate.Healthy || pg.Condition.Warning != "backups are failing: exit status 2" || pg.Activity != nil {
		t.Errorf("Postgres = %+v, want Healthy with the backup Warning", pg)
	}

	// Failing WAL archiving is a Warning too.
	f.clusters = []obj{pgCluster("Cluster in healthy state",
		cond("Ready", "True", "ClusterIsReady", "", time.Hour),
		cond("ContinuousArchiving", "False", "ContinuousArchivingFailing", "unexpected failure invoking barman-cloud-wal-archive", time.Minute),
	)}
	pg = capability(t, f.state(t), platformstate.CapabilityPostgres)
	if pg.Condition.State != platformstate.Healthy || !strings.Contains(pg.Condition.Warning, "WAL archiving is failing") {
		t.Errorf("Postgres = %+v, want Healthy with the archiving Warning", pg)
	}
}

func TestPostgresCondition(t *testing.T) {
	for _, tc := range []struct {
		name     string
		cluster  obj
		want     string
		activity string
	}{
		{"ready", pgCluster("Cluster in healthy state", cond("Ready", "True", "ClusterIsReady", "", time.Hour)), platformstate.Healthy, ""},
		{"not ready for 30 s", pgCluster("Failing over", cond("Ready", "False", "ClusterIsNotReady", "Cluster Is Not Ready", 30*time.Second)), platformstate.Healthy, ""},
		{"not ready for 2 minutes", pgCluster("Failing over", cond("Ready", "False", "ClusterIsNotReady", "Cluster Is Not Ready", 2*time.Minute)), platformstate.Degraded, ""},
		{"Unknown", pgCluster("", cond("Ready", "Unknown", "", "", time.Minute)), platformstate.Unknown, ""},
		{"being created", pgCluster("Setting up primary", cond("Ready", "False", "ClusterIsNotReady", "", 5*time.Minute)), platformstate.Healthy, platformstate.Arriving},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := env()
			f.clusters = []obj{tc.cluster}
			got := f.state(t)
			pg := capability(t, got, platformstate.CapabilityPostgres)
			if pg.Condition.State != tc.want || (pg.Activity == nil) != (tc.activity == "") || (pg.Activity != nil && pg.Activity.State != tc.activity) {
				t.Errorf("Postgres = %+v (activity %+v), want %s %s", pg, pg.Activity, tc.want, tc.activity)
			}
			// The Capability never rolls up into its Environment.
			want{condition: "Healthy"}.check(t, got)
		})
	}
}

func TestCustomDomainCertificate(t *testing.T) {
	in := func(d time.Duration) *time.Time { at := now.Add(d); return &at }
	ready := cond("Ready", "True", "Ready", "Certificate is up to date and has not expired", time.Hour)
	for _, tc := range []struct {
		name            string
		cert            obj
		state, warning  string
		activity        string
		stuck           bool
		activityReasonS string
	}{
		{"valid for 60 days", certificate("www.shop.example", in(60*24*time.Hour), ready), platformstate.Healthy, "", "", false, ""},
		{"expiring within 14 days", certificate("www.shop.example", in(10*24*time.Hour), ready), platformstate.Healthy, "the certificate expires 2026-10-07 12:00 UTC", "", false, ""},
		{"expired", certificate("www.shop.example", in(-time.Hour), cond("Ready", "False", "Expired", "Certificate expired", time.Hour)), platformstate.Degraded, "the certificate expires", "", false, ""},
		{"never issued yet", certificate("www.shop.example", nil, cond("Ready", "False", "DoesNotExist", "Issuing certificate as Secret does not exist", 5*time.Minute)), platformstate.Healthy, "", platformstate.Arriving, false, ""},
		{"never issued, and issuing failed", func() obj {
			c := certificate("www.shop.example", nil, cond("Ready", "False", "Failed", "the ACME order failed: DNS problem", 5*time.Minute))
			c["status"].(obj)["lastFailureTime"] = ago(5 * time.Minute)
			return c
		}(), platformstate.Healthy, "", platformstate.Arriving, true, "DNS problem"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := env()
			f.certs = []obj{tc.cert}
			got := f.state(t)
			d := capability(t, got, platformstate.CapabilityCustomDomain)
			if d.Name != "www.shop.example" || d.Condition.State != tc.state || !strings.HasPrefix(d.Condition.Warning, tc.warning) || (tc.warning == "") != (d.Condition.Warning == "") {
				t.Errorf("domain = %+v, want %s with Warning %q", d, tc.state, tc.warning)
			}
			switch {
			case tc.activity == "" && d.Activity != nil:
				t.Errorf("Activity = %+v, want none", d.Activity)
			case tc.activity != "" && (d.Activity == nil || d.Activity.State != tc.activity || d.Activity.Stuck != tc.stuck || !strings.Contains(d.Activity.Reason, tc.activityReasonS)):
				t.Errorf("Activity = %+v, want %s stuck=%v", d.Activity, tc.activity, tc.stuck)
			}
			want{condition: "Healthy"}.check(t, got)
		})
	}
}

func TestCapabilitiesInOrder(t *testing.T) {
	f := env()
	f.clusters = []obj{pgCluster("Cluster in healthy state", cond("Ready", "True", "", "", time.Hour))}
	f.certs = []obj{certificate("www.shop.example", nil)}
	task := func(name string) obj {
		return obj{"metadata": obj{"name": "shop-" + name, "creationTimestamp": ago(time.Hour),
			"labels": obj{"app.kubernetes.io/component": "scheduled-task", "iidp.itema.no/task": name}},
			"spec": obj{"schedule": "0 3 * * *"}}
	}
	f.cronJobs = []obj{task("report"), task("cleanup")}
	f.cronJobs[1]["metadata"].(obj)["deletionTimestamp"] = ago(time.Second)
	var got []string
	for _, c := range f.state(t).Capabilities {
		s := c.Type + ":" + c.Name + ":" + c.Condition.State
		if c.Activity != nil {
			s += ":" + c.Activity.State
		}
		got = append(got, s)
	}
	if strings.Join(got, " ") != "postgres:shop-db:Healthy custom-domain:www.shop.example:Healthy:Arriving scheduled-task:cleanup:Healthy:Leaving scheduled-task:report:Healthy" {
		t.Errorf("Capabilities = %v", got)
	}
}

// Platform components: a Condition, plus Updating for one ArgoCD manages.
// Traefik and k3s's own parts have a Condition only.
func TestPlatformComponents(t *testing.T) {
	var app platformstate.ArgoCDApplication
	decodeInto(t, argoApp("argocd", "Synced", "Healthy"), &app)
	var rolling, served platformstate.Deployment
	decodeInto(t, rollingDeployment("v3.5.3"), &rolling)
	decodeInto(t, servedDeployment("v3.5.3"), &served)
	var crashing, ready, statefulNotReady platformstate.Pod
	decodeInto(t, waitingPod("shop-a", "v3.5.3", "CrashLoopBackOff"), &crashing)
	decodeInto(t, readyPod("shop-a", "v3.5.3"), &ready)
	decodeInto(t, notReadyPod("argocd-application-controller-0", "v3.5.3", 2*time.Minute), &statefulNotReady)
	statefulNotReady.Metadata.Labels = map[string]string{"app.kubernetes.io/name": "argocd-application-controller"}

	var outOfSync, failed platformstate.ArgoCDApplication
	decodeInto(t, argoApp("argocd", "OutOfSync", "Healthy"), &outOfSync)
	aMinuteAgo := now.Add(-time.Minute)
	failedObj := argoApp("argocd", "OutOfSync", "Healthy")
	syncing(failedObj, c2, "Failed", "ComparisonError", time.Minute)
	decodeInto(t, failedObj, &failed)

	for _, tc := range []struct {
		name      string
		o         platformstate.ComponentObjects
		condition string
		activity  string
		stuck     bool
	}{
		{"managed by ArgoCD, all well", platformstate.ComponentObjects{Name: "argocd", ArgoCD: &app, Deployments: []platformstate.Deployment{served}, Pods: []platformstate.Pod{ready}}, platformstate.Healthy, "", false},
		{"managed by ArgoCD, OutOfSync for a minute: Updating", platformstate.ComponentObjects{Name: "argocd", ArgoCD: &outOfSync, Deployments: []platformstate.Deployment{served}, Pods: []platformstate.Pod{ready}, OutOfSyncSince: &aMinuteAgo}, platformstate.Healthy, platformstate.Updating, false},
		{"managed by ArgoCD, OutOfSync with no sync since an hour: Updating, stuck", platformstate.ComponentObjects{Name: "argocd", ArgoCD: &outOfSync, Deployments: []platformstate.Deployment{served}, Pods: []platformstate.Pod{ready}}, platformstate.Healthy, platformstate.Updating, true},
		{"managed by ArgoCD, rolling out: Updating", platformstate.ComponentObjects{Name: "argocd", ArgoCD: &app, Deployments: []platformstate.Deployment{rolling}, Pods: []platformstate.Pod{ready}}, platformstate.Healthy, platformstate.Updating, false},
		{"managed by ArgoCD, a failed sync: Updating, stuck", platformstate.ComponentObjects{Name: "argocd", ArgoCD: &failed, Deployments: []platformstate.Deployment{served}, Pods: []platformstate.Pod{ready}}, platformstate.Healthy, platformstate.Updating, true},
		{"managed by ArgoCD, crash-looping: Degraded", platformstate.ComponentObjects{Name: "argocd", ArgoCD: &app, Deployments: []platformstate.Deployment{served}, Pods: []platformstate.Pod{crashing}}, platformstate.Degraded, "", false},
		{"a StatefulSet's pod not ready for 2 minutes: Degraded", platformstate.ComponentObjects{Name: "argocd", ArgoCD: &app, Deployments: []platformstate.Deployment{served}, Pods: []platformstate.Pod{ready, statefulNotReady}}, platformstate.Degraded, "", false},
		{"Traefik, rolling out: a Condition only", platformstate.ComponentObjects{Name: "traefik", Deployments: []platformstate.Deployment{rolling}, Pods: []platformstate.Pod{ready}}, platformstate.Healthy, "", false},
		{"Traefik, crash-looping: Degraded", platformstate.ComponentObjects{Name: "traefik", Deployments: []platformstate.Deployment{served}, Pods: []platformstate.Pod{crashing}}, platformstate.Degraded, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := platformstate.ComponentOf(tc.o, now)
			if got.Name != tc.o.Name || got.Condition.State != tc.condition {
				t.Errorf("Condition = %+v, want %s", got.Condition, tc.condition)
			}
			switch {
			case tc.activity == "" && got.Activity != nil:
				t.Errorf("Activity = %+v, want none", got.Activity)
			case tc.activity != "" && (got.Activity == nil || got.Activity.State != tc.activity || got.Activity.Stuck != tc.stuck):
				t.Errorf("Activity = %+v, want %s stuck=%v", got.Activity, tc.activity, tc.stuck)
			}
		})
	}
}

// The new cut-down structs decode the API's own JSON, as a list response
// or an unstructured object's content would give it.
func TestObjectsDecodeTheAPIsJSON(t *testing.T) {
	var events []platformstate.Event
	if err := json.Unmarshal([]byte(`[{"apiVersion":"events.k8s.io/v1","kind":"Event",
		"metadata":{"name":"shop-prod.18a","namespace":"argocd","creationTimestamp":"2026-09-27T11:59:00Z",
			"annotations":{"iidp.itema.no/commit":"`+c2+`","iidp.itema.no/kind":"promote"},"managedFields":[{"manager":"x"}]},
		"eventTime":"2026-09-27T11:59:00.123456Z","reason":"DeployAccepted","note":"Promote shop prod 2.0.0 accepted","type":"Normal",
		"regarding":{"apiVersion":"argoproj.io/v1alpha1","kind":"Application","namespace":"argocd","name":"shop-prod"}},
		{"metadata":{"name":"old","creationTimestamp":"2026-09-27T11:00:00Z"},"eventTime":null,"deprecatedLastTimestamp":"2026-09-27T11:30:00Z","reason":"BackOff"}]`), &events); err != nil {
		t.Fatal(err)
	}
	if e := events[0]; e.Reason != "DeployAccepted" || e.Regarding.Name != "shop-prod" || e.Metadata.Annotations["iidp.itema.no/commit"] != c2 ||
		!e.Time().Equal(time.Date(2026, 9, 27, 11, 59, 0, 123456000, time.UTC)) {
		t.Errorf("event = %+v at %s", e, e.Time())
	}
	if at := events[1].Time(); !at.Equal(time.Date(2026, 9, 27, 11, 30, 0, 0, time.UTC)) {
		t.Errorf("an Event without eventTime is at %s, want its deprecatedLastTimestamp", at)
	}

	var app platformstate.ArgoCDApplication
	if err := json.Unmarshal([]byte(`{"metadata":{"name":"shop-prod","deletionTimestamp":"2026-09-27T11:00:00Z"},
		"status":{"sync":{"status":"OutOfSync","revisions":["0.4.0","`+c2+`"]},"reconciledAt":"2026-09-27T11:58:00Z",
		"operationState":{"phase":"Running","operation":{"sync":{"revisions":["0.4.0","`+c2+`"]}},"syncResult":{"revisions":["0.4.0","`+c1+`"]}},
		"history":[{"id":3,"revisions":["0.4.0","`+c1+`"],"deployedAt":"2026-09-27T10:00:00Z","source":{"repoURL":"x"}}],
		"conditions":[{"type":"ComparisonError","message":"boom","lastTransitionTime":"2026-09-27T11:00:00Z"}]}}`), &app); err != nil {
		t.Fatal(err)
	}
	if app.Metadata.DeletionTimestamp == nil || app.Status.Sync.Revisions[1] != c2 || app.Status.ReconciledAt == nil ||
		app.Status.OperationState.Operation.Sync.Revisions[1] != c2 || app.Status.OperationState.SyncResult.Revisions[1] != c1 ||
		app.Status.History[0].Revisions[1] != c1 || app.Status.History[0].ID != 3 || app.Status.Conditions[0].Type != "ComparisonError" {
		t.Errorf("ArgoCD Application = %+v", app)
	}

	var clusters []platformstate.PostgresCluster
	if err := json.Unmarshal([]byte(`[{"apiVersion":"postgresql.cnpg.io/v1","kind":"Cluster","metadata":{"name":"shop-db"},
		"status":{"phase":"Cluster in healthy state","conditions":[{"type":"LastBackupSucceeded","status":"False","reason":"LastBackupFailed","message":"exit status 2"}]}}]`), &clusters); err != nil {
		t.Fatal(err)
	}
	if c := clusters[0]; c.Status.Phase != "Cluster in healthy state" || c.Status.Conditions[0].Reason != "LastBackupFailed" {
		t.Errorf("cluster = %+v", c)
	}

	var certs []platformstate.Certificate
	if err := json.Unmarshal([]byte(`[{"apiVersion":"cert-manager.io/v1","kind":"Certificate","metadata":{"name":"www"},
		"spec":{"dnsNames":["www.shop.example"]},"status":{"notAfter":"2026-12-01T00:00:00Z","lastFailureTime":"2026-09-01T00:00:00Z",
		"conditions":[{"type":"Ready","status":"True"}]}}]`), &certs); err != nil {
		t.Fatal(err)
	}
	if c := certs[0]; c.Spec.DNSNames[0] != "www.shop.example" || c.Status.NotAfter == nil || c.Status.LastFailureTime == nil || c.Status.Conditions[0].Status != "True" {
		t.Errorf("certificate = %+v", c)
	}
}
