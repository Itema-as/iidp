package dbtunnel

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Itema-as/iidp/internal/kubeevent"
	"github.com/Itema-as/iidp/internal/platformstate"
	"github.com/Itema-as/iidp/internal/render"
)

// Each session's start and end, and every refusal, is an events.k8s.io/v1
// Event regarding the Environment's ArgoCD Application, the way the Deploy
// gate records a Deploy, so Argus shows it in the Environment's activity,
// and a log line with the same fields, which is the record that lasts.
// Neither ever holds SQL, query results, a password or a token.

// ReportingController is the Events' reportingController.
const ReportingController = "iidp.itema.no/database-tunnel"

// eventTimeout bounds recording one Event, which never holds up a session.
const eventTimeout = 10 * time.Second

// eventAction is every session Event's action.
const eventAction = "Connect"

func (g Grant) logAttrs() []any {
	return []any{"login", g.Login, "application", g.Application, "environment", g.Environment, "role", g.Role, "access", g.Access}
}

func (g Grant) annotations() map[string]string {
	a := map[string]string{
		platformstate.AnnotationApplication: g.Application,
		platformstate.AnnotationEnvironment: g.Environment,
		platformstate.AnnotationLogin:       g.Login,
	}
	if g.Role != "" {
		a[platformstate.AnnotationRole] = g.Role
	}
	return a
}

func (t *Tunnel) started(g Grant) {
	t.log().Info("database session started", g.logAttrs()...)
	t.record(g, platformstate.ReasonDatabaseSessionStarted, "Normal",
		fmt.Sprintf("%s connected to %s %s's database as %s (%s)", g.Login, g.Application, g.Environment, g.Role, g.Access), g.annotations())
}

func (t *Tunnel) ended(g Grant, e Ended) {
	duration := e.Duration.Round(time.Second)
	t.log().Info("database session ended", append(g.logAttrs(), "duration", duration.String(), "reason", e.Reason,
		"bytes_from_client", e.FromClient, "bytes_to_client", e.ToClient)...)
	a := g.annotations()
	a[platformstate.AnnotationDuration] = duration.String()
	a[platformstate.AnnotationBytesFromClient] = strconv.FormatInt(e.FromClient, 10)
	a[platformstate.AnnotationBytesToClient] = strconv.FormatInt(e.ToClient, 10)
	t.record(g, platformstate.ReasonDatabaseSessionEnded, "Normal",
		fmt.Sprintf("%s's session on %s %s's database as %s ended after %s, as %s: %d bytes from the client, %d to it",
			g.Login, g.Application, g.Environment, g.Role, duration, e.Reason, e.FromClient, e.ToClient), a)
}

// refused logs a refusal, and records it once the developer's login and
// the Environment are known: before that there is nobody to name or no
// ArgoCD Application to record it on, and anyone on the internet could
// fill argocd with Events.
func (t *Tunnel) refused(g Grant, call string, err error) {
	t.log().Warn("database session refused", append(g.logAttrs(), "call", call, "error", err.Error())...)
	if g.Login == "" || !g.known {
		return
	}
	t.record(g, platformstate.ReasonDatabaseSessionRefused, "Warning",
		fmt.Sprintf("%s was refused %s %s's database: %s", g.Login, g.Application, g.Environment, strings.TrimPrefix(err.Error(), "refused: ")), g.annotations())
}

// record creates the Event in the background. A failure is logged and
// changes nothing else.
func (t *Tunnel) record(g Grant, reason, eventType, note string, annotations map[string]string) {
	if t.Events == nil {
		return
	}
	regarding := render.Environment{Application: g.Application, Environment: g.Environment}.Name()
	event := kubeevent.Environment(regarding, ReportingController, eventAction, reason, eventType, note, annotations, time.Now())
	t.events.Add(1)
	go func() {
		defer t.events.Done()
		ctx, cancel := context.WithTimeout(context.Background(), eventTimeout)
		defer cancel()
		if err := t.Events.CreateEvent(ctx, event); err != nil {
			t.log().Warn("a database session's Event was not recorded", append(g.logAttrs(), "event", event.Metadata.Name, "reason", reason, "error", err.Error())...)
		}
	}()
}
