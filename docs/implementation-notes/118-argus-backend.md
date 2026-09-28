# #118 Argus's backend: `iidp-argus`, its stream, and its bootstrap component

This note records the decisions taken while building Argus's server: the binary `cmd/iidp-argus`, the package `internal/argus`, the component `bootstrap/components/argus`, and two additions to `internal/platformstate`. The design is the spec's ([#115](https://github.com/Itema-as/iidp/issues/115)), from the backend decision ([#101](https://github.com/Itema-as/iidp/issues/101)), the state-sources decision ([#98](https://github.com/Itema-as/iidp/issues/98)) and the Deploy-tracing decision ([#106](https://github.com/Itema-as/iidp/issues/106), with its correction that the gate's Events live in `argocd`). This note names each place where those decisions had to be read more precisely, and why. The stream's message shapes are below and in `internal/argus/doc.go`; the drawing ([#119](https://github.com/Itema-as/iidp/issues/119)) builds on them. The RBAC and the values are in [`bootstrap/README.md`](../../bootstrap/README.md#argus).

Sources, checked on 2026-09-28:

- client-go **v0.36.4** (the node's k3s is v1.36.4), read in the module cache: `dynamic/dynamicinformer/informer.go` (how a dynamic informer is built, and that the package imports `k8s.io/client-go/informers`), `tools/cache/delta_fifo.go` (`TransformFunc`: called before anything else sees the object, safe to mutate, should be idempotent), `tools/cache/shared_informer.go` (`SetTransform`, `SetWatchErrorHandlerWithContext`, `RunWithContext`), `features/known_features.go` (`WatchListClient` on by default since 1.35), and `dynamic/fake/simple.go` (the fake filters label selectors on list, and ignores field selectors).
- Kubernetes (Context7, `kubernetes/kubernetes`): `pkg/registry/core/event/strategy.go`, whose selectable fields for Events include `type`, `reason` and `involvedObject.*`.
- ArgoCD's resource tracking id, `<application>:<group>/<kind>:<namespace>/<name>`, and that annotation tracking is 3.x's default, as [95-preview-environments.md](95-preview-environments.md) records from `util/argo/resource_tracking.go`.

## Two packages: the server without client-go, the binary with it

- **`internal/argus`** holds everything that is not reading the cluster: the store of cut-down objects, the model built from them with `platformstate`, the feed, the notes and their loudness, the seeding, and the HTTP server with the stream. It imports no Kubernetes module, so it is tested with plain structs and `httptest`.
- **`cmd/iidp-argus`** holds the informers, their transforms, the probe and `main`. It is the only package that imports `k8s.io/*`.
- `TestOnlyArgusLinksClientGo` runs `go list -deps` and fails if the CLI, the Deploy gate or `internal/argus` link anything from `k8s.io` or `sigs.k8s.io`. It also fails if `cmd/iidp-argus` links a typed clientset, typed informers, `k8s.io/api`, or ArgoCD's or CNPG's modules.

## Informers without `dynamicinformer`

`dynamicinformer.NewFilteredDynamicInformer` returns an `informers.GenericInformer`, and for that one interface its package imports `k8s.io/client-go/informers`. That brings in every typed informer, lister and clientset. With it, the binary was 26.4 MiB and linked 393 `k8s.io` packages. `newInformer` builds the same thing that function builds, written out: a `cache.ListWatch` over the dynamic client's `List` and `Watch`, wrapped in `cache.ToListWatcherWithWatchListSemantics`, and a `SharedIndexInformer`. Without the typed packages the binary is **12.1 MiB** (linux/amd64, `-s -w`, as GoReleaser builds it), against the gate's 8.2 MiB, and links 87 `k8s.io` packages, none of them typed clients.

### What each informer reads

| Informer | Resource | Narrowed by |
|---|---|---|
| `applications` | `applications.argoproj.io` | namespace `argocd` |
| `deployments`, `pods` | `deployments`, `pods` | the label `iidp.itema.no/application`, and again in the namespaces `argocd` and `kube-system` (two more informers each) |
| `jobs`, `cronjobs`, `clusters`, `ingresses` | `jobs`, `cronjobs`, `clusters.postgresql.cnpg.io`, `ingresses` | the label `iidp.itema.no/application` |
| `certificates` | `certificates.cert-manager.io` | nothing (below) |
| `events-argocd` | core v1 `events` | namespace `argocd` |
| `events-warnings` | core v1 `events` | the field selector `type=Warning,metadata.namespace!=argocd` |
| `nodes` | `nodes` | nothing: there is one |

Three points go beyond the design:

- **Certificates are read unnarrowed.** cert-manager's ingress-shim makes a custom domain's Certificate from the chart's Ingress. Whether it copies the Ingress's labels onto the Certificate was not confirmed from a source. Every Platform has only a handful of Certificates, so reading them all costs nothing, and the model matches each one to its Environment by namespace. This does not depend on the labels either way.
- **Events are read through core v1.** The Deploy gate creates `events.k8s.io/v1` Events, which are the same objects the API server serves as core v1 `events`. Core v1's field selector knows `type` (`strategy.go`, above). Whether `events.k8s.io/v1` converts that field label was not confirmed. A cluster-wide watch of every Event, Normal ones included, would carry every pod's lifecycle. The transform turns the core v1 names back into `platformstate.Event`'s: `message` becomes `note`, `involvedObject` becomes `regarding`, and `lastTimestamp` becomes `deprecatedLastTimestamp`. The ClusterRole's `events` is therefore core v1's, the `""` group.
- **Platform components are judged by their pods only in `argocd` and `kube-system`**, as #101 narrowed it. For the components whose workloads live elsewhere (cert-manager, external-dns, CNPG, monitoring, oauth2-proxy, and Argus itself in `argus`), `platformstate` now falls back to ArgoCD's app-level health (below).

Each informer's handlers put what it keeps into the store under its own name, and the model counts an object two informers both hold only once. The fake client, which ignores field selectors, gives exactly that case. Informers that cannot list (a CRD not installed yet, a missing permission) log the error each time client-go retries. After a minute `main` stops waiting for them and shows the rest, so one missing CRD cannot keep the whole view from starting.

### The transforms

Every informer has a `TransformFunc` that decodes the unstructured object into `platformstate`'s cut-down struct, with the converter in `apimachinery/pkg/runtime`. It keeps only that struct, in a `kept` holding the name, namespace, uid and resourceVersion the cache keys by. It also drops every annotation `platformstate` does not read, keeping only `iidp.itema.no/*`, ArgoCD's tracking id and Traefik's middlewares. That matters because an object's annotations can hold a `last-applied-configuration` as large as the object itself. The transform is idempotent, and it transforms a tombstone's object too. An object that does not decode is logged and kept empty, so one odd object cannot stop its informer. `TestTransformsKeepOnlyWhatTheModelReads` feeds one realistic object of each kind, with managedFields, a full pod spec and fat status, and checks what is kept and what is dropped.

## The model

The store holds the cut-down objects. After every change, once 100 ms have passed without another, and every 5 s for the rules that wait, it builds the whole model again. It then sends each Application or component whose JSON changed.

- **An Environment** is an ArgoCD Application labelled `iidp.itema.no/application`. Its objects are what its destination namespace holds, since the CLI gives every Environment a namespace of its own, and so the model needs no labels to place them. Its Events are the Deploy gate's, matched by `regarding` or by their application and environment annotations, as `platformstate` matches them.
- **OutOfSync time.** ArgoCD does not record when an Application turned OutOfSync. The store records when its watch saw one turn, and passes that time as `OutOfSyncSince`. An Application already OutOfSync when Argus starts has no such time, and `platformstate` counts from its last sync, as #116 decided.
- **Platform components** are the ArgoCD Applications that `platform-components` manages, by their tracking annotation (or the instance label, under label tracking). The root Applications `platform`, `platform-components`, `platform-secrets` and `applications` are therefore not components. A Deployment in `argocd` or `kube-system` belongs to the component its own tracking annotation names. The application controller's StatefulSet pods, which no Deployment selects, go to `argocd` by their `part-of` label. Two components ArgoCD does not manage have a Condition only:
  - `traefik`: the Deployment `kube-system/traefik` and k3s's ServiceLB pods for it;
  - `k3s`: the node and everything else in `kube-system` (CoreDNS, metrics-server, the local-path provisioner).
- **An Application** has no Condition or Activity of its own, as #116 left it.

## Additions to `platformstate`

Both are in `capability.go`, with table tests, so `iidp app status` shares them:

- **Itema login is a Capability** (`itema-login`) when an Ingress of the Environment names the shared middleware `oauth2-proxy-itema-login-auth@kubernetescrd`, or an Environment's own copy for sign-in groups, `<namespace>-<fullname>-itema-login@kubernetescrd`, in Traefik's middlewares annotation. That is the chart's `application.login.annotation`. There is one such Capability per Environment, whichever Ingresses carry the annotation. It is always Healthy, because it has no state of its own: oauth2-proxy is a Platform component. It is Leaving once every Ingress that carries it is being deleted. Removing login from an Environment is that Environment's Updating, as for any Capability.
- **A Scheduled task whose last run failed has the Warning flag**, "the last run failed at …". The last run is the CronJob's newest Job, the same one `tasks[].lastRun` shows. It is never Degraded, and the Environment's Condition and Activity do not change. A later run that succeeds, or one still running, clears the flag.

Two more were needed for Argus's Platform components:

- `ComponentObjects` gains `Nodes`, judged by their Ready condition, with the same 60 s grace as any Ready condition: `Unknown` once the kubelet stops reporting, Degraded after a minute not ready.
- **ArgoCD's health for a component whose workload is not in view.** When a component has no Deployments, pods or nodes at all, ArgoCD's app-level health `Degraded` makes it Degraded. Without this, cert-manager or oauth2-proxy could never be anything but Healthy in Argus, because their pods are outside the namespaces #101 narrowed the informers to. Where the pods are in view, they decide, as before. This is a new rule; see the open questions.

**`iidp app status` is unchanged.** Its JSON is `platformstate.Status`, whose Environments carry no Capabilities, and nothing in it was renamed or removed. The Deploy gate lists no Ingresses, so its Environments' `Objects` have none. Its `Tasks:` line already says a run "failed". If `iidp app status` ever shows Capabilities, it gets both rules from `CapabilitiesOf`, given `list` on `ingresses` for the gate.

## The stream

`GET /events` is server-sent events. The full shapes are in `internal/argus/doc.go`, which is the reference for #119. In short:

| Event | Data |
|---|---|
| `snapshot` | `{"at", "restartedAt", "ready", "cluster", "applications": [...], "components": [...], "feed": [...]}`, once, first |
| `application` | one `platformstate.Application`, whole: `{"name", "environments": [...]}`, each Environment the `iidp app status` shape plus `"capabilities"` and `"deploys"`; replaces the one of that name |
| `application-removed` | `{"name"}` |
| `component` | one `platformstate.Component`: `{"name", "condition", "activity"}` |
| `component-removed` | `{"name"}` |
| `note` | one feed entry: `{"id", "at", "place": {"application", "environment"} or {"component"} or {}, "loudness": "loud"/"normal"/"quiet", "message", "feedOnly", "seeded", "seam"}` |
| `cluster` | `{"state": "connected"/"interrupted"/"lost", "since"}` |

- **Environment.** `condition` is `{"state": "Healthy"/"Degraded"/"Unknown", "reason"}`. `activity` is `{"state": "Arriving"/"Unreleased"/"Deploying"/"Updating"/"Leaving", "stuck", "reason", "deploy"}`, or null. Each Capability is `{"type": "postgres"/"itema-login"/"custom-domain"/"scheduled-task", "name", "condition": {"state", "reason", "warning"}, "activity"}`.
- **Deploy.** `{"tag", "commit", "promote", "preview", "at", "hop": "Accepted"/"WaitingForArgoCD"/"Applying"/"RollingOut"/"Serving", "stuck", "reason", "refused", "supersededBy"}`, with booleans absent when false. Within its Environment, a Deploy is identified by `at` and `tag`, or by `tag` alone when `at` is absent.
- **Argus has no links.** `image.deployedAt` and `links`, which the Deploy gate reads from the Platform repository, are absent here.
- **Why the shapes are `platformstate`'s own.** Argus and `iidp app status` then never disagree, and the frontend reads the same field names the CLI's `--json` documents. Everything a browser gets is a domain object; no Kubernetes object crosses the stream.

**Whole Applications, not patches.** A change sends the whole Application it is in. Applications are small, a few kilobytes, and a whole object means the browser never merges anything: it replaces the Application by name. A Deploy is the "one more domain object" of #106 inside its Environment, in `deploys` and, while under way, in `activity.deploy`.

**Snapshot, then deltas, with no replay.** Subscribing takes the snapshot and registers the browser under one lock, so no change falls between them or is sent twice. Frames carry no `id:`, so a reconnect sends no `Last-Event-ID`; it gets a fresh snapshot, and `retry: 3000` sets the reconnect delay. A browser that falls 256 messages behind is closed, rather than holding the store up or dropping messages silently: it reconnects to a fresh snapshot. A keepalive comment goes out every 15 s. Requests end with the process's context, so a SIGTERM closes every stream at once, and the browsers reconnect to the next Argus.

## The feed and its loudness

A note is made from a change in the model: the domain object as it was against the object as it is. The rules, with the spec's table (#103 with #106's additions):

| Loudness | Notes |
|---|---|
| loud | an Environment, a Capability or a component turning Degraded; any Activity turning stuck (a Deploy's names its hop, "Deploy 1.0.1 is stuck at applying: the migration failed") |
| normal | an Environment arriving, including a first image into an Unreleased Environment; leaving; Unknown; a Promote accepted; Argus losing the cluster |
| quiet | a Deploy accepted, or found under way without the gate's Event; a refused Deploy with its reason; updating; a Preview Environment arriving, leaving or deploying; a Capability's Warning flag going up (backups, and the certificate and Scheduled task Warnings, which the table does not list, with them); a component updating; a Warning Event |
| feed-only | a Deploy serving or superseded; Healthy again; arrived; finished updating; no longer stuck; a Warning cleared; an Environment or component gone; Unreleased; Argus seeing the cluster again; every seeded note |

- **Two notes per Deploy**, as #106 put it: the gate's acceptance (or refusal), and its end, serving or superseded. A Deploy stuck at a hop is the loud stuck note of its Environment's Activity, not a third note. A Deploy found without the gate's Event, such as a Preview Environment's, is noted when it appears and when it serves.
- **A new Warning Event**, other than the gate's refusals (which are the Deploy's own note), is a quiet, feed-only note placed at its Environment, by namespace, at a component, or at the Platform. Warning Events are not in the table. They are what #101 seeds the feed with, so they are in the live feed as well, but they never move the camera: a Degraded Environment already has its own loud note.
- **Nothing is noted for what Argus finds when it starts**: the snapshot shows it, and the seeded notes say what came before.

**The ring buffer** keeps the last 200 notes or the last 24 hours of them, whichever is fewer. It trims on every note and on every read.

**Seeding and the seam.** Once the informers have synced, or have been waited on for a minute, `Seed` interprets what is there without notes. It then adds, oldest first, every seeded note within 24 hours, each feed-only:

- each ArgoCD Application's `status.history` ("shop prod synced commit 1111111", a component's by its revision);
- its `operationState`, when the last sync failed (loud, "ArgoCD's sync failed: …") or is running;
- each Deploy the gate's Events still show, accepted or refused;
- the Warning Events of the last hour.

Then comes the seam, "since Argus restarted at hh:mm", on Oslo's clock (the binary embeds `time/tzdata`), at the time Argus started. Everything after it is what Argus saw happen.

## The cluster signal

client-go's informers keep their last state and retry quietly while the API server is away. So `main` probes the API server's `/readyz` every 5 s. `/readyz` is open to any authenticated caller through `system:public-info-viewer`, so it needs no grant of Argus's own. The first failed probe makes the state `interrupted`, and 30 s of failures make it `lost` (`platformstate.WatchLost`). #100 says a broken watch makes things Unknown, and story 11 says the last known state must stay on screen. Both hold: the objects are not rewritten to Unknown on the server; the `cluster` message says to read every Condition as Unknown, and the drawing greys the scene under a banner. Losing the cluster is a normal note, and seeing it again is feed-only.

## Serving

- **The web directory** is `cmd/iidp-argus/web`, embedded with `go:embed` in `main`, where the prototype put it. For now it holds a placeholder: `index.html` and `app.js`, a hand-written module with `// @ts-check` that keeps the model from the stream and shows it as text. #119 replaces it.
- **The probes.** `/healthz` answers 200 while the process serves, and is the liveness probe. `/readyz` answers 200 once the first look at the cluster is complete, and 503 before; it is the readiness probe. A lost cluster fails neither, since Argus then still serves the last it saw, and says so.
- **Read-only.** Every route is a GET, and the ClusterRole grants nothing but reads.

## The component

- **Where it runs.** `bootstrap/templates/argus.yaml` renders the ArgoCD Application `argus` only with `argus.enabled`. It follows the bootstrap pin, as the gate's Application does, into the namespace `argus`. Unlike the gate, Argus needs no Secret of `argocd`'s, so it gets a namespace of its own. Off renders nothing, so there is no Deployment, RBAC or Ingress, and so no DNS record: external-dns makes records from Ingress hosts. `bootstrap/values.yaml` has it on. The kind fixture turns it off.
- **The Deployment.** One replica, with a service account whose token only the pod mounts, no volumes, a read-only root filesystem, and a security context like the gate's (non-root, no privilege escalation, every capability dropped). It asks for 10m of CPU and 48Mi of memory, with a 96Mi memory limit and no CPU limit. `GOMEMLIMIT=48MiB` makes the Go collector work harder as the heap nears the request, well before the limit. The first sync's transient peak (below) is what it bounds.
- **The Ingress** is at `argus.<baseDomain>`, with the shared Itema login middleware's annotation and TLS from the wildcard, as #101 decided. `TestArgusComponentRendersArgus` reads the middleware's name from `components/oauth2-proxy-login/middleware.yaml`, so the two cannot drift apart. The ForwardAuth checks every request, `/events` included. A stream reconnecting after the Itema login session has expired gets oauth2-proxy's redirect to Entra ID, which an EventSource cannot follow, so #119 should reload the page when the stream keeps failing.
- **The RBAC** is one ClusterRole, `iidp-argus`, with `get`, `list` and `watch` on exactly the ten resources of the ticket, bound to `argus/iidp-argus` alone. `TestArgusReadsTheClusterWithGetListWatchOnly` fails on anything else. `TestSourcesReadOnlyTheGrantedResources` checks that the informers read those ten and no others, and that every informer but the Certificates' and the node's is narrowed.
- **The image** is `ghcr.io/itema-as/iidp-argus:<version>`, built by GoReleaser's `dockers_v2` for linux/amd64 from `cmd/iidp-argus/Dockerfile`. That file is the gate's, less git. The release workflow sets `IIDP_PUBLISH_ARGUS`, as it sets `IIDP_PUBLISH_DEPLOY_GATE`. The tag is derived from the bootstrap revision as the gate's is, and a pin that is not a release tag fails only this Application.

## Memory

`TestMemoryWithThirtyEnvironments` (`cmd/iidp-argus/memory_test.go`, run with `IIDP_ARGUS_MEASURE=1`) builds a synthetic Platform of 15 Applications, each with prod and staging: 30 Environments. Each has its ArgoCD Application, with ten history entries and a twelve-entry resource list. It also has a Deployment, two pods, a migration Job and three task Jobs with their pods, a CronJob, a CNPG Cluster and its pod, a Certificate, two Ingresses, ten Events, and the Platform's own Deployments and pods in `argocd` and `kube-system`. Eleven component Applications and the node, with 80 images in its status, complete it. That is 874 objects, each as fat as the API server's: managedFields, full pod specs and fat status.

The real dynamic client reads them over HTTP from a stand-in API server, which serves list, watch and the streaming watch-list. client-go 0.36 used the watch-list for every informer, so the objects arrive one at a time and each is transformed before the next. The test also checks that the model holds all 15 Applications, 30 Environments, 120 Capabilities and 13 components. The figures, from three runs on 2026-09-28 (Go 1.27, darwin/arm64):

| | Heap held once synced | Highest above the start while syncing |
|---|---|---|
| Argus: informers with transforms, the store, the model and a snapshot | **5.9–7.5 MiB** | 14–18 MiB |
| The same informers without transforms | 46.2–46.8 MiB | 48–50 MiB |

This matches #98's research, 4.1 MiB stripped against 44.7 MiB unstripped, for about 30 Environments. The steady heap is under 8 MiB. The binary is 12.1 MiB, of which only the pages in use are resident. The Go runtime adds a few MiB. Even with the first sync's garbage on top, before `GOMEMLIMIT`'s collector reclaims it, Argus should stay well under the 64 MiB target. **Not measured on the node**: the process's RSS in the cluster should be read with `kubectl top pod -n argus` at the rollout. An earlier measurement with client-go's fake dynamic client was discarded: the fake deep-copies its whole store for every list, so its peak says nothing about a real API server.

## The dependency footprint

`go.mod` gains three direct requirements, `k8s.io/client-go v0.36.4`, `k8s.io/apimachinery v0.36.4` and `k8s.io/klog/v2 v2.140.0`. klog is client-go's logger, which `main` points at its JSON log. It also gains 30 indirect ones, among them `k8s.io/kube-openapi`, `k8s.io/utils`, `sigs.k8s.io/{json,randfill,structured-merge-diff/v6,yaml}`, `github.com/fxamacker/cbor/v2`, `github.com/google/gnostic-models`, `golang.org/x/{net,oauth2,text,time}` and `google.golang.org/protobuf`. `k8s.io/api` is not among them: it left with `dynamicinformer`. Some are there only because client-go's module needs them to build, and are not linked. `go list -deps ./cmd/iidp-argus` links 87 `k8s.io` packages. The CLI and the gate link none (`TestOnlyArgusLinksClientGo`), and their binaries are unchanged. ADR-0008 (#120) records why client-go reverses the repository's minimal-dependency habit.

## The kind e2e: not added

The ticket asks for it only if the 2-vCPU runner's budget allows. Argus's 10m CPU request fits, so budget is not the reason. The harness would have to cross-compile and load a second image, as it does the gate's, and an Argus check would have to read `/events` through Traefik and Itema login, or port-forward past them. None of that could be run here: there was no container runtime to run kind, so the check would reach CI untried. The fixture therefore keeps `argus.enabled: false`. The rendered-bootstrap tests, the transform tests and the informer-to-stream test on the fake dynamic client cover what the e2e would, short of a real API server. A follow-up can add it: build the image in `test/e2e/deploygate.go`'s way, turn Argus on in the fixture, and read one snapshot holding `shop`'s Environments.

## Tests

- `internal/platformstate/capability_test.go`:
  - `TestItemaLoginFromIngresses`: no Ingress, no annotation, another middleware, the shared one, the Environment's own, among others, two Ingresses, and Leaving;
  - `TestFailedScheduledTaskRunIsAWarning`: no run, succeeded, failed, failed then succeeded, failed then running, another task's; the Environment Healthy throughout, and its Tasks agreeing;
  - the component table gains the node's four states and ArgoCD's health with and without the workload in view;
  - `TestObjectsDecodeTheAPIsJSON` decodes an Ingress and a Node.
- `internal/argus`:
  - `server_test.go`, with `httptest`: `TestStreamSendsTheSnapshotThenDeltas`, a Healthy delta within the grace and a Degraded delta and loud note after it, with nothing sent when nothing changed; `TestStreamKeepsAlive`; `TestReconnectGetsAFreshSnapshot`, which also checks that a stream carries no `id:`; `TestASlowBrowserIsClosed`; `TestProbesAndTheWebDirectory`; `TestClusterLostAndFound`.
  - `feed_test.go`: `TestFeedKeeps200NotesOr24Hours`, and `TestSeedFillsTheFeedAndMarksTheSeam`, which covers the history, the failed operation, the Deploys, the last hour of Warning Events, the seam on Oslo's clock, and live notes after it.
  - `notes_test.go`: `TestLoudnessTable`, a case per row, and `TestComponentNotes`.
  - `model_test.go`: `TestAnEnvironmentHasItsNamespacesObjects`, `TestOutOfSyncIsTimedFromWhenArgusSawIt`, `TestPlatformComponents` and `TestOnlyArgusLinksClientGo`.
- `cmd/iidp-argus`:
  - `TestTransformsKeepOnlyWhatTheModelReads`;
  - `TestAnObjectThatDoesNotDecodeIsLeftOut`;
  - `TestSourcesReadOnlyTheGrantedResources`;
  - `TestInformersToStream`: on the fake dynamic client, through the real informers and transforms to the SSE handler, a pod going unready gives a Healthy delta, then a Degraded one once the clock passes 60 s, and the gate's Event gives a Deploy at Accepted and its note;
  - `TestMemoryWithThirtyEnvironments`, opt-in.
- `bootstrap/bootstrap_test.go`:
  - `TestArgusIsOnByDefaultAndOffRendersNothing`;
  - `TestArgusComponentRendersArgus`: one replica, the image, the resource figures and `GOMEMLIMIT`, the probes, and the Ingress's host, TLS and Itema login middleware;
  - `TestArgusReadsTheClusterWithGetListWatchOnly`;
  - `TestArgusImageTagCanBePinnedButNotGuessed`.

## Open questions

- **ArgoCD's health as a component's Condition** when none of its pods are in view is a rule #100 did not give. The alternative is to watch Deployments and pods in every component's namespace, which is a handful more objects.
- **Links and versions.** Argus's Environments carry no `links` (ArgoCD, Grafana) and no `image.deployedAt`, and components carry no version. The card in #113 wants them. The links need `argocdURL` and `grafanaURL` passed to Argus, and the Deploy gate's `links` moved into `platformstate`. `deployedAt` needs the Platform repository, which Argus does not read. A component's version could come from its ArgoCD Application's `targetRevision`, or from the node's `kubeletVersion` for k3s.
- **Sign-in groups** are not visible on the Itema login Capability: the model knows only which middleware an Ingress names.
- **Not confirmed from a source.** Whether ingress-shim copies an Ingress's labels to its Certificates (moot here, since Certificates are read unnarrowed), and whether `events.k8s.io/v1` accepts a `type` field selector (moot, since Events are read through core v1).

## For the rollout (#96)

- Release, then bump the bootstrap. The bump creates the `argus` Application, the namespace `argus`, the ClusterRole and its binding, and the Ingress, and external-dns adds `argus.app.itma.no`.
- Open `https://argus.app.itma.no`: Itema login, then the placeholder with every Application and the feed's seam. `kubectl top pod -n argus` gives the real memory. `kubectl logs -n argus deploy/iidp-argus` should show "Argus has read the cluster" and no repeated "a watch failed".
