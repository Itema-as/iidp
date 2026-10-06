// Package argus is Argus's server: it keeps the objects
// cmd/iidp-argus's informers hand it, interprets them with
// internal/platformstate into domain objects, and streams those to
// browsers over server-sent events. The server interprets and the browser
// only draws: the stream carries domain objects, never Kubernetes objects.
// It reads nothing itself and imports no Kubernetes client, so only
// cmd/iidp-argus links client-go.
//
// # The stream
//
// GET /events is a text/event-stream. Every message is one event, with a
// type and one line of JSON:
//
//	event: <type>
//	data: <JSON>
//
// A stream starts with "retry: 3000" and the snapshot, then sends each
// change as it happens, and a keepalive comment (": keepalive") every
// 15 s. There are no event ids and nothing is ever replayed: a browser
// that reconnects, or that the server closed for falling 256 messages
// behind, gets a fresh snapshot, and replaces everything it had with it.
// Message order is the order things happened in: a change's domain objects
// come before the notes it makes.
//
// The types, and their JSON:
//
//	snapshot             Snapshot: everything, on connect
//	application          platformstate.Application: one Application,
//	                     whole, whenever anything in it changes; replace
//	                     the one of that name
//	application-removed  {"name": "shop"}: its last Environment is gone
//	component            platformstate.Component: one Platform component,
//	                     whole; replace the one of that name
//	component-removed    {"name": "traefik"}
//	note                 Note: one new feed entry
//	cluster              ClusterState: Argus's connection to the cluster
//	                     changed
//
// # Snapshot
//
//	{
//	  "at": "2026-09-28T10:00:00Z",          the time of the snapshot
//	  "restartedAt": "2026-09-28T09:58:00Z", when this Argus started
//	  "ready": true,                         false until the first look
//	                                         at the cluster is complete
//	  "cluster": {"state": "connected"},     ClusterState, below
//	  "platform": Platform,                  where the card links out
//	                                         to, below
//	  "applications": [Application...],      by name
//	  "components": [Component...],          by name
//	  "feed": [Note...]                      oldest first
//	}
//
// # Application
//
// platformstate.Application, the same types iidp app status's --json
// uses for an Environment, with its Capabilities and Deploys added:
//
//	{
//	  "name": "shop",
//	  "environments": [                      prod, staging, then previews
//	    {                                    by name (pr-9 before pr-10)
//	      "name": "prod",
//	      "namespace": "shop-prod",
//	      "argocd": {"application": "shop-prod", "sync": "Synced",
//	                 "health": "Healthy", "operation": {"phase":
//	                 "Succeeded", "startedAt": ..., "finishedAt": ...}},
//	      "image": {"repository": "ghcr.io/itema-as/shop", "tag": "1.0.0"},
//	      "pods": {"ready": 1, "total": 1, "restarts": 0},
//	      "migration": {"result": "succeeded", "startedAt": ...,
//	                    "finishedAt": ...},
//	      "tasks": [{"name": "report", "schedule": "0 3 * * *",
//	                 "lastScheduleTime": ..., "lastRun": {...}}],
//	      "addresses": ["https://shop.app.itma.no"],
//	      "links": {"argocd": "https://argocd.../applications/argocd/shop-prod",
//	                "grafana": "https://itema.grafana.net/explore?..."},
//	      "condition": {"state": "Healthy"},
//	      "activity": null,
//	      "databaseAccess": {"readWrite": "push", "readOnly": "none",
//	                         "readWriteSetUp": true,
//	                         "readOnlySetUp": true},
//	      "capabilities": [Capability...],
//	      "deploys": [Deploy...]             oldest first
//	    }
//	  ]
//	}
//
// databaseAccess is who among the Application's developers may reach the
// database, read from the annotations the chart sets on the Environment's
// Cluster: each level the lowest permission on the Application repository
// that qualifies (pull, push, maintain or admin), or none. A level's
// setUp is false when it is not none but its role is not present among the
// Cluster's managed roles, because its password has not been written.
// Absent without Postgres, and for a Cluster from a chart older than
// database access.
//
// image, migration and a task's lastRun are null when there is none;
// argocd is never null here, since Argus knows an Environment only by its
// ArgoCD Application. links are iidp app status's (platformstate.LinksOf),
// from the Platform's argocdURL and grafanaURL; absent when neither is
// set. image.deployedAt, which iidp app status reads from the Platform
// repository, Argus cannot read there: it is when ArgoCD synced the
// Deploy that brought the tag (the history entry of the Deploy gate's
// commit, else the gate's acceptance), as far as this run of Argus has
// seen it, and absent otherwise (platform.go).
//
// condition is whether it serves:
//
//	{"state": "Healthy" | "Degraded" | "Unknown",
//	 "reason": "0 of 1 pods ready for over a minute"}  why, when not
//	                                                    Healthy
//
// activity is what is changing, or null when nothing is:
//
//	{"state": "Arriving" | "Unreleased" | "Deploying" | "Updating" |
//	          "Leaving",
//	 "stuck": false,
//	 "reason": "the migration failed",  why it is stuck
//	 "deploy": Deploy}                  the Deploy under way, for
//	                                    Deploying and a first Deploy's
//	                                    Arriving
//
// A Deploy (platformstate.Deploy) is one Deploy or Promote and where it
// is. Within its Environment it is identified by at and tag together, or
// by tag alone when at is absent:
//
//	{"tag": "1.0.1",
//	 "commit": "<Platform repository commit>",  absent without the gate's
//	                                            Event
//	 "promote": true,                   a Promote: staging's image to prod
//	 "preview": true,                   a Preview Environment's
//	 "at": "2026-09-28T09:59:00Z",      when the Deploy gate accepted or
//	                                    refused it; absent without its
//	                                    Event
//	 "hop": "Accepted" | "WaitingForArgoCD" | "Applying" | "RollingOut" |
//	        "Serving",                  absent when refused or superseded
//	 "stuck": true,                     it stopped at hop
//	 "reason": "...",                   why it is stuck, or why the gate
//	                                    refused it
//	 "refused": true,                   the gate refused it; never
//	                                    Deploying
//	 "supersededBy": "1.0.2"}           a later Deploy overtook it; not
//	                                    stuck
//
// Boolean fields are absent when false. A Deploy stays in the list while
// the gate's Event exists (an hour, the API server's default event TTL),
// so a served or superseded Deploy remains, with its end.
//
// A Capability (platformstate.Capability):
//
//	{"type": "postgres" | "itema-login" | "custom-domain" |
//	         "scheduled-task",
//	 "name": "shop-db",                 the CNPG Cluster, itema-login, the
//	                                    domain's host, the task's name
//	 "condition": {"state": "Healthy",
//	               "warning": "backups are failing: ..."},  the Warning
//	                                    flag: failing backups or WAL
//	                                    archiving, a certificate expiring
//	                                    within 14 days, a Scheduled task
//	                                    whose last run failed
//	 "activity": {"state": "Arriving" | "Leaving", "stuck": ...} | null}
//
// # Component
//
// platformstate.Component, one Platform component:
//
//	{"name": "argocd",
//	 "version": "v3.1.8",               absent when it cannot be told
//	 "condition": {"state": "Healthy" | "Degraded" | "Unknown", ...},
//	 "activity": {"state": "Updating", "stuck": ...} | null}
//
// The components are the ArgoCD Applications platform-components manages
// (argocd, cert-manager, deploy-gate, oauth2-proxy, argus and the rest),
// plus traefik and k3s, which ArgoCD does not manage and so never have an
// Activity. k3s is the node and whatever else runs in kube-system. The
// version is k3s's kubelet version, or the tag of the image named after
// the component, or the one tag all its images share.
//
// # Platform
//
// Where the browser's detail card links out to, from the bootstrap's
// values; each field is absent when not set:
//
//	{"argocdURL": "https://argocd.app.itma.no",
//	 "grafanaURL": "https://itema.grafana.net",
//	 "platformRepository": "https://github.com/Itema-as/iidp-platform",
//	 "bootstrapRepository": "https://github.com/Itema-as/iidp",
//	 "bootstrapRevision": "v1.2.3"}
//
// # Note
//
// One feed entry:
//
//	{"id": 12,                           increasing within one Argus
//	                                     run; the feed is in id order
//	 "at": "2026-09-28T10:00:00Z",
//	 "place": {"application": "shop", "environment": "prod"},
//	          or {"component": "argocd"}, or {} for the Platform as a
//	          whole; no application is the Platform
//	 "loudness": "loud" | "normal" | "quiet",
//	 "message": "shop prod is Degraded: 0 of 1 pods ready for over a
//	             minute",
//	 "feedOnly": true,                   it reports something finished,
//	                                     or from before Argus started:
//	                                     the camera does not visit it
//	 "seeded": true,                     read back when Argus started,
//	                                     not seen happening
//	 "seam": true}                       the one "since Argus restarted at
//	                                     hh:mm" entry
//
// A note about a database session, from the database tunnel's Events,
// says in the tunnel's own words who connected to which Environment's
// database as which role, when the session ended and why, or why it was
// refused.
//
// feed.go lists which changes are loud, normal and quiet. The feed keeps
// the last 200 notes or 24 hours of them, whichever is fewer; a browser
// that keeps its own feed from note messages should drop the same.
//
// # ClusterState
//
//	{"state": "connected"}
//	{"state": "interrupted", "since": "2026-09-28T10:00:00Z"}
//	{"state": "lost", "since": "2026-09-28T10:00:00Z"}
//
// interrupted is the API server not answering, for less than 30 s; lost is
// 30 s or more. While lost, every Condition is to be read as Unknown: the
// objects still sent are interpreted from the last the informers saw, kept
// so that the picture stays, but nothing new about the cluster reaches
// them until the state is connected again.
package argus
