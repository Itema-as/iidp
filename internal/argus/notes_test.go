package argus

import (
	"strings"
	"testing"
	"time"

	"github.com/Itema-as/iidp/internal/platformstate"
)

// Each row of feed.go's loudness table, and the notes that only report
// something finished, from the change of one Environment or component.
func TestLoudnessTable(t *testing.T) {
	healthy := &platformstate.Condition{State: platformstate.Healthy}
	env := func(name string, c *platformstate.Condition, a *platformstate.Activity, deploys ...platformstate.Deploy) *platformstate.EnvironmentState {
		if deploys == nil {
			deploys = []platformstate.Deploy{}
		}
		return &platformstate.EnvironmentState{
			Environment: platformstate.Environment{Name: name, Condition: c, Activity: a, Image: &platformstate.Image{Tag: "1.0.0"}},
			Deploys:     deploys,
		}
	}
	activity := func(state string, stuck bool, reason string) *platformstate.Activity {
		return &platformstate.Activity{State: state, Stuck: stuck, Reason: reason}
	}
	accepted := t0.Add(-time.Minute)
	deploy := func(tag, hop string) platformstate.Deploy {
		return platformstate.Deploy{Tag: tag, At: &accepted, Hop: hop}
	}
	withCapability := func(e *platformstate.EnvironmentState, c platformstate.Capability) *platformstate.EnvironmentState {
		e.Capabilities = []platformstate.Capability{c}
		return e
	}
	postgres := func(warning string) platformstate.Capability {
		return platformstate.Capability{Type: platformstate.CapabilityPostgres, Name: "shop-db", Condition: platformstate.Condition{State: platformstate.Healthy, Warning: warning}}
	}

	type change struct {
		prev, next *platformstate.EnvironmentState
	}
	for _, tc := range []struct {
		name     string
		change   change
		want     string // loudness, then feed-only, then the message
		wantNone bool
	}{
		// Loud.
		{"Degraded", change{env("prod", healthy, nil), env("prod", &platformstate.Condition{State: platformstate.Degraded, Reason: "0 of 1 pods ready for over a minute"}, nil)},
			"loud: shop prod is Degraded: 0 of 1 pods ready for over a minute", false},
		{"a Deploy turning stuck", change{
			env("prod", healthy, &platformstate.Activity{State: platformstate.Deploying, Deploy: &platformstate.Deploy{Tag: "1.0.1", Hop: platformstate.HopApplying}}, deploy("1.0.1", platformstate.HopApplying)),
			env("prod", healthy, &platformstate.Activity{State: platformstate.Deploying, Stuck: true, Reason: "the migration failed", Deploy: &platformstate.Deploy{Tag: "1.0.1", Hop: platformstate.HopApplying, Stuck: true}},
				platformstate.Deploy{Tag: "1.0.1", At: &accepted, Hop: platformstate.HopApplying, Stuck: true, Reason: "the migration failed"})},
			"loud: shop prod: Deploy 1.0.1 is stuck at applying: the migration failed", false},
		{"an update turning stuck", change{env("prod", healthy, activity(platformstate.Updating, false, "")), env("prod", healthy, activity(platformstate.Updating, true, "ArgoCD's sync failed"))},
			"loud: shop prod: updating is stuck: ArgoCD's sync failed", false},
		{"a Capability Degraded", change{withCapability(env("prod", healthy, nil), postgres("")),
			withCapability(env("prod", healthy, nil), platformstate.Capability{Type: platformstate.CapabilityCustomDomain, Name: "www.shop.example", Condition: platformstate.Condition{State: platformstate.Degraded, Reason: "Certificate expired"}})},
			"loud: shop prod: custom domain www.shop.example is Degraded: Certificate expired", false},
		// Normal.
		{"arriving", change{nil, env("prod", healthy, activity(platformstate.Arriving, false, ""))}, "normal: shop prod is arriving", false},
		{"a first image into an Unreleased Environment", change{env("prod", healthy, activity(platformstate.Unreleased, false, "")), env("prod", healthy, activity(platformstate.Arriving, false, ""))},
			"normal: shop prod is arriving: its first image is on its way", false},
		{"leaving", change{env("prod", healthy, nil), env("prod", healthy, activity(platformstate.Leaving, false, ""))}, "normal: shop prod is leaving", false},
		{"Unknown", change{env("prod", healthy, nil), env("prod", &platformstate.Condition{State: platformstate.Unknown, Reason: "the state of shop-a is unknown"}, nil)},
			"normal: shop prod is Unknown: the state of shop-a is unknown", false},
		{"a Promote", change{env("prod", healthy, nil), env("prod", healthy, nil, platformstate.Deploy{Tag: "2.0.0", Promote: true, At: &accepted, Hop: platformstate.HopAccepted})},
			"normal: Promote 2.0.0 to shop prod accepted", false},
		// Quiet.
		{"a Deploy", change{env("staging", healthy, nil), env("staging", healthy, nil, deploy("1.0.1", platformstate.HopAccepted))}, "quiet: Deploy 1.0.1 to shop staging accepted", false},
		{"updating", change{env("prod", healthy, nil), env("prod", healthy, activity(platformstate.Updating, false, ""))}, "quiet: shop prod is updating", false},
		{"a Preview Environment arriving", change{nil, env("pr-7", healthy, activity(platformstate.Arriving, false, ""))}, "quiet: shop pr-7 is arriving", false},
		{"a Preview Environment leaving", change{env("pr-7", healthy, nil), env("pr-7", healthy, activity(platformstate.Leaving, false, ""))}, "quiet: shop pr-7 is leaving", false},
		{"a Preview Environment deploying", change{env("pr-7", healthy, nil), env("pr-7", healthy, nil, platformstate.Deploy{Tag: "sha-2", Preview: true, Hop: platformstate.HopApplying})},
			"quiet: Deploy sha-2 to shop pr-7 under way", false},
		{"a backup Warning", change{withCapability(env("prod", healthy, nil), postgres("")), withCapability(env("prod", healthy, nil), postgres("backups are failing: exit status 2"))},
			"quiet: shop prod: Postgres shop-db: backups are failing: exit status 2", false},
		{"a refused Deploy", change{env("prod", healthy, nil), env("prod", healthy, nil, platformstate.Deploy{Tag: "9.9.9", At: &accepted, Refused: true, Reason: "the image does not exist"})},
			"quiet: Deploy 9.9.9 to shop prod refused: the image does not exist", false},
		// Finished: the feed only.
		{"serving", change{env("prod", healthy, nil, deploy("1.0.1", platformstate.HopRollingOut)), env("prod", healthy, nil, deploy("1.0.1", platformstate.HopServing))},
			"quiet feed-only: shop prod serves 1.0.1", false},
		{"superseded", change{env("prod", healthy, nil, deploy("1.0.1", platformstate.HopWaitingForArgoCD)),
			env("prod", healthy, nil, platformstate.Deploy{Tag: "1.0.1", At: &accepted, SupersededBy: "1.0.2"})},
			"quiet feed-only: Deploy 1.0.1 to shop prod was superseded by 1.0.2", false},
		{"a Deploy found by its tag, serving", change{env("prod", healthy, nil, platformstate.Deploy{Tag: "1.0.0", Hop: platformstate.HopRollingOut}), env("prod", healthy, nil)},
			"quiet feed-only: shop prod serves 1.0.0", false},
		{"Healthy again", change{env("prod", &platformstate.Condition{State: platformstate.Degraded}, nil), env("prod", healthy, nil)}, "quiet feed-only: shop prod is Healthy again", false},
		{"arrived", change{env("prod", healthy, activity(platformstate.Arriving, false, "")), env("prod", healthy, nil)}, "quiet feed-only: shop prod has arrived and is serving", false},
		{"no longer stuck", change{env("prod", healthy, activity(platformstate.Updating, true, "x")), env("prod", healthy, activity(platformstate.Updating, false, ""))},
			"quiet feed-only: shop prod is no longer stuck", false},
		{"a Warning cleared", change{withCapability(env("prod", healthy, nil), postgres("backups are failing")), withCapability(env("prod", healthy, nil), postgres(""))},
			"quiet feed-only: shop prod: Postgres shop-db no longer has a warning", false},
		// No change, no note.
		{"nothing changed", change{env("prod", healthy, activity(platformstate.Updating, false, "")), env("prod", healthy, activity(platformstate.Updating, false, ""))}, "", true},
		{"still stuck", change{env("prod", healthy, activity(platformstate.Updating, true, "x")), env("prod", healthy, activity(platformstate.Updating, true, "x"))}, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var prev *platformstate.Application
			if tc.change.prev != nil {
				prev = &platformstate.Application{Name: "shop", Environments: []platformstate.EnvironmentState{*tc.change.prev}}
			}
			next := &platformstate.Application{Name: "shop", Environments: []platformstate.EnvironmentState{*tc.change.next}}
			notes := applicationNotes(prev, next, t0)
			if tc.wantNone {
				if len(notes) != 0 {
					t.Errorf("notes = %+v, want none", notes)
				}
				return
			}
			if len(notes) != 1 {
				t.Fatalf("notes = %+v, want one", notes)
			}
			if got := noteText(notes[0]); got != tc.want {
				t.Errorf("note = %q, want %q", got, tc.want)
			}
			if p := notes[0].Place; p.Application != "shop" || p.Environment != tc.change.next.Name {
				t.Errorf("place = %+v", p)
			}
		})
	}
}

func noteText(n Note) string {
	s := n.Loudness
	if n.FeedOnly {
		s += " feed-only"
	}
	return s + ": " + n.Message
}

// A Platform component: Degraded is loud, Updating quiet, and its end
// feed-only.
func TestComponentNotes(t *testing.T) {
	healthy := platformstate.Condition{State: platformstate.Healthy}
	c := func(cond platformstate.Condition, a *platformstate.Activity) *platformstate.Component {
		return &platformstate.Component{Name: "argocd", Condition: cond, Activity: a}
	}
	updating := &platformstate.Activity{State: platformstate.Updating}
	for _, tc := range []struct {
		name       string
		prev, next *platformstate.Component
		want       string
	}{
		{"Degraded", c(healthy, nil), c(platformstate.Condition{State: platformstate.Degraded, Reason: "argocd-server-1 is in CrashLoopBackOff"}, nil), "loud: argocd is Degraded: argocd-server-1 is in CrashLoopBackOff"},
		{"Unknown", c(healthy, nil), c(platformstate.Condition{State: platformstate.Unknown, Reason: "ArgoCD reports its health as Unknown"}, nil), "normal: argocd is Unknown: ArgoCD reports its health as Unknown"},
		{"Updating", c(healthy, nil), c(healthy, updating), "quiet: argocd is updating"},
		{"stuck", c(healthy, updating), c(healthy, &platformstate.Activity{State: platformstate.Updating, Stuck: true, Reason: "ArgoCD's sync failed"}), "loud: argocd: updating is stuck: ArgoCD's sync failed"},
		{"updated", c(healthy, updating), c(healthy, nil), "quiet feed-only: argocd has finished updating"},
		{"recovered", c(platformstate.Condition{State: platformstate.Degraded}, nil), c(healthy, nil), "quiet feed-only: argocd is Healthy again"},
		{"gone", c(healthy, nil), nil, "quiet feed-only: argocd is gone"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			notes := componentNotes("argocd", tc.prev, tc.next, t0)
			var got []string
			for _, n := range notes {
				if n.Place != (Place{Component: "argocd"}) {
					t.Errorf("place = %+v", n.Place)
				}
				got = append(got, noteText(n))
			}
			if strings.Join(got, "; ") != tc.want {
				t.Errorf("notes = %q, want %q", got, tc.want)
			}
		})
	}
}
