package argus

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Itema-as/iidp/internal/platformstate"
)

func TestFeedKeeps200NotesOr24Hours(t *testing.T) {
	var f feed
	for i := 0; i < 250; i++ {
		f.add(Note{At: t0.Add(time.Duration(i) * time.Second), Message: fmt.Sprint(i)}, t0.Add(250*time.Second))
	}
	got := f.list(t0.Add(250 * time.Second))
	if len(got) != FeedSize || got[0].Message != "50" || got[0].ID != 51 || got[len(got)-1].Message != "249" || got[len(got)-1].ID != 250 {
		t.Errorf("after 250 notes: %d kept, from %+v to %+v; want the last 200", len(got), got[0], got[len(got)-1])
	}

	// Fewer than 200, but some older than 24 hours.
	f = feed{}
	for i := 0; i < 10; i++ {
		f.add(Note{At: t0.Add(time.Duration(i) * time.Hour), Message: fmt.Sprint(i)}, t0.Add(time.Duration(i)*time.Hour))
	}
	later := t0.Add(24*time.Hour + 3*time.Hour)
	got = f.list(later)
	if len(got) != 7 || got[0].Message != "3" {
		t.Errorf("at t0+27h: %+v, want the notes of the last 24 hours, from the one at t0+3h", got)
	}
	// Adding also drops what has aged out.
	f.add(Note{At: t0.Add(48 * time.Hour), Message: "new"}, t0.Add(48*time.Hour))
	if got := f.list(t0.Add(48 * time.Hour)); len(got) != 1 || got[0].Message != "new" || got[0].ID != 11 {
		t.Errorf("at t0+48h: %+v, want only the new note, still numbered after the others", got)
	}
}

// On start the feed is seeded from what the cluster still says of the
// past: ArgoCD's history and last operation, the Deploy gate's Events,
// and the last hour of Warning Events. Every seeded note is feed-only,
// and the seam follows them: "since Argus restarted at hh:mm", on Oslo's
// clock. What Argus sees afterwards comes after the seam.
func TestSeedFillsTheFeedAndMarksTheSeam(t *testing.T) {
	s, clock := newStore(t)
	app := argoApp("shop", "prod")
	st := app["status"].(obj)
	st["history"] = []any{
		obj{"id": 1, "revisions": []any{"0.3.0", "0000000000000000000000000000000000000000"}, "deployedAt": at(-30 * time.Hour)},
		obj{"id": 2, "revisions": []any{"0.4.0", commit1}, "deployedAt": at(-2 * time.Hour)},
	}
	st["operationState"] = obj{"phase": "Failed", "message": "one or more objects failed to apply", "startedAt": at(-11 * time.Minute), "finishedAt": at(-10 * time.Minute)}
	cm := component("cert-manager")
	cm["status"].(obj)["history"] = []any{obj{"id": 7, "revision": "v1.21.2", "deployedAt": at(-5 * time.Hour)}}
	put(t, s, app, envDeployment("shop", "prod", "1.0.0"), pod("shop-prod", "shop-a", "shop", "1.0.0", true, -time.Hour), cm,
		gateEvent("shop", "prod", platformstate.ReasonDeployAccepted, "deploy", "1.0.1", "2222222222222222222222222222222222222222", "Deploy shop prod 1.0.1 accepted", -20*time.Minute),
		gateEvent("shop", "prod", platformstate.ReasonDeployRefused, "deploy", "9.9.9", "", "Deploy shop prod refused: image ghcr.io/itema-as/shop:9.9.9 does not exist", -15*time.Minute),
		warning("shop-prod", "shop-a", "BackOff", "Back-off restarting failed container", -30*time.Minute),
		warning("shop-prod", "shop-b", "FailedScheduling", "0/1 nodes are available", -2*time.Hour),
		warning("kube-system", "coredns-1", "Unhealthy", "Readiness probe failed", -5*time.Minute),
	)
	s.Recompute() // before the seed: no notes, however much is there
	if feed := s.Snapshot().Feed; len(feed) != 0 {
		t.Fatalf("feed before the seed = %+v", feed)
	}
	s.Seed()

	var got []string
	for _, n := range s.Snapshot().Feed {
		if n.Seam {
			got = append(got, "SEAM "+n.Message)
			if n.Seeded || !n.FeedOnly || !n.At.Equal(t0) {
				t.Errorf("seam = %+v", n)
			}
			continue
		}
		if !n.Seeded || !n.FeedOnly {
			t.Errorf("seeded note %+v is not marked seeded and feed-only", n)
		}
		where := n.Place.Component
		if n.Place.Application != "" {
			where = n.Place.Application + "/" + n.Place.Environment
		}
		got = append(got, fmt.Sprintf("%s %s %s: %s", n.At.Sub(t0), n.Loudness, where, n.Message))
	}
	want := []string{
		"-5h0m0s quiet cert-manager: cert-manager synced v1.21.2",
		"-2h0m0s quiet shop/prod: shop prod synced commit 1111111",
		"-30m0s quiet shop/prod: Pod shop-a: BackOff: Back-off restarting failed container",
		"-20m0s quiet shop/prod: Deploy 1.0.1 to shop prod accepted",
		"-15m0s quiet shop/prod: Deploy 9.9.9 to shop prod refused: image ghcr.io/itema-as/shop:9.9.9 does not exist",
		"-10m0s loud shop/prod: shop prod: ArgoCD's sync failed: one or more objects failed to apply",
		"-5m0s quiet k3s: Pod coredns-1: Unhealthy: Readiness probe failed",
		"SEAM since Argus restarted at 12:00",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("seeded feed:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// After the seam, what Argus sees: a new Warning Event, and a
	// change.
	clock.add(time.Minute)
	put(t, s, warning("shop-prod", "shop-a", "Unhealthy", "Liveness probe failed", time.Minute))
	put(t, s, gateEvent("shop", "prod", platformstate.ReasonDeployRefused, "deploy", "9.9.8", "", "Deploy shop prod refused: no", time.Minute))
	s.Recompute()
	feed := s.Snapshot().Feed
	var after []string
	for _, n := range feed[len(want):] {
		if n.Seeded || n.Seam {
			t.Errorf("note after the seam %+v is marked seeded", n)
		}
		after = append(after, n.Message)
	}
	if got := strings.Join(after, "; "); got != "Pod shop-a: Unhealthy: Liveness probe failed; Deploy 9.9.8 to shop prod refused: no" {
		t.Errorf("after the seam: %s", got)
	}

	// Seeding twice does nothing.
	s.Seed()
	if n := len(s.Snapshot().Feed); n != len(feed) {
		t.Errorf("a second Seed changed the feed from %d to %d notes", len(feed), n)
	}
}
