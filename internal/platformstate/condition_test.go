package platformstate_test

import (
	"strings"
	"testing"
	"time"

	"github.com/Itema-as/iidp/internal/platformstate"
)

// want is an Environment's expected Condition and Activity. An empty
// activity is none; a reason is a substring the actual one must contain.
type want struct {
	condition, conditionReason string
	activity                   string
	stuck                      bool
	reason                     string
	hop                        string
}

func (w want) check(t *testing.T, got platformstate.EnvironmentState) {
	t.Helper()
	if got.Condition == nil || got.Condition.State != w.condition || !strings.Contains(got.Condition.Reason, w.conditionReason) {
		t.Errorf("Condition = %+v, want %s (%q)", got.Condition, w.condition, w.conditionReason)
	}
	if got.Condition != nil && got.Condition.Warning != "" {
		t.Errorf("an Environment has the Warning flag %q; only Capabilities have it", got.Condition.Warning)
	}
	a := got.Activity
	switch {
	case w.activity == "" && a != nil:
		t.Errorf("Activity = %+v (deploy %+v), want none", a, a.Deploy)
	case w.activity == "":
	case a == nil:
		t.Errorf("Activity = none, want %s", w.activity)
	case a.State != w.activity || a.Stuck != w.stuck || !strings.Contains(a.Reason, w.reason) || (w.stuck && a.Reason == ""):
		t.Errorf("Activity = %s stuck=%v %q, want %s stuck=%v %q", a.State, a.Stuck, a.Reason, w.activity, w.stuck, w.reason)
	}
	if w.hop != "" {
		if a == nil || a.Deploy == nil {
			t.Errorf("no Deploy under way, want one at %s", w.hop)
		} else if a.Deploy.Hop != w.hop || a.Deploy.Stuck != w.stuck {
			t.Errorf("Deploy = %+v, want at %s, stuck=%v", a.Deploy, w.hop, w.stuck)
		}
	} else if a != nil && a.Deploy != nil {
		t.Errorf("Deploy = %+v, want none", a.Deploy)
	}
}

// Every mapping from ArgoCD's state, each as the objects that make it.
func TestArgoCDMappingTable(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(f *fixture)
		want  want
	}{
		{"Healthy + Synced, no operation: Healthy, no Activity", func(f *fixture) {
			delete(status(f.app), "operationState")
		}, want{condition: "Healthy"}},

		{"Healthy + Synced, no image yet: Arriving", func(f *fixture) {
			f.app["metadata"].(obj)["creationTimestamp"] = ago(10 * time.Minute)
			f.deployments, f.pods = nil, nil
		}, want{condition: "Healthy", activity: "Arriving"}},
		{"Healthy + Synced, no image after 30 minutes: Unreleased", func(f *fixture) {
			f.app["metadata"].(obj)["creationTimestamp"] = ago(31 * time.Minute)
			f.deployments, f.pods = nil, nil
		}, want{condition: "Healthy", activity: "Unreleased"}},

		{"OutOfSync: Updating", func(f *fixture) {
			looked(f.app, c2, "OutOfSync", 10*time.Second)
			syncing(f.app, c1, "Succeeded", "", time.Minute)
		}, want{condition: "Healthy", activity: "Updating"}},
		{"operation Running: Updating", func(f *fixture) {
			syncing(f.app, c2, "Running", "", 10*time.Second)
		}, want{condition: "Healthy", activity: "Updating"}},
		{"OutOfSync on a Deploy's commit: Deploying", func(f *fixture) {
			f.events = []obj{accepted("2.0.0", c2, 30*time.Second)}
			looked(f.app, c2, "OutOfSync", 10*time.Second)
		}, want{condition: "Healthy", activity: "Deploying", hop: platformstate.HopWaitingForArgoCD}},

		{"health Progressing with the pods serving: Healthy", func(f *fixture) {
			status(f.app)["health"] = obj{"status": "Progressing"}
			f.deployments = []obj{rollingDeployment("2.0.0")}
			f.pods = []obj{readyPod("shop-a", "1.0.0"), notReadyPod("shop-b", "2.0.0", 30*time.Second)}
		}, want{condition: "Healthy", activity: "Deploying", hop: platformstate.HopRollingOut}},
		{"health Progressing with no pod ready for over 60 s: Degraded", func(f *fixture) {
			status(f.app)["health"] = obj{"status": "Progressing"}
			f.pods = []obj{notReadyPod("shop-a", "1.0.0", 90*time.Second)}
		}, want{condition: "Degraded", conditionReason: "0 of 1 pods ready for over a minute"}},
		{"health Progressing with no pod ready for 30 s: still Healthy", func(f *fixture) {
			status(f.app)["health"] = obj{"status": "Progressing"}
			f.pods = []obj{notReadyPod("shop-a", "1.0.0", 30*time.Second)}
		}, want{condition: "Healthy"}},

		{"Degraded from ProgressDeadlineExceeded, the old pod serving: Healthy + stuck", func(f *fixture) {
			status(f.app)["health"] = obj{"status": "Degraded"}
			f.deployments = []obj{deadlineDeployment("2.0.0")}
			f.pods = []obj{readyPod("shop-a", "1.0.0"), notReadyPod("shop-b", "2.0.0", 11*time.Minute)}
		}, want{condition: "Healthy", activity: "Deploying", stuck: true, reason: "progress deadline", hop: platformstate.HopRollingOut}},
		{"Degraded from crash-looping pods that are serving: Degraded", func(f *fixture) {
			status(f.app)["health"] = obj{"status": "Degraded"}
			f.pods = []obj{waitingPod("shop-a", "1.0.0", "CrashLoopBackOff")}
		}, want{condition: "Degraded", conditionReason: "CrashLoopBackOff"}},

		{"operation Failed: stuck", func(f *fixture) {
			looked(f.app, c2, "OutOfSync", 10*time.Second)
			syncing(f.app, c2, "Failed", "one or more objects failed to apply", 20*time.Second)
		}, want{condition: "Healthy", activity: "Updating", stuck: true, reason: "ArgoCD's sync failed: one or more objects failed to apply"}},
		{"operation Error: stuck", func(f *fixture) {
			looked(f.app, c2, "OutOfSync", 10*time.Second)
			syncing(f.app, c2, "Error", "rpc error", 20*time.Second)
		}, want{condition: "Healthy", activity: "Updating", stuck: true, reason: "ArgoCD's sync error: rpc error"}},

		{"Missing: Arriving", func(f *fixture) {
			f.app["metadata"].(obj)["creationTimestamp"] = ago(time.Minute)
			status(f.app)["health"] = obj{"status": "Missing"}
			looked(f.app, c1, "OutOfSync", 10*time.Second)
			syncing(f.app, c1, "Running", "", 20*time.Second)
			f.deployments, f.pods = nil, nil
		}, want{condition: "Healthy", activity: "Arriving"}},
		{"Missing because the chart won't render: Arriving, stuck", func(f *fixture) {
			f.app["metadata"].(obj)["creationTimestamp"] = ago(time.Minute)
			status(f.app)["health"] = obj{"status": "Missing"}
			delete(status(f.app), "operationState")
			argoCondition(f.app, "ComparisonError", "failed to render: values don't meet the schema")
			f.deployments, f.pods = nil, nil
		}, want{condition: "Healthy", activity: "Arriving", stuck: true, reason: "ArgoCD cannot render the Environment: failed to render"}},

		{"deletionTimestamp set: Leaving", func(f *fixture) {
			f.app["metadata"].(obj)["deletionTimestamp"] = ago(20 * time.Second)
		}, want{condition: "Healthy", activity: "Leaving"}},

		{"Suspended: not used, the pods decide", func(f *fixture) {
			status(f.app)["health"] = obj{"status": "Suspended"}
		}, want{condition: "Healthy"}},

		{"Unknown: Unknown", func(f *fixture) {
			status(f.app)["health"] = obj{"status": "Unknown"}
		}, want{condition: "Unknown", conditionReason: "ArgoCD"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := env()
			tc.setup(f)
			tc.want.check(t, f.state(t))
		})
	}
}

// Every rule for Degraded and Unknown, and the grace before Degraded.
func TestConditionRules(t *testing.T) {
	for _, tc := range []struct {
		name string
		pods []obj
		want want
	}{
		{"all pods ready", []obj{readyPod("shop-a", "1.0.0")}, want{condition: "Healthy"}},
		{"not ready for 59 s", []obj{notReadyPod("shop-a", "1.0.0", 59*time.Second)}, want{condition: "Healthy"}},
		{"not ready for 61 s", []obj{notReadyPod("shop-a", "1.0.0", 61*time.Second)}, want{condition: "Degraded", conditionReason: "0 of 1 pods ready"}},
		{"CrashLoopBackOff, at once", []obj{waitingPod("shop-a", "1.0.0", "CrashLoopBackOff")}, want{condition: "Degraded", conditionReason: "shop-a is in CrashLoopBackOff"}},
		{"OOMKilled, at once", []obj{oomKilledPod("shop-a", "1.0.0")}, want{condition: "Degraded", conditionReason: "shop-a was OOMKilled"}},
		{"Unschedulable, at once", []obj{unschedulablePod("shop-a", "1.0.0")}, want{condition: "Degraded", conditionReason: "Unschedulable: 0/1 nodes are available"}},
		{"a pod whose status is Unknown", []obj{unknownPod("shop-a", "1.0.0")}, want{condition: "Unknown", conditionReason: "shop-a"}},
		{"no pods at all, short since the Deployment said so an hour ago", nil, want{condition: "Degraded", conditionReason: "0 of 1"}},
		{"recovered after restarts: Healthy again at once", []obj{func() obj {
			p := readyPod("shop-a", "1.0.0")
			cs := status(p)["containerStatuses"].([]any)[0].(obj)
			cs["restartCount"] = 4
			cs["lastState"] = obj{"terminated": obj{"reason": "OOMKilled", "exitCode": 137}}
			return p
		}()}, want{condition: "Healthy"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := env()
			f.pods = tc.pods
			if tc.pods == nil {
				d := servedDeployment("1.0.0")
				d["status"].(obj)["conditions"].([]any)[0] = cond("Available", "False", "MinimumReplicasUnavailable", "", time.Hour)
				f.deployments = []obj{d}
			}
			tc.want.check(t, f.state(t))
		})
	}
}

// Nothing has served yet while an Environment arrives, so a slow first
// start is not Degraded; a crash still is.
func TestArrivingIsNotDegradedByTheGrace(t *testing.T) {
	f := env()
	f.deployments = []obj{firstDeployment("1.0.0", 5*time.Minute)}
	f.pods = []obj{notReadyPod("shop-a", "1.0.0", 5*time.Minute)}
	want{condition: "Healthy", activity: "Arriving", hop: platformstate.HopRollingOut}.check(t, f.state(t))

	f.pods = []obj{waitingPod("shop-a", "1.0.0", "CrashLoopBackOff")}
	want{condition: "Degraded", conditionReason: "CrashLoopBackOff", activity: "Arriving", hop: platformstate.HopRollingOut}.check(t, f.state(t))
}

// Every stuck rule: at once, or after its timeout on the injected clock.
func TestStuckRules(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(f *fixture)
		want  want
	}{
		{"a failed migration Job, at once", func(f *fixture) {
			looked(f.app, c2, "OutOfSync", 10*time.Second)
			syncing(f.app, c2, "Failed", "one or more synchronization tasks completed unsuccessfully", 10*time.Second)
			f.jobs = []obj{migration("2.0.0", 30*time.Second, "Failed")}
		}, want{condition: "Healthy", activity: "Deploying", stuck: true, reason: "the migration failed", hop: platformstate.HopApplying}},
		{"a failed migration Job of a change that is not a Deploy", func(f *fixture) {
			looked(f.app, c2, "OutOfSync", 10*time.Second)
			syncing(f.app, c2, "Failed", "", 10*time.Second)
			f.jobs = []obj{migration("1.0.0", 30*time.Second, "Failed")}
		}, want{condition: "Healthy", activity: "Updating", stuck: true, reason: "ArgoCD's sync failed"}},
		{"a Failed sync, at once", func(f *fixture) {
			syncing(f.app, c2, "Failed", "boom", time.Second)
		}, want{condition: "Healthy", activity: "Updating", stuck: true, reason: "failed: boom"}},
		{"a failing image pull, at once", func(f *fixture) {
			f.deployments = []obj{rollingDeployment("2.0.0")}
			f.pods = []obj{readyPod("shop-a", "1.0.0"), waitingPod("shop-b", "2.0.0", "ImagePullBackOff")}
		}, want{condition: "Healthy", activity: "Deploying", stuck: true, reason: "the image ghcr.io/itema-as/shop:2.0.0 cannot be pulled (ImagePullBackOff)", hop: platformstate.HopRollingOut}},
		{"ErrImagePull, at once", func(f *fixture) {
			f.deployments = []obj{rollingDeployment("2.0.0")}
			f.pods = []obj{readyPod("shop-a", "1.0.0"), waitingPod("shop-b", "2.0.0", "ErrImagePull")}
		}, want{condition: "Healthy", activity: "Deploying", stuck: true, reason: "ErrImagePull", hop: platformstate.HopRollingOut}},
		{"a ComparisonError, at once", func(f *fixture) {
			looked(f.app, c2, "Unknown", 10*time.Second)
			argoCondition(f.app, "ComparisonError", "Manifest generation error")
		}, want{condition: "Healthy", activity: "Updating", stuck: true, reason: "Manifest generation error"}},
		{"a DeletionError, at once", func(f *fixture) {
			f.app["metadata"].(obj)["deletionTimestamp"] = ago(10 * time.Minute)
			argoCondition(f.app, "DeletionError", "PreDelete hook failed")
		}, want{condition: "Healthy", activity: "Leaving", stuck: true, reason: "the deletion is blocked: PreDelete hook failed"}},
		{"a failed final backup, at once", func(f *fixture) {
			f.app["metadata"].(obj)["deletionTimestamp"] = ago(10 * time.Minute)
			f.jobs = []obj{job("final-backup", "1.0.0", 5*time.Minute, "Failed")}
		}, want{condition: "Healthy", activity: "Leaving", stuck: true, reason: "the final backup failed"}},
		{"a rollout that is not a Deploy past ProgressDeadlineExceeded", func(f *fixture) {
			f.deployments = []obj{deadlineDeployment("1.0.0")}
			f.pods = []obj{readyPod("shop-a", "1.0.0"), notReadyPod("shop-b", "1.0.0", 11*time.Minute)}
		}, want{condition: "Healthy", activity: "Updating", stuck: true, reason: "progress deadline"}},
		{"a rollout within its progress deadline", func(f *fixture) {
			f.deployments = []obj{rollingDeployment("1.0.0")}
			f.pods = []obj{readyPod("shop-a", "1.0.0"), notReadyPod("shop-b", "1.0.0", 2*time.Minute)}
		}, want{condition: "Healthy", activity: "Updating"}},
		{"OutOfSync with no operation for 4 minutes", func(f *fixture) {
			looked(f.app, c2, "OutOfSync", 10*time.Second)
			syncing(f.app, c1, "Succeeded", "", 4*time.Minute)
		}, want{condition: "Healthy", activity: "Updating"}},
		{"OutOfSync with no operation for 6 minutes", func(f *fixture) {
			looked(f.app, c2, "OutOfSync", 10*time.Second)
			syncing(f.app, c1, "Succeeded", "", 6*time.Minute)
		}, want{condition: "Healthy", activity: "Updating", stuck: true, reason: "OutOfSync for over 5 minutes with no sync started"}},
		{"OutOfSync for 6 minutes, as a watch saw it", func(f *fixture) {
			looked(f.app, c2, "OutOfSync", 10*time.Second)
			syncing(f.app, c1, "Succeeded", "", time.Hour)
			since := now.Add(-6 * time.Minute)
			f.outOfSync = &since
		}, want{condition: "Healthy", activity: "Updating", stuck: true, reason: "OutOfSync for over 5 minutes"}},
		{"OutOfSync for 2 minutes, as a watch saw it, after a sync an hour ago", func(f *fixture) {
			looked(f.app, c2, "OutOfSync", 10*time.Second)
			syncing(f.app, c1, "Succeeded", "", time.Hour)
			since := now.Add(-2 * time.Minute)
			f.outOfSync = &since
		}, want{condition: "Healthy", activity: "Updating"}},
		{"OutOfSync for an hour with a sync running", func(f *fixture) {
			looked(f.app, c2, "OutOfSync", 10*time.Second)
			syncing(f.app, c2, "Running", "", 3*time.Second)
			since := now.Add(-time.Hour)
			f.outOfSync = &since
		}, want{condition: "Healthy", activity: "Updating"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := env()
			tc.setup(f)
			tc.want.check(t, f.state(t))
		})
	}
}

// A hook Job whose pod cannot be scheduled holds its change back for
// good: ArgoCD waits for the hook, which has no deadline of its own. So
// it is stuck at once, with the scheduler's words, as an Unschedulable
// pod of the Deployment is Degraded at once.
func TestUnschedulableHookPodIsStuck(t *testing.T) {
	const why = "Unschedulable: 0/1 nodes are available: 1 Insufficient cpu."
	for _, tc := range []struct {
		name  string
		setup func() *fixture
		want  want
	}{
		{"hello-pr-2 in #132: a preview's first migration, Unschedulable for 20 minutes", func() *fixture {
			f := previewEnv(22 * time.Minute)
			syncing(f.app, c1, "Running", "waiting for completion of hook batch/Job/shop-migration", 21*time.Minute)
			f.jobs = []obj{migration("sha-new", 20*time.Minute, "")}
			f.pods = []obj{unschedulableJobPod("shop-migration-x7k2p", "shop-migration")}
			return f
		}, want{condition: "Healthy", activity: "Arriving", stuck: true, reason: "the migration's Pod shop-migration-x7k2p is " + why, hop: platformstate.HopApplying}},
		{"the same, 30 seconds in: at once", func() *fixture {
			f := previewEnv(time.Minute)
			syncing(f.app, c1, "Running", "", 50*time.Second)
			f.jobs = []obj{migration("sha-new", 40*time.Second, "")}
			f.pods = []obj{unschedulableJobPod("shop-migration-x7k2p", "shop-migration")}
			return f
		}, want{condition: "Healthy", activity: "Arriving", stuck: true, reason: "the migration's Pod shop-migration-x7k2p is " + why, hop: platformstate.HopApplying}},
		{"a Deploy's migration", func() *fixture {
			f := env()
			f.events = []obj{accepted("2.0.0", c2, 2*time.Minute)}
			looked(f.app, c2, "OutOfSync", time.Minute)
			syncing(f.app, c2, "Running", "", time.Minute)
			f.jobs = []obj{migration("2.0.0", 50*time.Second, "")}
			f.pods = []obj{readyPod("shop-a", "1.0.0"), unschedulableJobPod("shop-migration-b", "shop-migration")}
			return f
		}, want{condition: "Healthy", activity: "Deploying", stuck: true, reason: "the migration's Pod shop-migration-b is " + why, hop: platformstate.HopApplying}},
		{"the migration of a change that is not a Deploy", func() *fixture {
			f := env()
			looked(f.app, c2, "OutOfSync", time.Minute)
			syncing(f.app, c2, "Running", "", time.Minute)
			f.jobs = []obj{migration("1.0.0", 50*time.Second, "")}
			f.pods = []obj{readyPod("shop-a", "1.0.0"), unschedulableJobPod("shop-migration-b", "shop-migration")}
			return f
		}, want{condition: "Healthy", activity: "Updating", stuck: true, reason: "the migration's Pod shop-migration-b is " + why}},
		{"a migration's pod still getting its image", func() *fixture {
			f := previewEnv(time.Minute)
			syncing(f.app, c1, "Running", "", 50*time.Second)
			f.jobs = []obj{migration("sha-new", 40*time.Second, "")}
			f.pods = []obj{jobPod("shop-migration-x7k2p", "shop-migration", "Pending")}
			return f
		}, want{condition: "Healthy", activity: "Arriving", hop: platformstate.HopApplying}},
		{"an Unschedulable pod of another Job", func() *fixture {
			f := previewEnv(time.Minute)
			syncing(f.app, c1, "Running", "", 50*time.Second)
			f.jobs = []obj{migration("sha-new", 40*time.Second, "")}
			f.pods = []obj{unschedulableJobPod("shop-migration-old", "shop-migration-old")}
			return f
		}, want{condition: "Healthy", activity: "Arriving", hop: platformstate.HopApplying}},
		{"the final backup's pod", func() *fixture {
			f := env()
			f.app["metadata"].(obj)["deletionTimestamp"] = ago(5 * time.Minute)
			f.jobs = []obj{job("final-backup", "1.0.0", 4*time.Minute, "")}
			f.pods = []obj{readyPod("shop-a", "1.0.0"), unschedulableJobPod("shop-final-backup-c", "shop-final-backup")}
			return f
		}, want{condition: "Healthy", activity: "Leaving", stuck: true, reason: "the final backup's Pod shop-final-backup-c is " + why}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.want.check(t, tc.setup().state(t))
		})
	}
}

// Arriving with something on its way, and Leaving, are stuck after
// their time limits, with what is known of why.
func TestArrivingAndLeavingAreStuckAfterTheirTimeLimits(t *testing.T) {
	// migrating is pr-2 whose first migration was created d ago, with a
	// pod in phase, or none when phase is "".
	migrating := func(d time.Duration, phase string) *fixture {
		f := previewEnv(d + 2*time.Minute)
		syncing(f.app, c1, "Running", "", d+time.Minute)
		f.jobs = []obj{migration("sha-new", d, "")}
		if phase != "" {
			f.pods = []obj{jobPod("shop-migration-x7k2p", "shop-migration", phase)}
		}
		return f
	}
	leaving := func(d time.Duration) *fixture {
		f := env()
		f.app["metadata"].(obj)["deletionTimestamp"] = ago(d)
		return f
	}
	for _, tc := range []struct {
		name  string
		setup func() *fixture
		want  want
	}{
		{"a first migration running for 14 minutes", func() *fixture { return migrating(14*time.Minute, "Running") },
			want{condition: "Healthy", activity: "Arriving", hop: platformstate.HopApplying}},
		{"a first migration running for 16 minutes", func() *fixture { return migrating(16*time.Minute, "Running") },
			want{condition: "Healthy", activity: "Arriving", stuck: true, reason: "no progress for over 15 minutes; the migration has not finished", hop: platformstate.HopApplying}},
		{"a first migration with no pod for 16 minutes", func() *fixture { return migrating(16*time.Minute, "") },
			want{condition: "Healthy", activity: "Arriving", stuck: true, reason: "no progress for over 15 minutes; the migration's Pod has not started", hop: platformstate.HopApplying}},
		{"a preview's first sync running for 16 minutes before any migration", func() *fixture {
			f := previewEnv(17 * time.Minute)
			syncing(f.app, c1, "Running", "", 16*time.Minute)
			return f
		}, want{condition: "Healthy", activity: "Arriving", stuck: true, reason: "no progress for over 15 minutes; ArgoCD's sync is still running", hop: platformstate.HopApplying}},
		{"a first rollout with no pod ready for 14 minutes", func() *fixture {
			f := env()
			f.deployments = []obj{firstDeployment("1.0.0", 14*time.Minute)}
			f.pods = []obj{notReadyPod("shop-a", "1.0.0", 14*time.Minute)}
			return f
		}, want{condition: "Healthy", activity: "Arriving", hop: platformstate.HopRollingOut}},
		{"a first rollout with no pod ready for 16 minutes", func() *fixture {
			f := env()
			f.deployments = []obj{firstDeployment("1.0.0", 16*time.Minute)}
			f.pods = []obj{notReadyPod("shop-a", "1.0.0", 16*time.Minute)}
			return f
		}, want{condition: "Healthy", activity: "Arriving", stuck: true, reason: "no progress for over 15 minutes; 0 of 1 pods ready", hop: platformstate.HopRollingOut}},
		{"a first Deploy into an Unreleased Environment, accepted 14 minutes ago", func() *fixture {
			f := env()
			f.deployments, f.pods = nil, nil
			f.events = []obj{accepted("1.0.0", c2, 14*time.Minute)}
			return f
		}, want{condition: "Healthy", activity: "Arriving", hop: platformstate.HopAccepted}},
		{"a first Deploy into an Unreleased Environment, accepted 16 minutes ago", func() *fixture {
			f := env()
			f.deployments, f.pods = nil, nil
			f.events = []obj{accepted("1.0.0", c2, 16*time.Minute)}
			return f
		}, want{condition: "Healthy", activity: "Arriving", stuck: true, reason: "no progress for over 15 minutes", hop: platformstate.HopAccepted}},
		{"nothing on its way for 20 minutes: not stuck, and Unreleased later", func() *fixture {
			f := env()
			f.app["metadata"].(obj)["creationTimestamp"] = ago(20 * time.Minute)
			f.deployments, f.pods = nil, nil
			return f
		}, want{condition: "Healthy", activity: "Arriving"}},

		{"Leaving for 44 minutes", func() *fixture { return leaving(44 * time.Minute) },
			want{condition: "Healthy", activity: "Leaving"}},
		{"Leaving for 46 minutes", func() *fixture { return leaving(46 * time.Minute) },
			want{condition: "Healthy", activity: "Leaving", stuck: true, reason: "Leaving for over 45 minutes"}},
		{"#131: Leaving for 20 minutes with a sync running: not yet", func() *fixture {
			f := leaving(20 * time.Minute)
			syncing(f.app, c2, "Running", "", 30*time.Minute)
			return f
		}, want{condition: "Healthy", activity: "Leaving"}},
		{"#131: Leaving for 46 minutes with a sync running", func() *fixture {
			f := leaving(46 * time.Minute)
			syncing(f.app, c2, "Running", "", time.Hour)
			return f
		}, want{condition: "Healthy", activity: "Leaving", stuck: true, reason: "Leaving for over 45 minutes; ArgoCD's sync operation is still running"}},
		{"Leaving for 46 minutes with the final backup running", func() *fixture {
			f := leaving(46 * time.Minute)
			f.jobs = []obj{job("final-backup", "1.0.0", 45*time.Minute, "")}
			f.pods = []obj{readyPod("shop-a", "1.0.0"), jobPod("shop-final-backup-c", "shop-final-backup", "Running")}
			return f
		}, want{condition: "Healthy", activity: "Leaving", stuck: true, reason: "Leaving for over 45 minutes; the final backup has not finished"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.setup().state(t)
			tc.want.check(t, got)
			// The Deploy in the Activity and in the list of Deploys agree.
			if a := got.Activity; a != nil && a.Deploy != nil {
				if last := got.Deploys[len(got.Deploys)-1]; last.Stuck != a.Deploy.Stuck || last.Reason != a.Deploy.Reason {
					t.Errorf("Deploys end with %+v, want the Activity's %+v", last, *a.Deploy)
				}
			}
		})
	}
}

// A stuck change is not Degraded, and Degraded is not stuck: the two
// layers are independent.
func TestDegradedAndStuckAreIndependent(t *testing.T) {
	f := env()
	f.deployments = []obj{rollingDeployment("2.0.0")}
	f.pods = []obj{waitingPod("shop-a", "1.0.0", "CrashLoopBackOff"), waitingPod("shop-b", "2.0.0", "ImagePullBackOff")}
	want{condition: "Degraded", conditionReason: "CrashLoopBackOff", activity: "Deploying", stuck: true, reason: "cannot be pulled", hop: platformstate.HopRollingOut}.check(t, f.state(t))
}

func TestWatchLostAfter30Seconds(t *testing.T) {
	if platformstate.WatchLost(nil, now) {
		t.Errorf("a watch that is not broken is lost")
	}
	for d, lost := range map[time.Duration]bool{29 * time.Second: false, 30 * time.Second: true, time.Minute: true} {
		since := now.Add(-d)
		if got := platformstate.WatchLost(&since, now); got != lost {
			t.Errorf("broken for %s: lost = %v, want %v", d, got, lost)
		}
	}
}
