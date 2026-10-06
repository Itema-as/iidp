// Package kubeevent records events.k8s.io/v1 Events through the Kubernetes
// API with the standard library: the Deploy gate's Deploy Events and the
// database tunnel's session Events. Each regards an Environment's ArgoCD
// Application, and an Event lives in the namespace of the object it points
// at, so both are in argocd.
package kubeevent

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Itema-as/iidp/internal/platformstate"
)

// NoteLimit is the API server's limit on an Event's note, in bytes.
const NoteLimit = 1024

// Event is an events.k8s.io/v1 Event, with the fields iidp sets.
type Event struct {
	APIVersion          string          `json:"apiVersion"`
	Kind                string          `json:"kind"`
	Metadata            Metadata        `json:"metadata"`
	EventTime           string          `json:"eventTime"`
	ReportingController string          `json:"reportingController"`
	ReportingInstance   string          `json:"reportingInstance"`
	Action              string          `json:"action"`
	Reason              string          `json:"reason"`
	Regarding           ObjectReference `json:"regarding"`
	Note                string          `json:"note"`
	Type                string          `json:"type"`
}

// Metadata is the part of an Event's metadata iidp sets.
type Metadata struct {
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

// Sink records Events.
type Sink interface {
	CreateEvent(ctx context.Context, event Event) error
}

// Environment is an Event at now about the ArgoCD Application named
// regarding, in argocd, reported by controller from this pod. Its note is
// cut to NoteLimit.
func Environment(regarding, controller, action, reason, eventType, note string, annotations map[string]string, now time.Time) Event {
	now = now.UTC()
	return Event{
		APIVersion: "events.k8s.io/v1",
		Kind:       "Event",
		Metadata: Metadata{
			Name:        name(regarding, now),
			Namespace:   platformstate.ArgoCDNamespace,
			Annotations: annotations,
		},
		// metav1.MicroTime's format: exactly six fractional digits.
		EventTime:           now.Format("2006-01-02T15:04:05.000000Z07:00"),
		ReportingController: controller,
		ReportingInstance:   reportingInstance(controller),
		Action:              action,
		Reason:              reason,
		Regarding: ObjectReference{
			APIVersion: "argoproj.io/v1alpha1",
			Kind:       "Application",
			Namespace:  platformstate.ArgoCDNamespace,
			Name:       regarding,
		},
		Note: Truncate(note, NoteLimit),
		Type: eventType,
	}
}

// Kube creates Events through the Kubernetes API.
type Kube struct {
	Kube *platformstate.Kube
}

// CreateEvent implements Sink.
func (k *Kube) CreateEvent(ctx context.Context, event Event) error {
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

// name is the shape client-go names Events with, plus a random suffix so
// two Events about the same Environment never collide.
func name(regarding string, now time.Time) string {
	suffix := make([]byte, 4)
	_, _ = rand.Read(suffix)
	return fmt.Sprintf("%s.%x%s", regarding, now.UnixNano(), hex.EncodeToString(suffix))
}

// reportingInstance is the pod's name, which is its hostname, or else the
// controller's name without its iidp.itema.no/ prefix.
func reportingInstance(controller string) string {
	if name, err := os.Hostname(); err == nil && name != "" {
		return Truncate(name, 128)
	}
	return "iidp-" + strings.TrimPrefix(controller, "iidp.itema.no/")
}

// Truncate cuts s to at most n bytes, on a character boundary.
func Truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n - len("…")
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}
