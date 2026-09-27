package platformstate

import (
	"fmt"
	"strings"
	"time"
)

// Every Environment, Capability and Platform component has two
// independent layers (#100): a Condition, whether it is serving, and an
// Activity, whether something about it is changing. An Activity is either
// progressing or stuck. Degraded is about users; stuck is about a change
// not landing while the previous version still serves.
//
// Everything here is a pure function of the objects and the time now. It
// never reads the cluster, so the List path (iidp app status) and Argus's
// informers get the same answer from the same objects.

// The Conditions.
const (
	Healthy  = "Healthy"
	Degraded = "Degraded"
	Unknown  = "Unknown"
)

// The Activities.
const (
	// Arriving runs from the Environment's ArgoCD Application appearing
	// until its first image is serving.
	Arriving = "Arriving"
	// Unreleased is an Environment that has had no image for
	// UnreleasedAfter since it arrived.
	Unreleased = "Unreleased"
	// Deploying is a new image on its way, a Promote included.
	Deploying = "Deploying"
	// Updating is any other change to the desired state.
	Updating = "Updating"
	// Leaving runs from the deletion timestamp until the ArgoCD
	// Application is gone, the final backup included.
	Leaving = "Leaving"
)

// The timeouts of the rules.
const (
	// NotReadyGrace is how long an object may be short of ready pods
	// before it is Degraded.
	NotReadyGrace = 60 * time.Second
	// UnreleasedAfter is how long an Environment may be without an image
	// before Arriving becomes Unreleased.
	UnreleasedAfter = 30 * time.Minute
	// OutOfSyncStuckAfter is how long ArgoCD may be OutOfSync with no
	// sync started before the change is stuck.
	OutOfSyncStuckAfter = 5 * time.Minute
	// CertificateWarningWithin is how close a certificate's expiry puts
	// up the Warning flag.
	CertificateWarningWithin = 14 * 24 * time.Hour
	// WatchLostAfter is how long a broken watch may last before what it
	// watched is Unknown.
	WatchLostAfter = 30 * time.Second
)

// Condition is whether an object is serving.
type Condition struct {
	// State is Healthy, Degraded or Unknown.
	State string `json:"state"`
	// Reason says why it is Degraded or Unknown.
	Reason string `json:"reason,omitempty"`
	// Warning is set on a Capability that serves but is at risk: its
	// backups are failing, or its certificate expires soon. Never set on
	// an Environment or a Platform component.
	Warning string `json:"warning,omitempty"`
}

// Activity is what is changing about an object.
type Activity struct {
	// State is Arriving, Unreleased, Deploying, Updating or Leaving.
	State string `json:"state"`
	// Stuck is set when the change is not landing.
	Stuck bool `json:"stuck"`
	// Reason says why it is stuck.
	Reason string `json:"reason,omitempty"`
	// Deploy is the Deploy under way, for Deploying and for an Arriving
	// Environment's first Deploy.
	Deploy *Deploy `json:"deploy,omitempty"`
}

// WatchLost reports whether a watch broken since brokenSince has been
// broken long enough for what it watched to be Unknown. A nil
// brokenSince is a watch that is not broken.
func WatchLost(brokenSince *time.Time, now time.Time) bool {
	return brokenSince != nil && now.Sub(*brokenSince) >= WatchLostAfter
}

// IsPreview reports whether an Environment name is a Preview
// Environment's, pr-<number>.
func IsPreview(name string) bool {
	rest, ok := strings.CutPrefix(name, "pr-")
	if !ok || rest == "" {
		return false
	}
	for _, c := range rest {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// facts are what the rules read about one Environment or Platform
// component, gathered once.
type facts struct {
	app *ArgoCDApplication
	// workload is the Environment's Deployment; nil before the first
	// image.
	workload *Deployment
	// pods are the workload's pods that have not finished.
	pods    []Pod
	desired int
	// migrations and finalBackups are the chart's hook Jobs.
	migrations   []Job
	finalBackups []Job
	// outOfSyncSince is when the caller saw ArgoCD turn OutOfSync; nil
	// when it does not know.
	outOfSyncSince *time.Time
	now            time.Time
}

func factsOf(o Objects, now time.Time) facts {
	f := facts{app: &o.ArgoCD, outOfSyncSince: o.OutOfSyncSince, now: now}
	if d, ok := workload(o.Deployments); ok {
		f.workload = &d
		f.desired = desiredReplicas(d)
		f.pods = runningPods(o.Pods, d.Spec.Selector.MatchLabels)
	}
	for _, job := range o.Jobs {
		switch job.Metadata.Labels[ComponentLabel] {
		case ComponentMigration:
			f.migrations = append(f.migrations, job)
		case ComponentFinalBackup:
			f.finalBackups = append(f.finalBackups, job)
		}
	}
	return f
}

func desiredReplicas(d Deployment) int {
	if d.Spec.Replicas == nil {
		return 1
	}
	return *d.Spec.Replicas
}

// runningPods are the pods selector matches that have not finished.
func runningPods(pods []Pod, selector map[string]string) []Pod {
	var out []Pod
	if len(selector) == 0 {
		return out
	}
	for _, pod := range pods {
		if matches(pod.Metadata.Labels, selector) && pod.Status.Phase != "Succeeded" && pod.Status.Phase != "Failed" {
			out = append(out, pod)
		}
	}
	return out
}

func podReady(pod Pod) bool {
	c, ok := conditionOf(pod.Status.Conditions, "Ready")
	return ok && c.Status == conditionTrue
}

// podUnknown reports a pod whose state the kubelet no longer reports, as
// when its node is lost.
func podUnknown(pod Pod) bool {
	if pod.Status.Phase == "Unknown" {
		return true
	}
	c, ok := conditionOf(pod.Status.Conditions, "Ready")
	return ok && c.Status == conditionUnknown
}

// notReadySince is when pod stopped being ready, or was created if it
// never was.
func notReadySince(pod Pod) time.Time {
	if c, ok := conditionOf(pod.Status.Conditions, "Ready"); ok && c.LastTransitionTime != nil {
		return *c.LastTransitionTime
	}
	return pod.Metadata.CreationTimestamp
}

// failingAtOnce is why a pod that is not ready is Degraded without the
// grace: a container in CrashLoopBackOff, a container killed for memory
// that is not ready again, or a pod that cannot be scheduled.
func failingAtOnce(pod Pod) string {
	for _, c := range pod.Status.ContainerStatuses {
		if w := c.State.Waiting; w != nil && w.Reason == "CrashLoopBackOff" {
			return fmt.Sprintf("%s is in CrashLoopBackOff", pod.Metadata.Name)
		}
		if c.Ready {
			continue
		}
		for _, s := range []ContainerState{c.State, c.LastState} {
			if t := s.Terminated; t != nil && t.Reason == "OOMKilled" {
				return fmt.Sprintf("%s was OOMKilled", pod.Metadata.Name)
			}
		}
	}
	if c, ok := conditionOf(pod.Status.Conditions, "PodScheduled"); ok && c.Status == conditionFalse && c.Reason == "Unschedulable" {
		return fmt.Sprintf("%s is Unschedulable: %s", pod.Metadata.Name, c.Message)
	}
	return ""
}

// imagePullFailing is why a pod's image cannot be pulled, or "".
func imagePullFailing(pod Pod) string {
	for _, c := range pod.Status.ContainerStatuses {
		if w := c.State.Waiting; w != nil {
			switch w.Reason {
			case "ErrImagePull", "ImagePullBackOff", "InvalidImageName":
				image := c.Image
				for _, spec := range pod.Spec.Containers {
					if spec.Name == c.Name {
						image = spec.Image
					}
				}
				return fmt.Sprintf("the image %s cannot be pulled (%s)", image, w.Reason)
			}
		}
	}
	return ""
}

// served reports whether a workload has served at least once: its
// Deployment has completed a rollout, pods are serving now, or pods of an
// earlier version are still there while a new one rolls out.
func served(f facts) bool {
	if f.workload == nil {
		return false
	}
	if c, ok := conditionOf(f.workload.Status.Conditions, "Progressing"); ok && c.Reason == "NewReplicaSetAvailable" {
		return true
	}
	ready := 0
	tag := templateTag(f.workload)
	for _, pod := range f.pods {
		if podReady(pod) {
			ready++
		}
		if t := podTag(pod); t != "" && t != tag {
			return true
		}
	}
	return ready > 0 && ready >= f.desired
}

// podsCondition is the Condition of a set of pods that should have
// desired of them ready. Short of that, a pod failing at once makes it
// Degraded now, and otherwise it is Degraded once the shortfall is older
// than NotReadyGrace, unless graced is false (nothing has served yet, so
// no user is short of anything). A pod whose state is unknown makes
// it Unknown, unless it is Degraded.
func podsCondition(pods []Pod, desired int, shortSince time.Time, graced bool, now time.Time) Condition {
	ready := 0
	unknown := ""
	for _, pod := range pods {
		switch {
		case podReady(pod):
			ready++
		case podUnknown(pod) && unknown == "":
			unknown = fmt.Sprintf("the state of %s is unknown", pod.Metadata.Name)
		}
	}
	if ready < desired {
		for _, pod := range pods {
			if podReady(pod) {
				continue
			}
			if why := failingAtOnce(pod); why != "" {
				return Condition{State: Degraded, Reason: why}
			}
		}
		since := shortSince
		for _, pod := range pods {
			if !podReady(pod) && !podUnknown(pod) && notReadySince(pod).Before(since) {
				since = notReadySince(pod)
			}
		}
		if graced && unknown == "" && now.Sub(since) > NotReadyGrace {
			return Condition{State: Degraded, Reason: fmt.Sprintf("%d of %d pods ready for over %s", ready, desired, durationText(NotReadyGrace))}
		}
	}
	if unknown != "" {
		return Condition{State: Unknown, Reason: unknown}
	}
	return Condition{State: Healthy}
}

// workloadCondition is the Condition of an Environment's or a Platform
// component's own workload.
func workloadCondition(f facts) Condition {
	var c Condition
	switch {
	case f.workload == nil || f.desired == 0:
		c = Condition{State: Healthy}
	default:
		// The shortfall began no later than now; the pods say how much
		// earlier, and a Deployment with too few pods at all says it in
		// its Available condition.
		since := f.now
		if a, ok := conditionOf(f.workload.Status.Conditions, "Available"); ok && a.Status == conditionFalse && a.LastTransitionTime != nil {
			since = *a.LastTransitionTime
		}
		c = podsCondition(f.pods, f.desired, since, served(f), f.now)
	}
	if c.State == Healthy && f.app != nil && f.app.Status.Health.Status == Unknown {
		c = Condition{State: Unknown, Reason: "ArgoCD reports its health as Unknown"}
	}
	return c
}

func durationText(d time.Duration) string {
	if d%time.Minute == 0 {
		if d == time.Minute {
			return "a minute"
		}
		return fmt.Sprintf("%d minutes", int(d/time.Minute))
	}
	return d.String()
}

// operationRunning reports a sync ArgoCD is running now.
func operationRunning(app *ArgoCDApplication) bool {
	op := app.Status.OperationState
	return op != nil && (op.Phase == "Running" || op.Phase == "Terminating")
}

// argoCDCondition is ArgoCD's own condition of type t, if it has one.
func argoCDCondition(app *ArgoCDApplication, t string) (string, bool) {
	for _, c := range app.Status.Conditions {
		if c.Type == t {
			return c.Message, true
		}
	}
	return "", false
}

// rolloutInProgress reports a Deployment whose pods are not yet all of
// its current template.
func rolloutInProgress(d *Deployment, desired int) bool {
	if d == nil {
		return false
	}
	s := d.Status
	return s.ObservedGeneration < d.Metadata.Generation || s.UpdatedReplicas < desired || s.Replicas > s.UpdatedReplicas
}

func progressDeadlineExceeded(d *Deployment) bool {
	if d == nil {
		return false
	}
	c, ok := conditionOf(d.Status.Conditions, "Progressing")
	return ok && c.Status == conditionFalse && c.Reason == "ProgressDeadlineExceeded"
}

// newestJob is the newest of jobs by creation.
func newestJob(jobs []Job) (Job, bool) {
	if len(jobs) == 0 {
		return Job{}, false
	}
	newest := jobs[0]
	for _, job := range jobs[1:] {
		if job.Metadata.CreationTimestamp.After(newest.Metadata.CreationTimestamp) {
			newest = job
		}
	}
	return newest, true
}

func jobFailed(job Job) bool {
	c, ok := conditionOf(job.Status.Conditions, "Failed")
	return ok && c.Status == conditionTrue
}

func jobComplete(job Job) bool {
	c, ok := conditionOf(job.Status.Conditions, "Complete")
	return ok && c.Status == conditionTrue
}

// jobTag is the image tag a Job runs.
func jobTag(job Job) string {
	if len(job.Spec.Template.Spec.Containers) == 0 {
		return ""
	}
	_, tag := splitImage(job.Spec.Template.Spec.Containers[0].Image)
	return tag
}

// syncStuck is why ArgoCD's side of a change is stuck at once: the chart
// will not render, or the sync failed.
func syncStuck(app *ArgoCDApplication) string {
	if msg, ok := argoCDCondition(app, "ComparisonError"); ok {
		return "ArgoCD cannot render the Environment: " + msg
	}
	if op := app.Status.OperationState; op != nil && (op.Phase == "Failed" || op.Phase == "Error") {
		if op.Message != "" {
			return "ArgoCD's sync " + strings.ToLower(op.Phase) + ": " + op.Message
		}
		return "ArgoCD's sync " + strings.ToLower(op.Phase)
	}
	return ""
}

// migrationFailed is why the newest migration Job makes a change stuck.
func migrationFailed(f facts) string {
	if job, ok := newestJob(f.migrations); ok && jobFailed(job) {
		return "the migration failed"
	}
	return ""
}

// outOfSyncStuck is why ArgoCD being OutOfSync with no sync started makes
// a change stuck: for over OutOfSyncStuckAfter since since, or since the
// caller saw it turn OutOfSync if that is later.
func outOfSyncStuck(f facts, since time.Time) string {
	if f.app.Status.Sync.Status != "OutOfSync" || operationRunning(f.app) {
		return ""
	}
	if f.outOfSyncSince != nil && f.outOfSyncSince.After(since) {
		since = *f.outOfSyncSince
	}
	if f.now.Sub(since) > OutOfSyncStuckAfter {
		return fmt.Sprintf("OutOfSync for over %s with no sync started", durationText(OutOfSyncStuckAfter))
	}
	return ""
}

// outOfSyncFrom is the earliest ArgoCD can have been OutOfSync on the
// current change, as far as its Application says: the later of its
// creation and its last sync's end.
func outOfSyncFrom(f facts) time.Time {
	since := f.app.Metadata.CreationTimestamp
	if op := f.app.Status.OperationState; op != nil && op.FinishedAt != nil && op.FinishedAt.After(since) {
		since = *op.FinishedAt
	}
	return since
}

// rolloutStuck is why the workload's rollout is stuck: an image that
// cannot be pulled, or the progress deadline passed.
func rolloutStuck(f facts, tag string) string {
	for _, pod := range f.pods {
		if tag != "" && podTag(pod) != tag {
			continue
		}
		if why := imagePullFailing(pod); why != "" {
			return why
		}
	}
	if progressDeadlineExceeded(f.workload) {
		return "the rollout passed its progress deadline"
	}
	return ""
}

// podTag is the image tag a pod's first container runs.
func podTag(pod Pod) string {
	if len(pod.Spec.Containers) == 0 {
		return ""
	}
	_, tag := splitImage(pod.Spec.Containers[0].Image)
	return tag
}

// templateTag is the image tag a Deployment's pods are to run.
func templateTag(d *Deployment) string {
	if d == nil || len(d.Spec.Template.Spec.Containers) == 0 {
		return ""
	}
	_, tag := splitImage(d.Spec.Template.Spec.Containers[0].Image)
	return tag
}

// changeStuck is why a change that is not a Deploy is stuck; "" when it
// is not.
func changeStuck(f facts) string {
	for _, why := range []string{
		syncStuck(f.app),
		migrationFailed(f),
		rolloutStuck(f, ""),
		outOfSyncStuck(f, outOfSyncFrom(f)),
	} {
		if why != "" {
			return why
		}
	}
	return ""
}

// changing reports whether ArgoCD or the workload shows a change to the
// desired state under way.
func changing(f facts) bool {
	return f.app.Status.Sync.Status == "OutOfSync" || operationRunning(f.app) || rolloutInProgress(f.workload, f.desired) || progressDeadlineExceeded(f.workload)
}

// environmentState is an Environment's Condition and Activity, and its
// Deploys.
func environmentState(name string, o Objects, now time.Time) (Condition, *Activity, []Deploy) {
	f := factsOf(o, now)
	condition := workloadCondition(f)
	deploys := deploysOf(name, o, f)

	if o.ArgoCD.Metadata.DeletionTimestamp != nil {
		a := &Activity{State: Leaving}
		if msg, ok := argoCDCondition(f.app, "DeletionError"); ok {
			a.Stuck, a.Reason = true, "the deletion is blocked: "+msg
		} else if job, ok := newestJob(f.finalBackups); ok && jobFailed(job) {
			a.Stuck, a.Reason = true, "the final backup failed"
		}
		return condition, a, deploys
	}

	// The Deploy under way is the newest one the Deploy gate did not
	// refuse, until it serves or is overtaken.
	var current *Deploy
	for i := len(deploys) - 1; i >= 0; i-- {
		if d := deploys[i]; !d.Refused {
			if d.SupersededBy == "" && d.Hop != HopServing {
				current = &d
			}
			break
		}
	}
	// A Deploy is stuck by the rules of the hop it is at; any other
	// change by all of them.
	stuck := ""
	if current == nil {
		stuck = changeStuck(f)
	}

	var a *Activity
	switch {
	case !served(f) && (current != nil || f.workload != nil || now.Sub(o.ArgoCD.Metadata.CreationTimestamp) < UnreleasedAfter):
		a = &Activity{State: Arriving}
	case !served(f):
		a = &Activity{State: Unreleased}
	case current != nil:
		a = &Activity{State: Deploying}
	case changing(f) || stuck != "":
		a = &Activity{State: Updating}
	default:
		return condition, nil, deploys
	}
	if current != nil {
		a.Deploy = current
		a.Stuck, a.Reason = current.Stuck, current.Reason
	} else if stuck != "" {
		a.Stuck, a.Reason = true, stuck
	}
	return condition, a, deploys
}
