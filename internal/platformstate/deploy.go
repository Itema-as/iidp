package platformstate

import (
	"sort"
	"strings"
	"time"
)

// A Deploy travels five hops, and Deploying starts at the first.
const (
	// HopAccepted: the Deploy gate recorded DeployAccepted, and ArgoCD
	// has not looked at the Platform repository since.
	HopAccepted = "Accepted"
	// HopWaitingForArgoCD: ArgoCD has looked since, but has not started
	// syncing the Deploy's commit.
	HopWaitingForArgoCD = "WaitingForArgoCD"
	// HopApplying: ArgoCD is syncing that commit, the migration included.
	HopApplying = "Applying"
	// HopRollingOut: the Deployment runs the new tag, and its pods are
	// starting.
	HopRollingOut = "RollingOut"
	// HopServing: that version is ready. Deploying ends here.
	HopServing = "Serving"
)

// The Deploy gate's Events: an events.k8s.io/v1 Event in argocd
// regarding the Environment's ArgoCD Application, for every Deploy or
// Promote it accepts or refuses, with these annotations.
const (
	ReasonDeployAccepted = "DeployAccepted"
	ReasonDeployRefused  = "DeployRefused"

	AnnotationApplication = "iidp.itema.no/application"
	AnnotationEnvironment = "iidp.itema.no/environment"
	AnnotationTag         = "iidp.itema.no/tag"
	// AnnotationCommit is the Platform repository commit; accepted only.
	AnnotationCommit = "iidp.itema.no/commit"
	// AnnotationKind is deploy or promote.
	AnnotationKind = "iidp.itema.no/kind"
	// AnnotationRefusal is the HTTP status of a refusal.
	AnnotationRefusal = "iidp.itema.no/refusal"

	kindPromote = "promote"
)

// The Database tunnel's Events: an events.k8s.io/v1 Event in argocd
// regarding the Environment's ArgoCD Application when a developer's session
// on its database starts, when it ends, and when one is refused, with
// AnnotationApplication, AnnotationEnvironment and these annotations.
const (
	ReasonDatabaseSessionStarted = "DatabaseSessionStarted"
	ReasonDatabaseSessionEnded   = "DatabaseSessionEnded"
	ReasonDatabaseSessionRefused = "DatabaseSessionRefused"

	// AnnotationLogin is the developer's GitHub login.
	AnnotationLogin = "iidp.itema.no/login"
	// AnnotationRole is the Postgres role, once one was chosen.
	AnnotationRole = "iidp.itema.no/role"
	// AnnotationDuration, AnnotationBytesFromClient and
	// AnnotationBytesToClient are a session's length and traffic; ended
	// only.
	AnnotationDuration        = "iidp.itema.no/duration"
	AnnotationBytesFromClient = "iidp.itema.no/bytes-from-client"
	AnnotationBytesToClient   = "iidp.itema.no/bytes-to-client"
)

// IsDatabaseSessionEvent reports whether an Event is the database
// tunnel's.
func IsDatabaseSessionEvent(e Event) bool {
	switch e.Reason {
	case ReasonDatabaseSessionStarted, ReasonDatabaseSessionEnded, ReasonDatabaseSessionRefused:
		return true
	}
	return false
}

// Deploy is one Deploy or Promote of an Environment, and where it is.
type Deploy struct {
	// Tag is the image tag it deploys.
	Tag string `json:"tag"`
	// Commit is the Platform repository commit that set the tag; absent
	// when the Deploy gate's Event was not seen, as for a Preview
	// Environment's Deploy.
	Commit string `json:"commit,omitempty"`
	// Promote is set for a Promote: staging's image moving on to prod.
	Promote bool `json:"promote,omitempty"`
	// Preview is set for a Preview Environment's Deploy, which never
	// passes the Deploy gate and so is seen from Applying on.
	Preview bool `json:"preview,omitempty"`
	// At is when the Deploy gate accepted or refused it; absent when its
	// Event was not seen.
	At *time.Time `json:"at,omitempty"`
	// Hop is Accepted, WaitingForArgoCD, Applying, RollingOut or
	// Serving; absent for a refused or superseded Deploy.
	Hop string `json:"hop,omitempty"`
	// Stuck is set when it stopped at Hop.
	Stuck bool `json:"stuck,omitempty"`
	// Reason says why it is stuck, or, for a refused Deploy, why the
	// Deploy gate refused it.
	Reason string `json:"reason,omitempty"`
	// Refused is set for a Deploy the Deploy gate refused. It never
	// becomes Deploying.
	Refused bool `json:"refused,omitempty"`
	// SupersededBy is the tag of the Deploy that overtook it before it
	// was serving. A superseded Deploy is not stuck.
	SupersededBy string `json:"supersededBy,omitempty"`
}

// deploysOf are an Environment's Deploys, oldest first: one for each of
// the Deploy gate's Events about it, and one more for a Deploy the
// cluster shows under way that no Event accounts for.
func deploysOf(name string, o Objects, f facts) []Deploy {
	preview := IsPreview(name)
	var deploys []Deploy
	var accepted []int
	for _, e := range gateEvents(o, name) {
		at := e.Time()
		d := Deploy{
			Tag:     e.Metadata.Annotations[AnnotationTag],
			Promote: e.Metadata.Annotations[AnnotationKind] == kindPromote,
			Preview: preview,
			At:      &at,
		}
		if e.Reason == ReasonDeployRefused {
			d.Refused, d.Reason = true, refusalReason(e)
		} else {
			d.Commit = e.Metadata.Annotations[AnnotationCommit]
			accepted = append(accepted, len(deploys))
		}
		deploys = append(deploys, d)
	}

	// An earlier Deploy either reached ArgoCD before the next one or was
	// skipped over: ArgoCD syncs the newest commit it finds.
	for i := 0; i+1 < len(accepted); i++ {
		d := &deploys[accepted[i]]
		if d.Commit != "" && commitSynced(f.app, d.Commit) {
			d.Hop = HopServing
		} else {
			d.SupersededBy = deploys[accepted[i+1]].Tag
		}
	}
	var newest *Deploy
	if len(accepted) > 0 {
		newest = &deploys[accepted[len(accepted)-1]]
		placeDeploy(newest, f)
	}

	if newest == nil || newest.Hop == HopServing || newest.SupersededBy != "" {
		if d, ok := untracedDeploy(f, preview); ok && (newest == nil || d.Tag != newest.Tag) {
			deploys = append(deploys, d)
		}
	}
	return deploys
}

// gateEvents are the Deploy gate's Events about the Environment name of
// o's ArgoCD Application, oldest first.
func gateEvents(o Objects, name string) []Event {
	var out []Event
	for _, e := range o.Events {
		if e.Reason != ReasonDeployAccepted && e.Reason != ReasonDeployRefused {
			continue
		}
		a := e.Metadata.Annotations
		regardsIt := e.Regarding.Name != "" && e.Regarding.Name == o.ArgoCD.Metadata.Name
		namesIt := a[AnnotationEnvironment] == name && a[AnnotationApplication] != "" && a[AnnotationApplication] == o.ArgoCD.Metadata.Labels[ApplicationLabel]
		if regardsIt || namesIt {
			out = append(out, e)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time().Before(out[j].Time()) })
	return out
}

// refusalReason is a refusal's reason in plain words: the Event's note
// after "refused: ", or the whole note.
func refusalReason(e Event) string {
	if _, reason, ok := strings.Cut(e.Note, "refused: "); ok {
		return reason
	}
	return e.Note
}

// placeDeploy works out the hop of the newest Deploy the Deploy gate
// accepted: by its Platform repository commit against ArgoCD's
// revisions, with its image tag as the fallback when ArgoCD has moved on
// to a later commit or no longer lists this one.
func placeDeploy(d *Deploy, f facts) {
	running := templateTag(f.workload)
	op := f.app.Status.OperationState
	onIt := d.Commit != "" && operationOn(f.app, d.Commit)
	switch {
	case onIt && operationRunning(f.app):
		d.Hop = HopApplying
		d.Reason = migrationStuckFor(f, d.Tag)
	case onIt && (op.Phase == "Failed" || op.Phase == "Error"):
		d.Hop = HopApplying
		if d.Reason = migrationStuckFor(f, d.Tag); d.Reason == "" {
			d.Reason = syncStuck(f.app)
		}
	case (d.Commit != "" && commitSynced(f.app, d.Commit)) || (running != "" && running == d.Tag):
		switch {
		case running != "" && running != d.Tag:
			// ArgoCD synced it, and the Environment has run another tag
			// since: a Deploy whose Event was not seen.
			d.SupersededBy = running
		case servingTag(f, d.Tag):
			d.Hop = HopServing
		default:
			d.Hop = HopRollingOut
			d.Reason = rolloutStuck(f, d.Tag)
		}
	case migrationFor(f, d.Tag):
		d.Hop = HopApplying
		d.Reason = migrationStuckFor(f, d.Tag)
	case argoCDLooked(f.app, d):
		d.Hop = HopWaitingForArgoCD
		if msg, ok := argoCDCondition(f.app, "ComparisonError"); ok {
			d.Reason = "ArgoCD cannot render the Environment: " + msg
		} else {
			d.Reason = outOfSyncStuck(f, *d.At)
		}
	default:
		d.Hop = HopAccepted
		if msg, ok := argoCDCondition(f.app, "ComparisonError"); ok {
			d.Reason = "ArgoCD cannot render the Environment: " + msg
		}
	}
	d.Stuck = d.Reason != ""
}

// untracedDeploy is a Deploy under way that no Event of the Deploy gate
// accounts for, found by its tag: a Preview Environment's, from the tag
// its ApplicationSet sets; otherwise a migration Job, or a Deployment,
// with a tag not yet serving.
func untracedDeploy(f facts, preview bool) (Deploy, bool) {
	running := templateTag(f.workload)
	if want := previewTag(f.app); preview && want != "" {
		d := Deploy{Tag: want, Preview: true}
		switch {
		case running == want && servingTag(f, want):
			return Deploy{}, false
		case running == want:
			d.Hop = HopRollingOut
			d.Reason = rolloutStuck(f, want)
		default:
			// It never passes the Deploy gate, so it is drawn from
			// Applying on, whether or not ArgoCD has started.
			d.Hop = HopApplying
			if d.Reason = migrationStuckFor(f, want); d.Reason == "" {
				d.Reason = syncStuck(f.app)
			}
			if d.Reason == "" {
				d.Reason = outOfSyncStuck(f, outOfSyncFrom(f))
			}
		}
		d.Stuck = d.Reason != ""
		return d, true
	}

	if job, ok := newestJob(f.migrations); ok {
		if tag := jobTag(job); tag != "" && tag != running && (!jobComplete(job) || operationRunning(f.app)) {
			d := Deploy{Tag: tag, Preview: preview, Hop: HopApplying, Reason: migrationStuck(f)}
			d.Stuck = d.Reason != ""
			return d, true
		}
	}
	if running == "" || servingTag(f, running) {
		return Deploy{}, false
	}
	other := false
	for _, pod := range f.pods {
		if t := podTag(pod); t != "" && t != running {
			other = true
		}
	}
	if !other && served(f) {
		// Every pod runs the tag already: a restart or a size change,
		// not a Deploy.
		return Deploy{}, false
	}
	d := Deploy{Tag: running, Preview: preview, Hop: HopRollingOut, Reason: rolloutStuck(f, running)}
	d.Stuck = d.Reason != ""
	return d, true
}

// previewTag is the image tag a Preview Environment's ApplicationSet
// sets in its Application's helm.valuesObject.
func previewTag(app *ArgoCDApplication) string {
	for _, s := range app.Spec.Sources {
		if s.Helm != nil && s.Helm.ValuesObject != nil && s.Helm.ValuesObject.Image.Tag != "" {
			return s.Helm.ValuesObject.Image.Tag
		}
	}
	return ""
}

// servingTag reports whether as many pods as the Deployment wants are
// ready and run tag.
func servingTag(f facts, tag string) bool {
	if f.workload == nil || templateTag(f.workload) != tag {
		return false
	}
	ready := 0
	for _, pod := range f.pods {
		if podReady(pod) && podTag(pod) == tag {
			ready++
		}
	}
	return ready >= f.desired
}

// migrationFor reports a migration Job for tag that has not succeeded,
// or that ran for a sync still going.
func migrationFor(f facts, tag string) bool {
	job, ok := newestJob(f.migrations)
	return ok && jobTag(job) == tag && (!jobComplete(job) || operationRunning(f.app))
}

// migrationStuckFor is why the newest migration Job makes a Deploy of tag
// stuck, as migrationStuck, when it ran tag (or any tag, if its image
// says none).
func migrationStuckFor(f facts, tag string) string {
	job, ok := newestJob(f.migrations)
	if !ok {
		return ""
	}
	if t := jobTag(job); t != "" && t != tag {
		return ""
	}
	return migrationStuck(f)
}

// operationOn reports whether ArgoCD's last sync operation is for
// commit.
func operationOn(app *ArgoCDApplication, commit string) bool {
	op := app.Status.OperationState
	if op == nil {
		return false
	}
	if s := op.Operation.Sync; s != nil && anyCommit(commit, s.Revision, s.Revisions) {
		return true
	}
	return op.SyncResult != nil && anyCommit(commit, op.SyncResult.Revision, op.SyncResult.Revisions)
}

// commitSynced reports whether ArgoCD has synced commit: it is in its
// history, its last sync of it succeeded, or it reports itself Synced to
// it.
func commitSynced(app *ArgoCDApplication, commit string) bool {
	for _, h := range app.Status.History {
		if anyCommit(commit, h.Revision, h.Revisions) {
			return true
		}
	}
	if op := app.Status.OperationState; op != nil && op.Phase == "Succeeded" && operationOn(app, commit) {
		return true
	}
	return app.Status.Sync.Status == "Synced" && anyCommit(commit, app.Status.Sync.Revision, app.Status.Sync.Revisions)
}

// argoCDLooked reports whether ArgoCD has compared the cluster with the
// Platform repository since d was accepted: it names d's commit as its
// target, or it reconciled after.
func argoCDLooked(app *ArgoCDApplication, d *Deploy) bool {
	if anyCommit(d.Commit, app.Status.Sync.Revision, app.Status.Sync.Revisions) {
		return true
	}
	return d.At != nil && app.Status.ReconciledAt != nil && !app.Status.ReconciledAt.Before(*d.At)
}

// anyCommit reports whether commit is revision or one of revisions. A
// short commit matches the full one it abbreviates.
func anyCommit(commit, revision string, revisions []string) bool {
	if commit == "" {
		return false
	}
	for _, r := range append([]string{revision}, revisions...) {
		if sameCommit(commit, r) {
			return true
		}
	}
	return false
}

func sameCommit(a, b string) bool {
	if len(a) < 7 || len(b) < 7 {
		return a != "" && a == b
	}
	if len(a) > len(b) {
		a, b = b, a
	}
	return strings.HasPrefix(b, a)
}
