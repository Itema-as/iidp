package argus

import (
	"time"
	// The seam note says the restart's time on Oslo's clock, whether or
	// not the image has a zoneinfo database.
	_ "time/tzdata"
)

// The loudness of a note: how much the camera cares about it.
//
//	loud    Degraded; any Activity turning stuck; a Platform component
//	        Degraded
//	normal  Arriving (a first image into an Unreleased Environment
//	        included); Leaving; Unknown; a Promote
//	quiet   a Deploy; Updating; a Preview Environment arriving, leaving
//	        or deploying; a backup Warning; a Platform component
//	        Updating; a refused Deploy
//
// A note that only reports something finished is feed-only (Note.FeedOnly)
// whatever its loudness.
const (
	Loud   = "loud"
	Normal = "normal"
	Quiet  = "quiet"
)

// The feed keeps the last FeedSize notes or the last FeedAge of them,
// whichever is fewer.
const (
	FeedSize = 200
	FeedAge  = 24 * time.Hour
)

// Place is where a note happened: an Application (and one of its
// Environments), or the Platform (and one of its components). An empty
// Application is the Platform.
type Place struct {
	Application string `json:"application,omitempty"`
	Environment string `json:"environment,omitempty"`
	Component   string `json:"component,omitempty"`
}

// Note is one entry of the feed.
type Note struct {
	// ID orders the notes of one run of Argus: every note has a higher ID
	// than the one before it. The feed is in ID order, oldest first.
	ID int64 `json:"id"`
	// At is when it happened: when Argus saw the change, or, for a
	// Deploy, a Warning Event or a seeded note, the time the cluster
	// recorded.
	At       time.Time `json:"at"`
	Place    Place     `json:"place"`
	Loudness string    `json:"loudness"`
	Message  string    `json:"message"`
	// FeedOnly marks a note that only reports something finished (a
	// Deploy serving, a recovery) or that happened before Argus started:
	// it goes in the feed, and the camera does not visit it.
	FeedOnly bool `json:"feedOnly,omitempty"`
	// Seeded marks a note Argus read back when it started, from ArgoCD's
	// history and the last hour of Warning Events, rather than saw
	// happen.
	Seeded bool `json:"seeded,omitempty"`
	// Seam marks the one note that separates the seeded notes from the
	// ones Argus saw: "since Argus restarted at hh:mm".
	Seam bool `json:"seam,omitempty"`
}

// feed is the ring of recent notes, kept in memory only.
type feed struct {
	notes  []Note
	nextID int64
}

// add numbers n and keeps it, dropping what falls outside the limits.
func (f *feed) add(n Note, now time.Time) Note {
	f.nextID++
	n.ID = f.nextID
	f.notes = append(f.notes, n)
	f.trim(now)
	return n
}

func (f *feed) trim(now time.Time) {
	notes := f.notes
	if over := len(notes) - FeedSize; over > 0 {
		notes = notes[over:]
	}
	kept := make([]Note, 0, len(notes))
	for _, n := range notes {
		if now.Sub(n.At) <= FeedAge {
			kept = append(kept, n)
		}
	}
	f.notes = kept
}

// list is the feed as of now, oldest first.
func (f *feed) list(now time.Time) []Note {
	f.trim(now)
	return append([]Note{}, f.notes...)
}

// oslo is the Platform's clock, for the seam's hh:mm.
var oslo = func() *time.Location {
	if loc, err := time.LoadLocation("Europe/Oslo"); err == nil {
		return loc
	}
	return time.UTC
}()

func seamMessage(restartedAt time.Time) string {
	return "since Argus restarted at " + restartedAt.In(oslo).Format("15:04")
}
