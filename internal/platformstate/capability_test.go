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
	f.ingresses = []obj{ingress("shop", "oauth2-proxy-itema-login-auth@kubernetescrd")}
	var got []string
	for _, c := range f.state(t).Capabilities {
		s := c.Type + ":" + c.Name + ":" + c.Condition.State
		if c.Activity != nil {
			s += ":" + c.Activity.State
		}
		got = append(got, s)
	}
	if strings.Join(got, " ") != "postgres:shop-db:Healthy itema-login:itema-login:Healthy custom-domain:www.shop.example:Healthy:Arriving scheduled-task:cleanup:Healthy:Leaving scheduled-task:report:Healthy" {
		t.Errorf("Capabilities = %v", got)
	}
}

// ingress is one of the chart's Ingresses of shop prod, with Traefik's
// middlewares annotation when middlewares is not empty.
func ingress(name, middlewares string) obj {
	metadata := obj{"name": name, "namespace": "shop-prod", "creationTimestamp": ago(time.Hour),
		"labels": obj{"iidp.itema.no/application": "shop", "iidp.itema.no/environment": "prod"}}
	if middlewares != "" {
		metadata["annotations"] = obj{
			"traefik.ingress.kubernetes.io/router.entrypoints": "websecure",
			"traefik.ingress.kubernetes.io/router.middlewares": middlewares,
		}
	}
	return obj{"apiVersion": "networking.k8s.io/v1", "kind": "Ingress", "metadata": metadata,
		"spec": obj{"rules": []any{obj{"host": "shop.app.itma.no"}}}}
}

// Itema login is modelled from the Environment's Ingresses: the
// application chart's annotation names the shared ForwardAuth middleware,
// or the Environment's own copy with sign-in groups. It has no state
// of its own, and it never touches the Environment's.
func TestItemaLoginFromIngresses(t *testing.T) {
	leavingIngress := ingress("shop", "oauth2-proxy-itema-login-auth@kubernetescrd")
	leavingIngress["metadata"].(obj)["deletionTimestamp"] = ago(time.Second)
	for _, tc := range []struct {
		name      string
		ingresses []obj
		want      string // "" for no Itema login; else its Activity, or "none"
	}{
		{"no Ingress", nil, ""},
		{"an Ingress without middlewares", []obj{ingress("shop", "")}, ""},
		{"another middleware only", []obj{ingress("shop", "kube-system-redirect@kubernetescrd")}, ""},
		{"a middleware merely called itema-login", []obj{ingress("shop", "-itema-login@kubernetescrd")}, ""},
		{"the shared middleware", []obj{ingress("shop", "oauth2-proxy-itema-login-auth@kubernetescrd")}, "none"},
		{"the Environment's own, with sign-in groups", []obj{ingress("shop", "shop-prod-shop-itema-login@kubernetescrd")}, "none"},
		{"among other middlewares", []obj{ingress("shop", "kube-system-headers@kubernetescrd, oauth2-proxy-itema-login-auth@kubernetescrd")}, "none"},
		{"on the custom domains' Ingress too: one Capability", []obj{ingress("shop", "oauth2-proxy-itema-login-auth@kubernetescrd"), ingress("shop-http01", "oauth2-proxy-itema-login-auth@kubernetescrd")}, "none"},
		{"its only Ingress being deleted", []obj{leavingIngress}, platformstate.Leaving},
		{"one of two being deleted", []obj{leavingIngress, ingress("shop-http01", "oauth2-proxy-itema-login-auth@kubernetescrd")}, "none"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := env()
			f.ingresses = tc.ingresses
			got := f.state(t)
			var logins []platformstate.Capability
			for _, c := range got.Capabilities {
				if c.Type == platformstate.CapabilityItemaLogin {
					logins = append(logins, c)
				}
			}
			switch {
			case tc.want == "" && len(logins) != 0:
				t.Fatalf("Capabilities = %+v, want no Itema login", got.Capabilities)
			case tc.want == "":
			case len(logins) != 1:
				t.Fatalf("Capabilities = %+v, want one Itema login", got.Capabilities)
			case logins[0].Name != "itema-login" || logins[0].Condition != (platformstate.Condition{State: platformstate.Healthy}):
				t.Errorf("Itema login = %+v, want Healthy with nothing to warn of", logins[0])
			case tc.want == "none" && logins[0].Activity != nil:
				t.Errorf("Itema login Activity = %+v, want none", logins[0].Activity)
			case tc.want != "none" && (logins[0].Activity == nil || logins[0].Activity.State != tc.want):
				t.Errorf("Itema login Activity = %+v, want %s", logins[0].Activity, tc.want)
			}
			want{condition: "Healthy"}.check(t, got)
		})
	}
}

// A Scheduled task whose last run failed has the Warning flag, like
// failing backups: never Degraded, and the Environment stays as it is.
// The last run is the newest of its Jobs, the one iidp app status shows.
func TestFailedScheduledTaskRunIsAWarning(t *testing.T) {
	cronJob := obj{"metadata": obj{"name": "shop-report", "namespace": "shop-prod", "creationTimestamp": ago(24 * time.Hour),
		"labels": obj{"app.kubernetes.io/component": "scheduled-task", "iidp.itema.no/task": "report", "iidp.itema.no/application": "shop"}},
		"spec": obj{"schedule": "0 3 * * *"}}
	run := func(name string, d time.Duration, condition string) obj {
		j := job("scheduled-task", "1.0.0", d, condition)
		j["metadata"].(obj)["name"] = name
		j["metadata"].(obj)["labels"].(obj)["iidp.itema.no/task"] = "report"
		return j
	}
	for _, tc := range []struct {
		name    string
		jobs    []obj
		warning string
	}{
		{"no run yet", nil, ""},
		{"the last run succeeded", []obj{run("shop-report-1", time.Hour, "Complete")}, ""},
		{"the last run failed", []obj{run("shop-report-1", time.Hour, "Complete"), run("shop-report-2", 10*time.Minute, "Failed")}, "the last run failed at 2026-09-27 11:50 UTC"},
		{"a failed run, then one that succeeded", []obj{run("shop-report-1", time.Hour, "Failed"), run("shop-report-2", 10*time.Minute, "Complete")}, ""},
		{"a failed run, then one still running", []obj{run("shop-report-1", time.Hour, "Failed"), run("shop-report-2", time.Minute, "")}, ""},
		{"another task's failed run", []obj{func() obj {
			j := run("shop-other-1", time.Minute, "Failed")
			j["metadata"].(obj)["labels"].(obj)["iidp.itema.no/task"] = "other"
			return j
		}()}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := env()
			f.cronJobs = []obj{cronJob}
			f.jobs = tc.jobs
			got := f.state(t)
			task := capability(t, got, platformstate.CapabilityScheduledTask)
			if task.Name != "report" || task.Condition.State != platformstate.Healthy || task.Condition.Warning != tc.warning || task.Activity != nil {
				t.Errorf("Scheduled task = %+v, want Healthy with Warning %q", task, tc.warning)
			}
			// The Environment is untouched, and its Tasks agree on the run.
			want{condition: "Healthy"}.check(t, got)
			if len(got.Tasks) != 1 || (tc.warning != "") != (got.Tasks[0].LastRun != nil && got.Tasks[0].LastRun.Result == platformstate.RunFailed) {
				t.Errorf("Tasks = %+v, want the last run failed exactly when the Warning is up", got.Tasks)
			}
		})
	}
}

// A run is pending until a pod of its Job has started, and then running
// until the Job finishes: a migration and a Scheduled task's run alike.
func TestRunIsPendingUntilItsPodStarts(t *testing.T) {
	for _, tc := range []struct {
		name      string
		condition string
		pods      []obj
		want      string
	}{
		{"no pod yet", "", nil, platformstate.RunPending},
		{"its pod Unschedulable", "", []obj{unschedulableJobPod("shop-migration-a", "shop-migration")}, platformstate.RunPending},
		{"its pod getting its image", "", []obj{jobPod("shop-migration-a", "shop-migration", "Pending")}, platformstate.RunPending},
		{"only another Job's pod running", "", []obj{jobPod("shop-migration-old", "shop-migration-old", "Running")}, platformstate.RunPending},
		{"its pod running", "", []obj{jobPod("shop-migration-a", "shop-migration", "Running")}, platformstate.RunRunning},
		{"its pod running, by the older label alone", "", []obj{func() obj {
			p := jobPod("shop-migration-a", "shop-migration", "Running")
			delete(p["metadata"].(obj)["labels"].(obj), "batch.kubernetes.io/job-name")
			return p
		}()}, platformstate.RunRunning},
		{"its pod done, the Job not yet", "", []obj{jobPod("shop-migration-a", "shop-migration", "Succeeded")}, platformstate.RunRunning},
		{"a second pod pending after one that failed", "", []obj{jobPod("shop-migration-a", "shop-migration", "Failed"), jobPod("shop-migration-b", "shop-migration", "Pending")}, platformstate.RunRunning},
		{"Complete, its pod gone", "Complete", nil, platformstate.RunSucceeded},
		{"Failed, its pod gone", "Failed", nil, platformstate.RunFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := env()
			f.cronJobs = []obj{{"metadata": obj{"name": "shop-report", "namespace": "shop-prod", "creationTimestamp": ago(24 * time.Hour),
				"labels": obj{"app.kubernetes.io/component": "scheduled-task", "iidp.itema.no/task": "report"}}}}
			m := migration("1.0.0", time.Minute, tc.condition)
			run := job("scheduled-task", "1.0.0", time.Minute, tc.condition)
			run["metadata"].(obj)["name"] = "shop-migration"
			run["metadata"].(obj)["labels"].(obj)["iidp.itema.no/task"] = "report"
			f.jobs = []obj{m}
			f.pods = append([]obj{readyPod("shop-a", "1.0.0")}, tc.pods...)
			if got := f.state(t).Migration; got == nil || got.Result != tc.want {
				t.Errorf("migration = %+v, want %s", got, tc.want)
			}
			// A task's run, as the Job the same pods belong to, reads the
			// same.
			f.jobs = []obj{run}
			if got := f.state(t).Tasks; len(got) != 1 || got[0].LastRun == nil || got[0].LastRun.Result != tc.want {
				t.Errorf("tasks = %+v, want the last run %s", got, tc.want)
			}
		})
	}
}

// A Scheduled task run whose pod cannot be scheduled is the task's
// Warning, as a failed run is. Tasks do not block serving, so the
// Environment is neither Degraded nor stuck.
func TestUnschedulableScheduledTaskRunIsAWarning(t *testing.T) {
	f := env()
	f.cronJobs = []obj{{"metadata": obj{"name": "shop-heartbeat", "namespace": "shop-prod", "creationTimestamp": ago(24 * time.Hour),
		"labels": obj{"app.kubernetes.io/component": "scheduled-task", "iidp.itema.no/task": "heartbeat", "iidp.itema.no/application": "shop"}},
		"spec": obj{"schedule": "*/10 * * * *"}}}
	run := job("scheduled-task", "1.0.0", 3*time.Minute, "")
	run["metadata"].(obj)["name"] = "shop-heartbeat-29311500"
	run["metadata"].(obj)["labels"].(obj)["iidp.itema.no/task"] = "heartbeat"
	f.jobs = []obj{run}
	f.pods = []obj{readyPod("shop-a", "1.0.0"), unschedulableJobPod("shop-heartbeat-29311500-q8w4d", "shop-heartbeat-29311500")}

	got := f.state(t)
	want{condition: "Healthy"}.check(t, got)
	task := capability(t, got, platformstate.CapabilityScheduledTask)
	if w := "the last run's Pod shop-heartbeat-29311500-q8w4d is Unschedulable: 0/1 nodes are available: 1 Insufficient cpu."; task.Condition.State != platformstate.Healthy || task.Condition.Warning != w || task.Activity != nil {
		t.Errorf("Scheduled task = %+v, want Healthy with the Warning %q", task, w)
	}
	if len(got.Tasks) != 1 || got.Tasks[0].LastRun == nil || got.Tasks[0].LastRun.Result != platformstate.RunPending {
		t.Errorf("Tasks = %+v, want the last run pending", got.Tasks)
	}

	// A run pending for its image is no Warning.
	f.pods = []obj{readyPod("shop-a", "1.0.0"), jobPod("shop-heartbeat-29311500-q8w4d", "shop-heartbeat-29311500", "Pending")}
	if task := capability(t, f.state(t), platformstate.CapabilityScheduledTask); task.Condition.Warning != "" {
		t.Errorf("Scheduled task = %+v, want no Warning", task)
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

	var outOfSync, failed, degraded platformstate.ArgoCDApplication
	decodeInto(t, argoApp("argocd", "OutOfSync", "Healthy"), &outOfSync)
	decodeInto(t, argoApp("cert-manager", "Synced", "Degraded"), &degraded)
	node := func(status string, d time.Duration) platformstate.Node {
		var n platformstate.Node
		decodeInto(t, obj{"metadata": obj{"name": "iidp", "creationTimestamp": ago(30 * 24 * time.Hour)},
			"status": obj{"conditions": []any{cond("MemoryPressure", "False", "KubeletHasSufficientMemory", "", time.Hour),
				cond("Ready", status, "KubeletNotReady", "container runtime is down", d)}}}, &n)
		return n
	}
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
		{"k3s, its node ready", platformstate.ComponentObjects{Name: "k3s", Nodes: []platformstate.Node{node("True", time.Hour)}, Deployments: []platformstate.Deployment{served}, Pods: []platformstate.Pod{ready}}, platformstate.Healthy, "", false},
		{"k3s, its node not ready for 30 s", platformstate.ComponentObjects{Name: "k3s", Nodes: []platformstate.Node{node("False", 30*time.Second)}}, platformstate.Healthy, "", false},
		{"k3s, its node not ready for 2 minutes: Degraded", platformstate.ComponentObjects{Name: "k3s", Nodes: []platformstate.Node{node("False", 2*time.Minute)}}, platformstate.Degraded, "", false},
		{"k3s, its node no longer reporting: Unknown", platformstate.ComponentObjects{Name: "k3s", Nodes: []platformstate.Node{node("Unknown", time.Minute)}}, platformstate.Unknown, "", false},
		{"k3s, a pod crash-looping on a ready node: Degraded", platformstate.ComponentObjects{Name: "k3s", Nodes: []platformstate.Node{node("True", time.Hour)}, Deployments: []platformstate.Deployment{served}, Pods: []platformstate.Pod{crashing}}, platformstate.Degraded, "", false},
		{"managed by ArgoCD, none of its workload in view, ArgoCD says Degraded", platformstate.ComponentObjects{Name: "cert-manager", ArgoCD: &degraded}, platformstate.Degraded, "", false},
		{"managed by ArgoCD, its pods in view and ready, ArgoCD says Degraded: its pods decide", platformstate.ComponentObjects{Name: "cert-manager", ArgoCD: &degraded, Deployments: []platformstate.Deployment{served}, Pods: []platformstate.Pod{ready}}, platformstate.Healthy, "", false},
		{"managed by ArgoCD, none of its workload in view, ArgoCD says Healthy", platformstate.ComponentObjects{Name: "cert-manager", ArgoCD: &app}, platformstate.Healthy, "", false},
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

// A component's version is the tag of the image named after it, else the
// one tag all its images share; k3s's is the node's.
func TestComponentVersion(t *testing.T) {
	withImages := func(images ...string) *platformstate.ArgoCDApplication {
		var app platformstate.ArgoCDApplication
		app.Status.Summary.Images = images
		return &app
	}
	var node platformstate.Node
	node.Status.NodeInfo.KubeletVersion = "v1.36.4+k3s1"
	var traefik platformstate.Deployment
	decodeInto(t, obj{"spec": obj{"template": obj{"spec": obj{"containers": []any{
		obj{"name": "traefik", "image": "rancher/mirrored-library-traefik:3.3.6"}}}}}}, &traefik)

	for _, tc := range []struct {
		name string
		o    platformstate.ComponentObjects
		want string
	}{
		{"ArgoCD's own image among others", platformstate.ComponentObjects{Name: "argocd", ArgoCD: withImages("redis:7.4.2-alpine", "quay.io/argoproj/argocd:v3.1.8", "ghcr.io/dexidp/dex:v2.43.0")}, "v3.1.8"},
		{"the Deploy gate, by its iidp- name, pinned by digest too", platformstate.ComponentObjects{Name: "deploy-gate", ArgoCD: withImages("ghcr.io/itema-as/iidp-deploy-gate:1.4.0@sha256:0123")}, "1.4.0"},
		{"cert-manager's three images", platformstate.ComponentObjects{Name: "cert-manager", ArgoCD: withImages("quay.io/jetstack/cert-manager-cainjector:v1.18.2", "quay.io/jetstack/cert-manager-controller:v1.18.2")}, "v1.18.2"},
		{"none named after it, one tag between them", platformstate.ComponentObjects{Name: "cnpg-barman-cloud", ArgoCD: withImages("ghcr.io/cloudnative-pg/plugin-barman-cloud:v0.6.0", "ghcr.io/cloudnative-pg/plugin-barman-cloud-sidecar:v0.6.0")}, "v0.6.0"},
		{"none named after it, several tags: unknown", platformstate.ComponentObjects{Name: "monitoring", ArgoCD: withImages("grafana/alloy:v1.10.0", "ghcr.io/jimmidyson/configmap-reload:v0.15.0")}, ""},
		{"no images at all", platformstate.ComponentObjects{Name: "guardrails", ArgoCD: withImages()}, ""},
		{"Traefik, from its Deployment", platformstate.ComponentObjects{Name: "traefik", Deployments: []platformstate.Deployment{traefik}}, "3.3.6"},
		{"k3s, from the node", platformstate.ComponentObjects{Name: "k3s", Nodes: []platformstate.Node{node}, Deployments: []platformstate.Deployment{traefik}}, "v1.36.4+k3s1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := platformstate.ComponentOf(tc.o, now).Version; got != tc.want {
				t.Errorf("Version = %q, want %q", got, tc.want)
			}
		})
	}
}

// The cut-down structs decode the API's own JSON, as a list response or
// an unstructured object's content would give it.
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

	var ingresses []platformstate.Ingress
	if err := json.Unmarshal([]byte(`[{"apiVersion":"networking.k8s.io/v1","kind":"Ingress","metadata":{"name":"shop","namespace":"shop-prod",
		"annotations":{"traefik.ingress.kubernetes.io/router.middlewares":"oauth2-proxy-itema-login-auth@kubernetescrd"}},
		"spec":{"rules":[{"host":"shop.app.itma.no"}]}}]`), &ingresses); err != nil {
		t.Fatal(err)
	}
	if a := ingresses[0].Metadata.Annotations[platformstate.MiddlewaresAnnotation]; a != "oauth2-proxy-itema-login-auth@kubernetescrd" {
		t.Errorf("Ingress middlewares = %q", a)
	}

	var nodes []platformstate.Node
	if err := json.Unmarshal([]byte(`[{"apiVersion":"v1","kind":"Node","metadata":{"name":"iidp"},
		"status":{"conditions":[{"type":"Ready","status":"True","reason":"KubeletReady","lastHeartbeatTime":"2026-09-27T11:59:00Z","lastTransitionTime":"2026-09-01T00:00:00Z"}],
		"nodeInfo":{"kubeletVersion":"v1.36.4+k3s1"}}}]`), &nodes); err != nil {
		t.Fatal(err)
	}
	if n := nodes[0]; n.Metadata.Name != "iidp" || n.Status.Conditions[0].Type != "Ready" || n.Status.Conditions[0].Status != "True" {
		t.Errorf("node = %+v", n)
	}
}

// An Environment's database access is read from the annotations the chart
// sets on its Cluster, so Argus shows what is deployed.
func TestDatabaseAccessComesFromTheClustersAnnotations(t *testing.T) {
	f := env()
	cluster := pgCluster("Cluster in healthy state", cond("Ready", "True", "ClusterIsReady", "", time.Hour))
	cluster["metadata"].(obj)["annotations"] = obj{
		"iidp.itema.no/db-access-read-write": "push",
		"iidp.itema.no/db-access-read-only":  "none",
	}
	f.clusters = []obj{cluster}
	got := f.state(t)
	if got.DatabaseAccess == nil || *got.DatabaseAccess != (platformstate.DatabaseAccess{ReadWrite: "push", ReadOnly: "none"}) {
		t.Errorf("DatabaseAccess = %+v, want read-write push and read-only none", got.DatabaseAccess)
	}
	data, err := json.Marshal(got.Environment)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"databaseAccess":{"readWrite":"push","readOnly":"none"}`) {
		t.Errorf("JSON = %s, want databaseAccess", data)
	}

	// A chart from before database access sets no annotations, and an
	// Environment without Postgres has no Cluster: neither has levels.
	f.clusters = []obj{pgCluster("Cluster in healthy state", cond("Ready", "True", "ClusterIsReady", "", time.Hour))}
	if got := f.state(t); got.DatabaseAccess != nil {
		t.Errorf("DatabaseAccess = %+v without annotations, want none", got.DatabaseAccess)
	}
	f.clusters = nil
	if got := f.state(t); got.DatabaseAccess != nil {
		t.Errorf("DatabaseAccess = %+v without a Cluster, want none", got.DatabaseAccess)
	}
}
