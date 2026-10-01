package argus

import (
	"fmt"
	"strings"
	"time"

	"github.com/Itema-as/iidp/internal/platformstate"
)

// Every change Argus sees in the model is a note, with the loudness feed.go
// gives it. The rules below compare an object as
// it was with the object as it is now; nil is an object that was not
// there. An object seen for the first time when Argus starts gives no
// note: the snapshot shows it, and the seeded notes say what came before.

// applicationNotes are the notes for one Application's change.
func applicationNotes(prev, next *platformstate.Application, now time.Time) []Note {
	var out []Note
	name := ""
	byName := map[string]*platformstate.EnvironmentState{}
	if prev != nil {
		name = prev.Name
		for i := range prev.Environments {
			byName[prev.Environments[i].Name] = &prev.Environments[i]
		}
	}
	seen := map[string]bool{}
	if next != nil {
		name = next.Name
		for i := range next.Environments {
			env := &next.Environments[i]
			seen[env.Name] = true
			out = append(out, environmentNotes(name, byName[env.Name], env, now)...)
		}
	}
	if prev != nil {
		for _, env := range prev.Environments {
			if !seen[env.Name] {
				out = append(out, Note{At: now, Place: Place{Application: name, Environment: env.Name}, Loudness: Quiet, FeedOnly: true,
					Message: fmt.Sprintf("%s %s is gone", name, env.Name)})
			}
		}
	}
	return out
}

// environmentNotes are the notes for one Environment's change: its
// Condition, its Activity, its Deploys and its Capabilities.
func environmentNotes(app string, prev, next *platformstate.EnvironmentState, now time.Time) []Note {
	var out []Note
	place := Place{Application: app, Environment: next.Name}
	label := app + " " + next.Name
	preview := platformstate.IsPreview(next.Name)
	note := func(loudness string, feedOnly bool, format string, args ...any) {
		out = append(out, Note{At: now, Place: place, Loudness: loudness, FeedOnly: feedOnly, Message: fmt.Sprintf(format, args...)})
	}
	// A Preview Environment's arriving, leaving and deploying are quiet.
	unlessPreview := func(loudness string) string {
		if preview {
			return Quiet
		}
		return loudness
	}

	var pc, nc platformstate.Condition
	pc.State = platformstate.Healthy
	if prev != nil && prev.Condition != nil {
		pc = *prev.Condition
	}
	if next.Condition != nil {
		nc = *next.Condition
	}
	if nc.State != pc.State {
		switch nc.State {
		case platformstate.Degraded:
			note(Loud, false, "%s is Degraded: %s", label, nc.Reason)
		case platformstate.Unknown:
			note(Normal, false, "%s is Unknown: %s", label, nc.Reason)
		case platformstate.Healthy:
			note(Quiet, true, "%s is Healthy again", label)
		}
	}

	var pa *platformstate.Activity
	if prev != nil {
		pa = prev.Activity
	}
	na := next.Activity
	switch {
	case na != nil && na.Stuck && (pa == nil || !pa.Stuck || pa.State != na.State):
		note(Loud, false, "%s: %s", label, activityText(*na))
	case na != nil && (pa == nil || pa.State != na.State):
		switch na.State {
		case platformstate.Arriving:
			if pa != nil && pa.State == platformstate.Unreleased {
				note(unlessPreview(Normal), false, "%s is arriving: its first image is on its way", label)
			} else {
				note(unlessPreview(Normal), false, "%s is arriving", label)
			}
		case platformstate.Leaving:
			note(unlessPreview(Normal), false, "%s is leaving", label)
		case platformstate.Updating:
			note(Quiet, false, "%s is updating", label)
		case platformstate.Unreleased:
			note(Quiet, true, "%s is Unreleased: nothing was deployed in its first 30 minutes", label)
		}
	case na != nil && pa != nil && pa.Stuck && !na.Stuck:
		note(Quiet, true, "%s is no longer stuck", label)
	case na == nil && pa != nil:
		switch pa.State {
		case platformstate.Arriving:
			note(Quiet, true, "%s has arrived and is serving", label)
		case platformstate.Updating:
			note(Quiet, true, "%s has finished updating", label)
		}
	}

	out = append(out, deployNotes(label, place, preview, prev, next, now)...)
	out = append(out, capabilityNotes(label, place, prev, next, now)...)
	return out
}

// deployNotes are a Deploy's two entries: when the Deploy gate accepts
// (or refuses) it, and when it ends, serving or superseded. Stuck is the
// Environment's Activity turning stuck, above.
func deployNotes(label string, place Place, preview bool, prev, next *platformstate.EnvironmentState, now time.Time) []Note {
	var out []Note
	before := map[string]platformstate.Deploy{}
	if prev != nil {
		for _, d := range prev.Deploys {
			before[deployKey(d)] = d
		}
	}
	after := map[string]bool{}
	for _, d := range next.Deploys {
		key := deployKey(d)
		after[key] = true
		p, had := before[key]
		if !had {
			out = append(out, deployStarted(label, place, preview, d, now))
		}
		switch {
		case d.Hop == platformstate.HopServing && (!had || p.Hop != platformstate.HopServing):
			out = append(out, Note{At: now, Place: place, Loudness: Quiet, FeedOnly: true, Message: fmt.Sprintf("%s serves %s", label, d.Tag)})
		case d.SupersededBy != "" && (!had || p.SupersededBy == ""):
			out = append(out, Note{At: now, Place: place, Loudness: Quiet, FeedOnly: true,
				Message: fmt.Sprintf("%s %s to %s was superseded by %s", deployWord(d), d.Tag, label, d.SupersededBy)})
		}
	}
	// A Deploy found without the gate's Event is no longer listed once
	// it serves.
	if prev != nil {
		for _, d := range prev.Deploys {
			if d.At == nil && !d.Refused && !after[deployKey(d)] && d.Hop != platformstate.HopServing &&
				next.Image != nil && next.Image.Tag == d.Tag {
				out = append(out, Note{At: now, Place: place, Loudness: Quiet, FeedOnly: true, Message: fmt.Sprintf("%s serves %s", label, d.Tag)})
			}
		}
	}
	return out
}

// deployStarted is a Deploy's first note: accepted or refused by the
// Deploy gate, or, without the gate's Event, under way.
func deployStarted(label string, place Place, preview bool, d platformstate.Deploy, now time.Time) Note {
	n := Note{At: now, Place: place, Loudness: Quiet}
	if d.At != nil {
		n.At = *d.At
	}
	switch {
	case d.Refused:
		n.Message = fmt.Sprintf("%s %s to %s refused: %s", deployWord(d), d.Tag, label, d.Reason)
	case d.At != nil:
		n.Message = fmt.Sprintf("%s %s to %s accepted", deployWord(d), d.Tag, label)
		if d.Promote && !preview {
			n.Loudness = Normal
		}
	default:
		n.Message = fmt.Sprintf("%s %s to %s under way", deployWord(d), d.Tag, label)
	}
	return n
}

// deployKey identifies a Deploy across two looks at the cluster: the
// Deploy gate's time and the tag, or the tag alone for one found without
// the gate's Event.
func deployKey(d platformstate.Deploy) string {
	at := "-"
	if d.At != nil {
		at = d.At.UTC().Format(time.RFC3339Nano)
	}
	return at + " " + d.Tag + " " + fmt.Sprint(d.Refused)
}

func deployWord(d platformstate.Deploy) string {
	if d.Promote {
		return "Promote"
	}
	return "Deploy"
}

// hopWords are the hops in words.
var hopWords = map[string]string{
	platformstate.HopAccepted:         "accepted",
	platformstate.HopWaitingForArgoCD: "waiting for ArgoCD",
	platformstate.HopApplying:         "applying",
	platformstate.HopRollingOut:       "rolling out",
	platformstate.HopServing:          "serving",
}

// activityText is a stuck Activity in words, naming the Deploy and its
// hop when it is one.
func activityText(a platformstate.Activity) string {
	if d := a.Deploy; d != nil {
		return fmt.Sprintf("%s %s is stuck at %s: %s", deployWord(*d), d.Tag, hopWords[d.Hop], a.Reason)
	}
	return fmt.Sprintf("%s is stuck: %s", strings.ToLower(a.State), a.Reason)
}

// capabilityNotes are the notes for an Environment's Capabilities: their
// Condition, their Warning flag and their Activity turning stuck. Adding
// or removing one is the Environment's Updating.
func capabilityNotes(label string, place Place, prev, next *platformstate.EnvironmentState, now time.Time) []Note {
	var out []Note
	before := map[string]platformstate.Capability{}
	if prev != nil {
		for _, c := range prev.Capabilities {
			before[c.Type+"/"+c.Name] = c
		}
	}
	for _, c := range next.Capabilities {
		p, had := before[c.Type+"/"+c.Name]
		if !had {
			p = platformstate.Capability{Condition: platformstate.Condition{State: platformstate.Healthy}}
		}
		what := label + ": " + capabilityName(c)
		note := func(loudness string, feedOnly bool, message string) {
			out = append(out, Note{At: now, Place: place, Loudness: loudness, FeedOnly: feedOnly, Message: message})
		}
		if c.Condition.State != p.Condition.State {
			switch c.Condition.State {
			case platformstate.Degraded:
				note(Loud, false, what+" is Degraded: "+c.Condition.Reason)
			case platformstate.Unknown:
				note(Normal, false, what+" is Unknown: "+c.Condition.Reason)
			case platformstate.Healthy:
				note(Quiet, true, what+" is Healthy again")
			}
		}
		switch w := c.Condition.Warning; {
		case w != "" && w != p.Condition.Warning:
			note(Quiet, false, what+": "+w)
		case w == "" && p.Condition.Warning != "":
			note(Quiet, true, what+" no longer has a warning")
		}
		if a := c.Activity; a != nil && a.Stuck && (p.Activity == nil || !p.Activity.Stuck) {
			note(Loud, false, what+" is stuck: "+a.Reason)
		}
	}
	return out
}

// capabilityName is a Capability in words.
func capabilityName(c platformstate.Capability) string {
	switch c.Type {
	case platformstate.CapabilityPostgres:
		return "Postgres " + c.Name
	case platformstate.CapabilityItemaLogin:
		return "Itema login"
	case platformstate.CapabilityCustomDomain:
		return "custom domain " + c.Name
	case platformstate.CapabilityScheduledTask:
		return "Scheduled task " + c.Name
	}
	return c.Type + " " + c.Name
}

// componentNotes are the notes for one Platform component's change.
func componentNotes(name string, prev, next *platformstate.Component, now time.Time) []Note {
	place := Place{Component: name}
	var out []Note
	note := func(loudness string, feedOnly bool, format string, args ...any) {
		out = append(out, Note{At: now, Place: place, Loudness: loudness, FeedOnly: feedOnly, Message: fmt.Sprintf(format, args...)})
	}
	if next == nil {
		note(Quiet, true, "%s is gone", name)
		return out
	}
	pc := platformstate.Condition{State: platformstate.Healthy}
	var pa *platformstate.Activity
	if prev != nil {
		pc, pa = prev.Condition, prev.Activity
	}
	if next.Condition.State != pc.State {
		switch next.Condition.State {
		case platformstate.Degraded:
			note(Loud, false, "%s is Degraded: %s", name, next.Condition.Reason)
		case platformstate.Unknown:
			note(Normal, false, "%s is Unknown: %s", name, next.Condition.Reason)
		case platformstate.Healthy:
			note(Quiet, true, "%s is Healthy again", name)
		}
	}
	na := next.Activity
	switch {
	case na != nil && na.Stuck && (pa == nil || !pa.Stuck):
		note(Loud, false, "%s: %s", name, activityText(*na))
	case na != nil && pa == nil:
		note(Quiet, false, "%s is updating", name)
	case na != nil && pa != nil && pa.Stuck && !na.Stuck:
		note(Quiet, true, "%s is no longer stuck", name)
	case na == nil && pa != nil:
		note(Quiet, true, "%s has finished updating", name)
	}
	return out
}
