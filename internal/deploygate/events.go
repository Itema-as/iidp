package deploygate

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Itema-as/iidp/internal/oidc"
	"github.com/Itema-as/iidp/internal/platformrepo"
	"github.com/Itema-as/iidp/internal/platformstate"
	"github.com/Itema-as/iidp/internal/render"
)

// The gate records every Deploy and Promote it accepts or refuses as an
// events.k8s.io/v1 Event regarding the Environment's ArgoCD Application,
// so Argus and kubectl get events see a Deploy from its first moment. An
// Event lives in the namespace of the object it points at, so these are in
// argocd.

// The Event reasons.
const (
	ReasonDeployAccepted = "DeployAccepted"
	ReasonDeployRefused  = "DeployRefused"
)

// The annotations each Event carries for machines. Commit is set only when
// something was committed, and Refusal, the HTTP status, only on a refusal.
const (
	AnnotationApplication = "iidp.itema.no/application"
	AnnotationEnvironment = "iidp.itema.no/environment"
	AnnotationTag         = "iidp.itema.no/tag"
	AnnotationCommit      = "iidp.itema.no/commit"
	AnnotationKind        = "iidp.itema.no/kind"
	AnnotationRefusal     = "iidp.itema.no/refusal"
)

// ReportingController is the Events' reportingController.
const ReportingController = "iidp.itema.no/deploy-gate"

// eventTimeout bounds recording one Event. It runs after the answer is
// sent, so it never delays a Deploy.
const eventTimeout = 10 * time.Second

// noteLimit is the API server's limit on an Event's note, in bytes.
const noteLimit = 1024

// Event is an events.k8s.io/v1 Event, with the fields the gate sets.
type Event struct {
	APIVersion          string          `json:"apiVersion"`
	Kind                string          `json:"kind"`
	Metadata            EventMetadata   `json:"metadata"`
	EventTime           string          `json:"eventTime"`
	ReportingController string          `json:"reportingController"`
	ReportingInstance   string          `json:"reportingInstance"`
	Action              string          `json:"action"`
	Reason              string          `json:"reason"`
	Regarding           ObjectReference `json:"regarding"`
	Note                string          `json:"note"`
	Type                string          `json:"type"`
}

// EventMetadata is the part of an Event's metadata the gate sets.
type EventMetadata struct {
	Name        string            `json:"name"`
	Namespace   string            `json:"namespace"`
	Annotations map[string]string `json:"annotations"`
}

// ObjectReference is what an Event is about.
type ObjectReference struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Namespace  string `json:"namespace"`
	Name       string `json:"name"`
}

// EventSink records Events.
type EventSink interface {
	CreateEvent(ctx context.Context, event Event) error
}

// KubeEvents creates Events through the Kubernetes API.
type KubeEvents struct {
	Kube *platformstate.Kube
}

// CreateEvent implements EventSink.
func (k *KubeEvents) CreateEvent(ctx context.Context, event Event) error {
	body, err := json.Marshal(event)
	if err != nil {
		return err
	}
	path := "/apis/events.k8s.io/v1/namespaces/" + url.PathEscape(event.Metadata.Namespace) + "/events"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, k.Kube.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if k.Kube.Token != nil {
		token, err := k.Kube.Token()
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := k.Kube.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("creating an Event in %s: %w", event.Metadata.Namespace, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return &platformstate.StatusError{Path: path, Status: resp.StatusCode, Body: strings.TrimSpace(string(msg))}
	}
	return nil
}

// WaitForEvents waits until every Event the gate has started recording is
// recorded or has failed. The gate calls it on shutdown, after the last
// answer; tests call it before looking at their sink.
func (g *Gate) WaitForEvents() {
	g.events.Wait()
}

// recordAccepted records a Deploy or Promote the gate answered 200.
func (g *Gate) recordAccepted(claims oidc.Claims, req Request, res Response) {
	action := deployAction(claims)
	annotations := eventAnnotations(claims, req, res.Environment)
	note := fmt.Sprintf("%s %s %s %s accepted", action, req.Application, res.Environment, req.Tag)
	if res.Unchanged {
		note += fmt.Sprintf(": %s already runs it, so nothing was committed", res.Environment)
	} else {
		annotations[AnnotationCommit] = res.Commit
	}
	g.record(req.Application, res.Environment, action, ReasonDeployAccepted, "Normal", note, annotations)
}

// recordRefused records a refused Deploy or Promote, once the caller has
// proved to be the Application's own repository and the Environment is
// resolved; environment is "" before that. Earlier refusals have no
// Environment to point at, and the caller sees them in CI.
func (g *Gate) recordRefused(claims oidc.Claims, req Request, environment string, status int, err error) {
	if environment == "" || errors.Is(err, platformrepo.ErrApplicationMissing) || errors.Is(err, platformrepo.ErrEnvironmentMissing) {
		return
	}
	action := deployAction(claims)
	annotations := eventAnnotations(claims, req, environment)
	annotations[AnnotationRefusal] = strconv.Itoa(status)
	reason := strings.TrimPrefix(err.Error(), "refused: ")
	note := fmt.Sprintf("%s %s %s refused: %s", action, req.Application, environment, reason)
	g.record(req.Application, environment, action, ReasonDeployRefused, "Warning", note, annotations)
}

// record creates the Event in the background, so the answer is never held
// up by it. A failure is logged and changes nothing else.
func (g *Gate) record(application, environment, action, reason, eventType, note string, annotations map[string]string) {
	if g.Events == nil {
		return
	}
	now := time.Now().UTC()
	regarding := render.Environment{Application: application, Environment: environment}.Name()
	event := Event{
		APIVersion: "events.k8s.io/v1",
		Kind:       "Event",
		Metadata: EventMetadata{
			Name:        eventName(regarding, now),
			Namespace:   platformstate.ArgoCDNamespace,
			Annotations: annotations,
		},
		// metav1.MicroTime's format: exactly six fractional digits.
		EventTime:           now.Format("2006-01-02T15:04:05.000000Z07:00"),
		ReportingController: ReportingController,
		ReportingInstance:   reportingInstance(),
		Action:              action,
		Reason:              reason,
		Regarding: ObjectReference{
			APIVersion: "argoproj.io/v1alpha1",
			Kind:       "Application",
			Namespace:  platformstate.ArgoCDNamespace,
			Name:       regarding,
		},
		Note: truncate(note, noteLimit),
		Type: eventType,
	}
	g.events.Add(1)
	go func() {
		defer g.events.Done()
		ctx, cancel := context.WithTimeout(context.Background(), eventTimeout)
		defer cancel()
		if err := g.Events.CreateEvent(ctx, event); err != nil {
			g.log().Warn("the Deploy's Event was not recorded", "event", event.Metadata.Name, "reason", reason,
				"application", application, "environment", environment, "error", err.Error())
		}
	}()
}

// deployAction is Promote for a v* tag and Deploy for main.
func deployAction(claims oidc.Claims) string {
	if strings.HasPrefix(claims.Ref, TagRefPrefix) {
		return "Promote"
	}
	return "Deploy"
}

func eventAnnotations(claims oidc.Claims, req Request, environment string) map[string]string {
	return map[string]string{
		AnnotationApplication: req.Application,
		AnnotationEnvironment: environment,
		AnnotationTag:         req.Tag,
		AnnotationKind:        strings.ToLower(deployAction(claims)),
	}
}

// eventName is the shape client-go names Events with, plus a random
// suffix so two Deploys of the same Environment never collide.
func eventName(regarding string, now time.Time) string {
	suffix := make([]byte, 4)
	_, _ = rand.Read(suffix)
	return fmt.Sprintf("%s.%x%s", regarding, now.UnixNano(), hex.EncodeToString(suffix))
}

// reportingInstance is the pod's name, which is its hostname.
func reportingInstance() string {
	if name, err := os.Hostname(); err == nil && name != "" {
		return truncate(name, 128)
	}
	return "iidp-deploy-gate"
}

// truncate cuts s to at most n bytes, on a character boundary.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n - len("…")
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}
