package argus

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/Itema-as/iidp/internal/platformstate"
)

// Seed ends the first look at the cluster: it interprets what the
// informers have listed, without notes (none of it is news), then fills
// the feed with what happened before Argus started, from what the cluster
// still says of it, and marks the seam. From then on every change is a
// note. It is called once, after the informers have synced or given up
// waiting.
//
// What it seeds, each within the feed's 24 hours, oldest first and all
// feed-only:
//
//   - each ArgoCD Application's status.history (a sync of a Platform
//     repository commit) and its operationState, when the last sync
//     failed or is running;
//   - each Deploy the Deploy gate's Events still show, accepted or
//     refused;
//   - each session the database tunnel's Events still show, its start,
//     its end or its refusal;
//   - the Warning Events of the last hour, except the gate's refusals,
//     which are the Deploys above, and the tunnel's.
func (s *Store) Seed() {
	s.Recompute()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seeded {
		return
	}
	now := s.now()
	argoCD := s.argoCDLocked()
	var notes []Note
	for _, app := range argoCD {
		notes = append(notes, argoCDHistory(app)...)
	}
	for _, name := range sortedKeys(s.apps, nil) {
		app := s.apps[name]
		for _, env := range app.Environments {
			place := Place{Application: name, Environment: env.Name}
			for _, d := range env.Deploys {
				if d.At != nil {
					notes = append(notes, deployStarted(name+" "+env.Name, place, platformstate.IsPreview(env.Name), d, now))
				}
			}
		}
	}
	for _, byKey := range s.objects {
		for _, obj := range byKey {
			e, ok := obj.(platformstate.Event)
			if !ok {
				continue
			}
			if n, warning, ok := eventFeedNote(e, argoCD); ok && (!warning || now.Sub(e.Time()) <= seedWarningsFor) {
				notes = append(notes, n)
			}
		}
	}
	sort.SliceStable(notes, func(i, j int) bool {
		if !notes[i].At.Equal(notes[j].At) {
			return notes[i].At.Before(notes[j].At)
		}
		return notes[i].Message < notes[j].Message
	})
	for i := range notes {
		notes[i].Seeded, notes[i].FeedOnly = true, true
	}
	notes = append(notes, Note{At: s.restartedAt, Loudness: Quiet, FeedOnly: true, Seam: true, Message: seamMessage(s.restartedAt)})
	s.seeded = true
	s.publishNotes(notes, now)
}

// argoCDHistory are the notes an ArgoCD Application's status still
// holds: a note per sync in its history, and its last operation when it
// failed or is running.
func argoCDHistory(app platformstate.ArgoCDApplication) []Note {
	name := app.Metadata.Labels[platformstate.ApplicationLabel]
	place, label := Place{Component: app.Metadata.Name}, app.Metadata.Name
	if name != "" {
		env := platformstate.EnvironmentName(name, app)
		place, label = Place{Application: name, Environment: env}, name+" "+env
	} else if !isComponent(app) {
		return nil
	}
	var out []Note
	for _, h := range app.Status.History {
		if h.DeployedAt == nil {
			continue
		}
		out = append(out, Note{At: *h.DeployedAt, Place: place, Loudness: Quiet,
			Message: fmt.Sprintf("%s synced %s", label, revisionText(h.Revision, h.Revisions))})
	}
	if op := app.Status.OperationState; op != nil {
		switch {
		case (op.Phase == "Failed" || op.Phase == "Error") && op.FinishedAt != nil:
			message := fmt.Sprintf("%s: ArgoCD's sync %s", label, strings.ToLower(op.Phase))
			if op.Message != "" {
				message += ": " + op.Message
			}
			out = append(out, Note{At: *op.FinishedAt, Place: place, Loudness: Loud, Message: message})
		case (op.Phase == "Running" || op.Phase == "Terminating") && op.StartedAt != nil:
			out = append(out, Note{At: *op.StartedAt, Place: place, Loudness: Quiet, Message: label + ": ArgoCD started a sync"})
		}
	}
	return out
}

var commitSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

// revisionText is what a sync synced: the Platform repository's commit,
// shortened, where a multi-source Application lists it next to its
// chart's version; otherwise the revision as ArgoCD gives it.
func revisionText(revision string, revisions []string) string {
	all := append([]string{revision}, revisions...)
	for _, r := range all {
		if commitSHA.MatchString(r) {
			return "commit " + r[:7]
		}
	}
	for _, r := range all {
		if r != "" {
			return r
		}
	}
	return "a revision ArgoCD did not name"
}
