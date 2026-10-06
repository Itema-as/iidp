package deploygate_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Itema-as/iidp/internal/deploygate"
	"github.com/Itema-as/iidp/internal/kubeevent"
)

// fakeEvents keeps the Events the gate records. err, when set, is what
// every CreateEvent answers; hold, when set, blocks every CreateEvent until
// it is closed.
type fakeEvents struct {
	mu     sync.Mutex
	events []kubeevent.Event
	err    error
	hold   chan struct{}
}

func (f *fakeEvents) CreateEvent(ctx context.Context, event kubeevent.Event) error {
	if f.hold != nil {
		select {
		case <-f.hold:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.events = append(f.events, event)
	return nil
}

// recorded waits for the gate's Events in flight and returns them all.
func (f *fakeEvents) recorded(e *env) []kubeevent.Event {
	e.gate.WaitForEvents()
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]kubeevent.Event(nil), f.events...)
}

func withEvents(e *env) *fakeEvents {
	sink := &fakeEvents{}
	e.gate.Events = sink
	return sink
}

// checkEvent asserts everything about an Event but its note, which each
// test checks itself: the ArgoCD Application it regards, in argocd, and
// exactly the annotations wanted.
func checkEvent(t *testing.T, event kubeevent.Event, reason, eventType, action, argoApp string, annotations map[string]string) {
	t.Helper()
	if event.APIVersion != "events.k8s.io/v1" || event.Kind != "Event" {
		t.Errorf("apiVersion/kind = %s/%s, want events.k8s.io/v1 Event", event.APIVersion, event.Kind)
	}
	if event.Reason != reason || event.Type != eventType || event.Action != action {
		t.Errorf("reason, type, action = %s %s %s, want %s %s %s", event.Reason, event.Type, event.Action, reason, eventType, action)
	}
	// Kubernetes requires an Event to be in the namespace of the object it
	// is about.
	want := kubeevent.ObjectReference{APIVersion: "argoproj.io/v1alpha1", Kind: "Application", Namespace: "argocd", Name: argoApp}
	if event.Regarding != want || event.Metadata.Namespace != "argocd" {
		t.Errorf("regarding = %+v in namespace %q, want %+v in argocd", event.Regarding, event.Metadata.Namespace, want)
	}
	if !strings.HasPrefix(event.Metadata.Name, argoApp+".") {
		t.Errorf("name = %q, want it to start with %s.", event.Metadata.Name, argoApp)
	}
	if fmt.Sprint(event.Metadata.Annotations) != fmt.Sprint(annotations) {
		t.Errorf("annotations = %v, want exactly %v", event.Metadata.Annotations, annotations)
	}
	if event.ReportingController != "iidp.itema.no/deploy-gate" || event.ReportingInstance == "" {
		t.Errorf("reportingController, reportingInstance = %q %q", event.ReportingController, event.ReportingInstance)
	}
	// The API server reads eventTime as a MicroTime: six fractional digits.
	at, err := time.Parse("2006-01-02T15:04:05.000000Z07:00", event.EventTime)
	if err != nil || time.Since(at) > time.Minute || !strings.HasSuffix(event.EventTime, "Z") || len(event.EventTime) != len("2006-01-02T15:04:05.000000Z") {
		t.Errorf("eventTime = %q (%v), want now as a MicroTime in UTC", event.EventTime, err)
	}
}

func TestAnAcceptedDeployIsRecordedAsAnEvent(t *testing.T) {
	e := newEnv(t)
	sink := withEvents(e)
	addApplication(t, e.platform, "shop", true, binding(shopRepoID, orgID))

	status, body := e.deploy(e.issuer.claims(), "shop", "auto", "1a2b3c4")
	if status != http.StatusOK {
		t.Fatalf("status = %d: %v", status, body)
	}
	events := sink.recorded(e)
	if len(events) != 1 {
		t.Fatalf("recorded %d Events, want 1: %+v", len(events), events)
	}
	checkEvent(t, events[0], "DeployAccepted", "Normal", "Deploy", "shop-staging", map[string]string{
		"iidp.itema.no/application": "shop",
		"iidp.itema.no/environment": "staging",
		"iidp.itema.no/tag":         "1a2b3c4",
		"iidp.itema.no/commit":      body["commit"].(string),
		"iidp.itema.no/kind":        "deploy",
	})
	if events[0].Note != "Deploy shop staging 1a2b3c4 accepted" {
		t.Errorf("note = %q", events[0].Note)
	}
	if body["commit"] != strings.TrimSpace(gitRun(t, cloneMain(t, e.platform), "rev-parse", "HEAD")) {
		t.Errorf("the response's commit %v is not the Platform repository's head", body["commit"])
	}

	// A second Deploy of the same Environment gets its own Event.
	if status, body := e.deploy(e.issuer.claims(), "shop", "auto", "5d6e7f8"); status != http.StatusOK {
		t.Fatalf("second deploy: %d %v", status, body)
	}
	events = sink.recorded(e)
	if len(events) != 2 || events[0].Metadata.Name == events[1].Metadata.Name {
		t.Errorf("Events after two Deploys = %+v, want two with different names", events)
	}
}

func TestAnAcceptedPromoteIsRecordedAsAnEvent(t *testing.T) {
	e := newEnv(t)
	sink := withEvents(e)
	addApplication(t, e.platform, "shop", true, binding(shopRepoID, orgID))
	claims := e.issuer.claims()
	claims["ref"], claims["ref_type"] = "refs/tags/v1.2.3", "tag"

	status, body := e.deploy(claims, "shop", "auto", "1.2.3")
	if status != http.StatusOK {
		t.Fatalf("status = %d: %v", status, body)
	}
	events := sink.recorded(e)
	if len(events) != 1 {
		t.Fatalf("recorded %d Events, want 1: %+v", len(events), events)
	}
	checkEvent(t, events[0], "DeployAccepted", "Normal", "Promote", "shop-prod", map[string]string{
		"iidp.itema.no/application": "shop",
		"iidp.itema.no/environment": "prod",
		"iidp.itema.no/tag":         "1.2.3",
		"iidp.itema.no/commit":      body["commit"].(string),
		"iidp.itema.no/kind":        "promote",
	})
	if events[0].Note != "Promote shop prod 1.2.3 accepted" {
		t.Errorf("note = %q", events[0].Note)
	}

	// The same Promote again commits nothing. It is still accepted, with
	// no commit to name.
	if status, body := e.deploy(claims, "shop", "prod", "1.2.3"); status != http.StatusOK || body["unchanged"] != true {
		t.Fatalf("a repeated promote = %d %v, want 200 and unchanged", status, body)
	}
	events = sink.recorded(e)
	if len(events) != 2 {
		t.Fatalf("recorded %d Events, want 2", len(events))
	}
	checkEvent(t, events[1], "DeployAccepted", "Normal", "Promote", "shop-prod", map[string]string{
		"iidp.itema.no/application": "shop",
		"iidp.itema.no/environment": "prod",
		"iidp.itema.no/tag":         "1.2.3",
		"iidp.itema.no/kind":        "promote",
	})
	if events[1].Note != "Promote shop prod 1.2.3 accepted: prod already runs it, so nothing was committed" {
		t.Errorf("note = %q", events[1].Note)
	}
}

func TestARefusalForAMissingImageIsRecordedWithItsReason(t *testing.T) {
	e := newEnv(t)
	sink := withEvents(e)
	addApplication(t, e.platform, "shop", true, binding(shopRepoID, orgID))
	e.registry.set(func(f *fakeRegistry) { f.missing["itema-as/shop:1a2b3c4"] = true })

	status, body := e.deploy(e.issuer.claims(), "shop", "auto", "1a2b3c4")
	e.refused(status, body, http.StatusUnprocessableEntity, "does not exist")
	events := sink.recorded(e)
	if len(events) != 1 {
		t.Fatalf("recorded %d Events, want 1: %+v", len(events), events)
	}
	checkEvent(t, events[0], "DeployRefused", "Warning", "Deploy", "shop-staging", map[string]string{
		"iidp.itema.no/application": "shop",
		"iidp.itema.no/environment": "staging",
		"iidp.itema.no/tag":         "1a2b3c4",
		"iidp.itema.no/kind":        "deploy",
		"iidp.itema.no/refusal":     "422",
	})
	want := "Deploy shop staging refused: the image ghcr.io/itema-as/shop:1a2b3c4 does not exist, so nothing was deployed."
	if !strings.HasPrefix(events[0].Note, want) {
		t.Errorf("note = %q, want it to start %q", events[0].Note, want)
	}
}

// A refusal after the Environment is known, other than the image check's,
// is recorded the same way.
func TestARefusedMigrationCommandIsRecorded(t *testing.T) {
	e := newEnv(t)
	sink := withEvents(e)
	addPostgresApplication(t, e.platform, "shop", false, false, "")

	status, body := e.deployWithMigration(e.issuer.claims(), "auto", "sha1", ptr("npm run migrate"))
	e.refused(status, body, http.StatusConflict, "Add the Postgres Capability first")
	events := sink.recorded(e)
	if len(events) != 1 || events[0].Reason != "DeployRefused" || events[0].Metadata.Annotations["iidp.itema.no/refusal"] != "409" ||
		!strings.Contains(events[0].Note, "Deploy shop prod refused: the Environment has no Postgres Capability") {
		t.Errorf("Events = %+v, want one DeployRefused for shop-prod with 409", events)
	}
}

// No Event without an Environment behind the refusal: nothing is recorded
// until the caller has proved to be the Application's own repository and
// the gate knows which Environment it deploys.
func TestARefusalWithNoApplicationBehindItIsNotRecorded(t *testing.T) {
	for _, tc := range []struct {
		name   string
		seed   func(e *env)
		claims func(e *env) map[string]any
		app    string
		env    string
		status int
	}{
		{"an unknown Application", nil, nil, "nothing", "auto", http.StatusNotFound},
		{"an unbound Application", func(e *env) { addApplication(t, e.platform, "shop", false, "") }, nil, "shop", "auto", http.StatusForbidden},
		{"another repository of the org", nil, func(e *env) map[string]any {
			c := e.issuer.claims()
			c["repository"], c["repository_id"] = "Itema-as/other", "999"
			return c
		}, "shop", "auto", http.StatusForbidden},
		{"another org", nil, func(e *env) map[string]any {
			c := e.issuer.claims()
			c["repository_owner"], c["repository_owner_id"] = "someone", "777"
			return c
		}, "shop", "auto", http.StatusForbidden},
		{"a disallowed ref", nil, func(e *env) map[string]any {
			c := e.issuer.claims()
			c["ref"] = "refs/heads/feature"
			return c
		}, "shop", "auto", http.StatusForbidden},
		{"an Environment the Application does not have", nil, nil, "shop", "staging", http.StatusNotFound},
		// main may not deploy prod when there is a staging: the gate never
		// decides on an Environment for the call.
		{"an Environment the ref may not deploy", func(e *env) { addApplication(t, e.platform, "shop", true, binding(shopRepoID, orgID)) }, nil, "shop", "prod", http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			sink := withEvents(e)
			if tc.seed != nil {
				tc.seed(e)
			} else {
				addApplication(t, e.platform, "shop", false, binding(shopRepoID, orgID))
			}
			claims := e.issuer.claims()
			if tc.claims != nil {
				claims = tc.claims(e)
			}
			status, body := e.deploy(claims, tc.app, tc.env, "sha1")
			e.refused(status, body, tc.status)
			if events := sink.recorded(e); len(events) != 0 {
				t.Errorf("recorded %+v, want no Event", events)
			}
		})
	}

	// Nor for a token the gate does not trust.
	e := newEnv(t)
	sink := withEvents(e)
	addApplication(t, e.platform, "shop", false, binding(shopRepoID, orgID))
	status, body := e.call("not-a-token", deploygate.Request{Application: "shop", Environment: "auto", Tag: "sha1"})
	e.refused(status, body, http.StatusUnauthorized)
	if events := sink.recorded(e); len(events) != 0 {
		t.Errorf("recorded %+v for an untrusted token, want no Event", events)
	}
}

func TestAnEventThatCannotBeRecordedDoesNotFailTheDeploy(t *testing.T) {
	e := newEnv(t)
	sink := withEvents(e)
	sink.err = errors.New(`the Kubernetes API answered HTTP 403: events.events.k8s.io is forbidden`)
	var logs bytes.Buffer
	e.gate.Log = slog.New(slog.NewTextHandler(&logs, nil))
	addApplication(t, e.platform, "shop", false, binding(shopRepoID, orgID))

	status, body := e.deploy(e.issuer.claims(), "shop", "auto", "1a2b3c4")
	if status != http.StatusOK || body["commit"] == "" {
		t.Fatalf("status = %d: %v, want the Deploy committed", status, body)
	}
	if got := headSubject(t, e.platform); got != "Deploy shop prod 1a2b3c4" {
		t.Errorf("head = %q, want the Deploy", got)
	}
	e.gate.WaitForEvents()
	if !strings.Contains(logs.String(), "the Deploy's Event was not recorded") || !strings.Contains(logs.String(), "forbidden") {
		t.Errorf("logs = %s, want the failure logged", logs.String())
	}
}

func TestAnEventThatHangsDoesNotDelayTheDeploy(t *testing.T) {
	e := newEnv(t)
	sink := withEvents(e)
	sink.hold = make(chan struct{})
	addApplication(t, e.platform, "shop", false, binding(shopRepoID, orgID))

	answered := make(chan int, 1)
	go func() {
		status, _ := e.deploy(e.issuer.claims(), "shop", "auto", "1a2b3c4")
		answered <- status
	}()
	select {
	case status := <-answered:
		if status != http.StatusOK {
			t.Errorf("status = %d, want 200", status)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the Deploy was not answered while its Event hung")
	}
	close(sink.hold)
	if events := sink.recorded(e); len(events) != 1 {
		t.Errorf("recorded %d Events once released, want 1", len(events))
	}
}
