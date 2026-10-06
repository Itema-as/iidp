package kubeevent_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Itema-as/iidp/internal/kubeevent"
	"github.com/Itema-as/iidp/internal/platformstate"
)

func TestKubePostsTheEventToItsNamespace(t *testing.T) {
	var gotMethod, gotPath, gotAuth, gotType string
	var got map[string]any
	answer := http.StatusCreated
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotAuth, gotType = r.Method, r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&got)
		if answer != http.StatusCreated {
			http.Error(w, `{"kind":"Status","reason":"Forbidden"}`, answer)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"kind":"Event"}`))
	}))
	defer api.Close()
	sink := &kubeevent.Kube{Kube: &platformstate.Kube{BaseURL: api.URL, Token: func() (string, error) { return "sa-token", nil }}}
	event := kubeevent.Event{
		APIVersion: "events.k8s.io/v1", Kind: "Event",
		Metadata:  kubeevent.Metadata{Name: "shop-prod.1", Namespace: "argocd", Annotations: map[string]string{"iidp.itema.no/tag": "1a2b3c4"}},
		EventTime: "2026-09-27T10:00:00.000000Z", ReportingController: "iidp.itema.no/deploy-gate", ReportingInstance: "gate-pod",
		Action: "Deploy", Reason: "DeployAccepted", Type: "Normal", Note: "Deploy shop prod 1a2b3c4 accepted",
		Regarding: kubeevent.ObjectReference{APIVersion: "argoproj.io/v1alpha1", Kind: "Application", Namespace: "argocd", Name: "shop-prod"},
	}
	if err := sink.CreateEvent(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPost || gotPath != "/apis/events.k8s.io/v1/namespaces/argocd/events" || gotAuth != "Bearer sa-token" || gotType != "application/json" {
		t.Errorf("request = %s %s, Authorization %q, Content-Type %q", gotMethod, gotPath, gotAuth, gotType)
	}
	regarding, _ := got["regarding"].(map[string]any)
	if got["reason"] != "DeployAccepted" || got["eventTime"] != "2026-09-27T10:00:00.000000Z" || regarding["name"] != "shop-prod" || regarding["namespace"] != "argocd" {
		t.Errorf("body = %v", got)
	}

	answer = http.StatusForbidden
	err := sink.CreateEvent(context.Background(), event)
	var refusal *platformstate.StatusError
	if !errors.As(err, &refusal) || refusal.Status != http.StatusForbidden {
		t.Errorf("CreateEvent = %v, want the API server's 403", err)
	}
}

// The API server refuses an Event whose note is over 1024 bytes, and a
// refusal's reason is the gate's or the tunnel's own message, so a long one
// is cut on a character boundary.
func TestTruncateKeepsNotesWithinTheAPIServersLimit(t *testing.T) {
	if got := kubeevent.Truncate("short", kubeevent.NoteLimit); got != "short" {
		t.Errorf("Truncate(short) = %q", got)
	}
	long := strings.Repeat("æ", 700) // 1400 bytes
	got := kubeevent.Truncate(long, kubeevent.NoteLimit)
	if len(got) > kubeevent.NoteLimit || !utf8.ValidString(got) || !strings.HasSuffix(got, "…") {
		t.Errorf("Truncate = %d bytes, valid UTF-8 %t, want at most %d ending in …", len(got), utf8.ValidString(got), kubeevent.NoteLimit)
	}
	event := kubeevent.Environment("shop-prod", "iidp.itema.no/deploy-gate", "Deploy", "DeployRefused", "Warning", long, nil, time.Now())
	if len(event.Note) > kubeevent.NoteLimit {
		t.Errorf("an Event's note is %d bytes, want at most %d", len(event.Note), kubeevent.NoteLimit)
	}
}
