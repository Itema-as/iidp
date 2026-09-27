package platformstate_test

import (
	"testing"
	"time"

	"github.com/Itema-as/iidp/internal/platformstate"
)

// A Deploy's five hops (#106), fed with the Deploy gate's Events (built
// here as #117 specifies them), ArgoCD's revisions and operation, the
// migration Job, and the Deployment's pods. prod serves 1.0.0 from c1;
// the Deploy is of 2.0.0, committed as c2.
func TestDeployHops(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(f *fixture)
		want  want
	}{
		{"Accepted: ArgoCD has not looked since", func(f *fixture) {
			f.events = []obj{accepted("2.0.0", c2, 20*time.Second)}
		}, want{condition: "Healthy", activity: "Deploying", hop: platformstate.HopAccepted}},
		{"stuck at Accepted: ArgoCD cannot render", func(f *fixture) {
			f.events = []obj{accepted("2.0.0", c2, 20*time.Second)}
			argoCondition(f.app, "ComparisonError", "rpc error: repository not accessible")
		}, want{condition: "Healthy", activity: "Deploying", stuck: true, reason: "repository not accessible", hop: platformstate.HopAccepted}},

		{"Waiting for ArgoCD: it has looked, and not started", func(f *fixture) {
			f.events = []obj{accepted("2.0.0", c2, 20*time.Second)}
			looked(f.app, c2, "OutOfSync", 5*time.Second)
		}, want{condition: "Healthy", activity: "Deploying", hop: platformstate.HopWaitingForArgoCD}},
		{"Waiting for ArgoCD: it reconciled after the Deploy, before the commit showed", func(f *fixture) {
			f.events = []obj{accepted("2.0.0", c2, 20*time.Second)}
			looked(f.app, c1, "Synced", 5*time.Second)
		}, want{condition: "Healthy", activity: "Deploying", hop: platformstate.HopWaitingForArgoCD}},
		{"stuck at Waiting for ArgoCD: OutOfSync with no sync for over 5 minutes", func(f *fixture) {
			f.events = []obj{accepted("2.0.0", c2, 6*time.Minute)}
			looked(f.app, c2, "OutOfSync", 5*time.Second)
		}, want{condition: "Healthy", activity: "Deploying", stuck: true, reason: "OutOfSync for over 5 minutes", hop: platformstate.HopWaitingForArgoCD}},

		{"Applying: ArgoCD syncs the commit, the migration running", func(f *fixture) {
			f.events = []obj{accepted("2.0.0", c2, time.Minute)}
			looked(f.app, c2, "OutOfSync", 30*time.Second)
			syncing(f.app, c2, "Running", "", 20*time.Second)
			f.jobs = []obj{migration("2.0.0", 15*time.Second, "")}
		}, want{condition: "Healthy", activity: "Deploying", hop: platformstate.HopApplying}},
		{"stuck at Applying: the migration failed", func(f *fixture) {
			f.events = []obj{accepted("2.0.0", c2, time.Minute)}
			looked(f.app, c2, "OutOfSync", 30*time.Second)
			syncing(f.app, c2, "Failed", "one or more synchronization tasks completed unsuccessfully", 10*time.Second)
			f.jobs = []obj{migration("2.0.0", 15*time.Second, "Failed")}
		}, want{condition: "Healthy", activity: "Deploying", stuck: true, reason: "the migration failed", hop: platformstate.HopApplying}},
		{"stuck at Applying: the sync failed", func(f *fixture) {
			f.events = []obj{accepted("2.0.0", c2, time.Minute)}
			looked(f.app, c2, "OutOfSync", 30*time.Second)
			syncing(f.app, c2, "Error", "the server could not find the requested resource", 10*time.Second)
		}, want{condition: "Healthy", activity: "Deploying", stuck: true, reason: "could not find the requested resource", hop: platformstate.HopApplying}},

		{"Rolling out: new pods with the new tag", func(f *fixture) {
			f.events = []obj{accepted("2.0.0", c2, 2*time.Minute)}
			looked(f.app, c2, "Synced", 30*time.Second)
			syncing(f.app, c2, "Succeeded", "", 40*time.Second)
			synced(f.app, c2, 40*time.Second)
			f.deployments = []obj{rollingDeployment("2.0.0")}
			f.pods = []obj{readyPod("shop-a", "1.0.0"), notReadyPod("shop-b", "2.0.0", 20*time.Second)}
		}, want{condition: "Healthy", activity: "Deploying", hop: platformstate.HopRollingOut}},
		{"stuck at Rolling out: the image cannot be pulled", func(f *fixture) {
			f.events = []obj{accepted("2.0.0", c2, 2*time.Minute)}
			synced(f.app, c2, 40*time.Second)
			f.deployments = []obj{rollingDeployment("2.0.0")}
			f.pods = []obj{readyPod("shop-a", "1.0.0"), waitingPod("shop-b", "2.0.0", "ImagePullBackOff")}
		}, want{condition: "Healthy", activity: "Deploying", stuck: true, reason: "cannot be pulled", hop: platformstate.HopRollingOut}},
		{"stuck at Rolling out: past the progress deadline", func(f *fixture) {
			f.events = []obj{accepted("2.0.0", c2, 15*time.Minute)}
			synced(f.app, c2, 14*time.Minute)
			f.deployments = []obj{deadlineDeployment("2.0.0")}
			f.pods = []obj{readyPod("shop-a", "1.0.0"), notReadyPod("shop-b", "2.0.0", 14*time.Minute)}
		}, want{condition: "Healthy", activity: "Deploying", stuck: true, reason: "progress deadline", hop: platformstate.HopRollingOut}},

		{"Serving: the new version is ready, and Deploying has ended", func(f *fixture) {
			f.events = []obj{accepted("2.0.0", c2, 3*time.Minute)}
			synced(f.app, c2, 2*time.Minute)
			f.deployments = []obj{servedDeployment("2.0.0")}
			f.pods = []obj{readyPod("shop-b", "2.0.0")}
		}, want{condition: "Healthy"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := env()
			tc.setup(f)
			got := f.state(t)
			tc.want.check(t, got)
			if len(got.Deploys) != 1 {
				t.Fatalf("Deploys = %+v, want the one Deploy", got.Deploys)
			}
			d := got.Deploys[0]
			if d.Tag != "2.0.0" || d.Commit != c2 || d.At == nil || !d.At.Before(now) || d.Promote || d.Preview || d.Refused {
				t.Errorf("Deploy = %+v, want 2.0.0 from c2, accepted before now", d)
			}
			if tc.want.hop == "" && d.Hop != platformstate.HopServing {
				t.Errorf("Deploy is at %s, want Serving", d.Hop)
			}
		})
	}
}

// The hops are joined by the Platform repository commit; where ArgoCD no
// longer shows it, or there is no Event, by the image tag.
func TestDeployJoinedByCommitOrByTag(t *testing.T) {
	t.Run("by commit: ArgoCD synced the Deploy's commit", func(t *testing.T) {
		f := env()
		f.events = []obj{accepted("2.0.0", c2[:7], 2*time.Minute)} // an abbreviated commit still matches
		synced(f.app, c2, time.Minute)
		f.deployments = []obj{rollingDeployment("2.0.0")}
		f.pods = []obj{readyPod("shop-a", "1.0.0"), notReadyPod("shop-b", "2.0.0", 10*time.Second)}
		want{condition: "Healthy", activity: "Deploying", hop: platformstate.HopRollingOut}.check(t, f.state(t))
	})
	t.Run("by tag: ArgoCD skipped to a later commit that carries it", func(t *testing.T) {
		f := env()
		f.events = []obj{accepted("2.0.0", c2, 4*time.Minute)}
		// c3 is a later change (a size), synced together with c2.
		looked(f.app, c3, "Synced", 30*time.Second)
		syncing(f.app, c3, "Succeeded", "", time.Minute)
		synced(f.app, c3, time.Minute)
		f.deployments = []obj{rollingDeployment("2.0.0")}
		f.pods = []obj{readyPod("shop-a", "1.0.0"), notReadyPod("shop-b", "2.0.0", 10*time.Second)}
		want{condition: "Healthy", activity: "Deploying", hop: platformstate.HopRollingOut}.check(t, f.state(t))
	})
	t.Run("by tag without an Event: the migration Job runs the new tag", func(t *testing.T) {
		f := env()
		looked(f.app, c2, "OutOfSync", 30*time.Second)
		syncing(f.app, c2, "Running", "", 20*time.Second)
		f.jobs = []obj{migration("2.0.0", 15*time.Second, "")}
		got := f.state(t)
		want{condition: "Healthy", activity: "Deploying", hop: platformstate.HopApplying}.check(t, got)
		if d := got.Activity.Deploy; d.Tag != "2.0.0" || d.Commit != "" || d.At != nil {
			t.Errorf("Deploy = %+v, want 2.0.0 found by tag, with no commit or time", d)
		}
	})
	t.Run("by tag without an Event: the Deployment rolls the new tag out", func(t *testing.T) {
		f := env()
		f.deployments = []obj{rollingDeployment("2.0.0")}
		f.pods = []obj{readyPod("shop-a", "1.0.0"), notReadyPod("shop-b", "2.0.0", 10*time.Second)}
		got := f.state(t)
		want{condition: "Healthy", activity: "Deploying", hop: platformstate.HopRollingOut}.check(t, got)
		if d := got.Activity.Deploy; d.Tag != "2.0.0" {
			t.Errorf("Deploy = %+v, want 2.0.0", d)
		}
	})
	t.Run("without an Event, a change that keeps the tag is Updating, not a Deploy", func(t *testing.T) {
		f := env()
		f.deployments = []obj{rollingDeployment("1.0.0")}
		f.pods = []obj{readyPod("shop-a", "1.0.0"), notReadyPod("shop-b", "1.0.0", 10*time.Second)}
		want{condition: "Healthy", activity: "Updating"}.check(t, f.state(t))
	})
}

// A second Deploy before the first finishes supersedes it; the first is
// not stuck.
func TestSupersededDeploy(t *testing.T) {
	f := env()
	f.events = []obj{accepted("2.0.0", c2, 3*time.Minute), accepted("3.0.0", c3, time.Minute)}
	looked(f.app, c3, "OutOfSync", 30*time.Second)
	syncing(f.app, c3, "Running", "", 20*time.Second)
	got := f.state(t)
	want{condition: "Healthy", activity: "Deploying", hop: platformstate.HopApplying}.check(t, got)
	if len(got.Deploys) != 2 {
		t.Fatalf("Deploys = %+v", got.Deploys)
	}
	first, second := got.Deploys[0], got.Deploys[1]
	if first.Tag != "2.0.0" || first.SupersededBy != "3.0.0" || first.Stuck || first.Hop != "" {
		t.Errorf("first = %+v, want superseded by 3.0.0 and not stuck", first)
	}
	if second.Tag != "3.0.0" || got.Activity.Deploy.Tag != "3.0.0" {
		t.Errorf("second = %+v, Activity's = %+v, want 3.0.0 under way", second, got.Activity.Deploy)
	}

	// A Deploy that reached ArgoCD before the next is not superseded.
	f = env()
	f.events = []obj{accepted("2.0.0", c2, 30*time.Minute), accepted("3.0.0", c3, time.Minute)}
	synced(f.app, c2, 29*time.Minute)
	looked(f.app, c3, "OutOfSync", 30*time.Second)
	got = f.state(t)
	if first := got.Deploys[0]; first.SupersededBy != "" || first.Hop != platformstate.HopServing {
		t.Errorf("first = %+v, want it to have landed", first)
	}
}

// A refused Deploy is recorded with its reason and never becomes
// Deploying, and does not hide a Deploy already under way.
func TestRefusedDeploy(t *testing.T) {
	f := env()
	f.events = []obj{refused("2.0.0", "image ghcr.io/itema-as/shop:2.0.0 does not exist", 10*time.Second)}
	got := f.state(t)
	want{condition: "Healthy"}.check(t, got)
	if len(got.Deploys) != 1 {
		t.Fatalf("Deploys = %+v", got.Deploys)
	}
	if d := got.Deploys[0]; !d.Refused || d.Reason != "image ghcr.io/itema-as/shop:2.0.0 does not exist" || d.Hop != "" || d.Stuck || d.Commit != "" {
		t.Errorf("Deploy = %+v, want refused with the reason in plain words, and no hop", d)
	}

	f = env()
	f.events = []obj{accepted("2.0.0", c2, time.Minute), refused("2.0.1", "image ghcr.io/itema-as/shop:2.0.1 does not exist", 10*time.Second)}
	looked(f.app, c2, "OutOfSync", 30*time.Second)
	syncing(f.app, c2, "Running", "", 20*time.Second)
	got = f.state(t)
	want{condition: "Healthy", activity: "Deploying", hop: platformstate.HopApplying}.check(t, got)
	if got.Activity.Deploy.Tag != "2.0.0" || !got.Deploys[1].Refused {
		t.Errorf("Deploys = %+v, want 2.0.0 under way and 2.0.1 refused", got.Deploys)
	}
}

// A Promote travels the same hops, marked as a Promote.
func TestPromote(t *testing.T) {
	f := env()
	f.events = []obj{promoted("2.0.0", c2, 2*time.Minute)}
	synced(f.app, c2, time.Minute)
	f.deployments = []obj{rollingDeployment("2.0.0")}
	f.pods = []obj{readyPod("shop-a", "1.0.0"), notReadyPod("shop-b", "2.0.0", 10*time.Second)}
	got := f.state(t)
	want{condition: "Healthy", activity: "Deploying", hop: platformstate.HopRollingOut}.check(t, got)
	if d := got.Activity.Deploy; !d.Promote || d.Tag != "2.0.0" || d.Commit != c2 {
		t.Errorf("Deploy = %+v, want a Promote of 2.0.0", d)
	}
}

// A Preview Environment's Deploy never passes the Deploy gate, so it is
// found from the tag its ApplicationSet sets, and starts at Applying.
func TestPreviewDeployStartsAtApplying(t *testing.T) {
	preview := func() *fixture {
		f := env()
		f.name = "pr-7"
		f.app = argoApp("pr-7", "Synced", "Healthy")
		delete(f.app["metadata"].(obj)["labels"].(obj), "iidp.itema.no/environment")
		f.app["spec"].(obj)["sources"] = []any{
			obj{"repoURL": "https://github.com/Itema-as/iidp-platform.git", "ref": "values"},
			obj{"chart": "application", "helm": obj{"valueFiles": []any{"$values/applications/shop/staging/values.yaml"},
				"valuesObject": obj{"environment": "pr-7", "image": obj{"tag": "sha-new"}, "size": "small"}}},
		}
		f.deployments = []obj{servedDeployment("sha-old")}
		f.pods = []obj{readyPod("shop-a", "sha-old")}
		return f
	}

	t.Run("before ArgoCD has started: already Applying", func(t *testing.T) {
		got := preview().state(t)
		want{condition: "Healthy", activity: "Deploying", hop: platformstate.HopApplying}.check(t, got)
		if d := got.Activity.Deploy; !d.Preview || d.Tag != "sha-new" || d.At != nil {
			t.Errorf("Deploy = %+v, want a preview's Deploy of sha-new", d)
		}
	})
	t.Run("rolling out", func(t *testing.T) {
		f := preview()
		f.deployments = []obj{rollingDeployment("sha-new")}
		f.pods = []obj{readyPod("shop-a", "sha-old"), notReadyPod("shop-b", "sha-new", 10*time.Second)}
		want{condition: "Healthy", activity: "Deploying", hop: platformstate.HopRollingOut}.check(t, f.state(t))
	})
	t.Run("serving: no Deploy under way", func(t *testing.T) {
		f := preview()
		f.deployments = []obj{servedDeployment("sha-new")}
		f.pods = []obj{readyPod("shop-b", "sha-new")}
		got := f.state(t)
		want{condition: "Healthy"}.check(t, got)
		if len(got.Deploys) != 0 {
			t.Errorf("Deploys = %+v, want none", got.Deploys)
		}
	})
	t.Run("the first image: Arriving, from Applying", func(t *testing.T) {
		f := preview()
		f.app["metadata"].(obj)["creationTimestamp"] = ago(40 * time.Minute)
		f.deployments, f.pods = nil, nil
		syncing(f.app, c1, "Running", "", 10*time.Second)
		got := f.state(t)
		if got.Name != "pr-7" {
			t.Errorf("name = %q", got.Name)
		}
		want{condition: "Healthy", activity: "Arriving", hop: platformstate.HopApplying}.check(t, got)
	})
	t.Run("the image cannot be pulled: stuck at Rolling out", func(t *testing.T) {
		f := preview()
		f.deployments = []obj{rollingDeployment("sha-new")}
		f.pods = []obj{readyPod("shop-a", "sha-old"), waitingPod("shop-b", "sha-new", "ImagePullBackOff")}
		want{condition: "Healthy", activity: "Deploying", stuck: true, reason: "cannot be pulled", hop: platformstate.HopRollingOut}.check(t, f.state(t))
	})
}

// A first Deploy into an Unreleased Environment makes it Arriving again.
func TestFirstDeployIntoAnUnreleasedEnvironmentIsArriving(t *testing.T) {
	f := env()
	f.deployments, f.pods = nil, nil
	want{condition: "Healthy", activity: "Unreleased"}.check(t, f.state(t))

	f.events = []obj{accepted("1.0.0", c2, 30*time.Second)}
	looked(f.app, c2, "OutOfSync", 10*time.Second)
	want{condition: "Healthy", activity: "Arriving", hop: platformstate.HopWaitingForArgoCD}.check(t, f.state(t))
}

// The Deploy gate's Events about other Environments are not this one's.
func TestOtherEnvironmentsEventsAreIgnored(t *testing.T) {
	f := env()
	e := accepted("2.0.0", c2, 20*time.Second)
	e["regarding"].(obj)["name"] = "shop-staging"
	e["metadata"].(obj)["annotations"].(obj)["iidp.itema.no/environment"] = "staging"
	other := gateEvent("BackOff", "", "", "", "Back-off restarting failed container", time.Minute)
	f.events = []obj{e, other}
	got := f.state(t)
	want{condition: "Healthy"}.check(t, got)
	if len(got.Deploys) != 0 {
		t.Errorf("Deploys = %+v, want none", got.Deploys)
	}
}
