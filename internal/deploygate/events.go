package deploygate

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Itema-as/iidp/internal/kubeevent"
	"github.com/Itema-as/iidp/internal/oidc"
	"github.com/Itema-as/iidp/internal/platformrepo"
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
	regarding := render.Environment{Application: application, Environment: environment}.Name()
	event := kubeevent.New(kubeevent.Spec{
		Regarding: regarding, Controller: ReportingController, Component: "iidp-deploy-gate",
		Action: action, Reason: reason, Type: eventType, Note: note, Annotations: annotations,
	}, time.Now())
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
