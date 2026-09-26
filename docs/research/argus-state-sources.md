# How can Argus hear about Platform state changes live?

Research for [#98](https://github.com/Itema-as/iidp/issues/98), part of the Argus map [#97](https://github.com/Itema-as/iidp/issues/97). Written 2026-09-26.

The question is which sources a small in-cluster Go component can watch to learn, live, about every Environment's sync and health, its Capabilities, and the Platform components. It must fit a 64 MiB backend budget with no new datastore. This compares two options, ArgoCD's API and watching Kubernetes directly, and then says what only the Deploy gate knows.

**Versions researched against.** Everything below was checked against what `bootstrap/versions.yaml` pins, not against `latest`:

| Component | Pin | Version read |
|---|---|---|
| ArgoCD | argo-cd chart 10.9.2 | ArgoCD **v3.5.3** (chart `appVersion`; also `docs/implementation-notes/04-bootstrap.md`) |
| Kubernetes | k3s v1.36.4+k3s1 | kubernetes **v1.36.4**, client-go **v0.36.1** (the client-go ArgoCD v3.5.3 itself uses, `go.mod`) |
| CloudNativePG | chart 0.29.0 | operator **v1.30.0** (chart `appVersion`) |
| cert-manager | chart v1.21.2 | **v1.21.2** |
| external-dns | chart 1.22.0 | **v0.22.0** |

**Markers.** Claims carry a source link. **[measured]** marks numbers from a local experiment, described where they appear. **[inferred]** marks conclusions drawn from docs or code but not observed on a running Platform. **[unconfirmed]** marks anything I could not verify. The last section lists each unconfirmed item and where I looked.

## The answer in short

- **Both options see ArgoCD's view.** An ArgoCD `Application` is one Environment (`CONTEXT.md`, `docs/platform-repository.md`). Its `.status` carries sync status, app-level health, `operationState`, the deployed revisions, and the running images. You can read it from the ArgoCD API stream or by watching the CR directly. Either way you get the same data at about the same latency.
- **ArgoCD v3 changed what the CR carries.** Per-resource health no longer lives in the CR. It lives in ArgoCD's Redis and reaches clients only through the API. So the API is the only way to get *ArgoCD's* per-resource health and resource tree (pods and their restart counts included). The alternative is for Argus to watch those resources itself.
- **Watching Kubernetes directly** covers everything in both sources except that tree. It also sees what ArgoCD doesn't: Traefik and other k3s-managed parts, the node, and Kubernetes Events. It needs a service account with a read-only ClusterRole. The API needs an ArgoCD API token, which has to be minted by hand. Informers must strip objects with transform funcs to fit the budget. Unstripped caches alone measured about 45 MiB for a 15-Application Platform. Stripped, they measured about 4 MiB **[measured]**.
- **Neither source can see DNS records.** external-dns keeps no per-record state in the cluster. Neither can see the Deploy gate's side of a Deploy. The gate knows about a Deploy up to about 3 minutes before ArgoCD does, because ArgoCD polls the Platform repository. It also knows about refused Deploys, which never reach git. Today that knowledge exists only in a log line on stdout and, for successful Deploys, in the commit.
- **Recommendation** (below): watch Kubernetes directly, using ArgoCD `Application` CRs for sync and operations and the Environments' own objects for health, all with label selectors and transform funcs. Add a small read-only "recent Deploys" feed from the Deploy gate for the pre-ArgoCD stage. Don't depend on the ArgoCD API.

## What the Platform runs (verified in this repository)

- **One ArgoCD `Application` per Environment.** It lives in the `argocd` namespace and is named `<app>-<env>`. Its destination is the namespace of the same name. It is labelled `iidp.itema.no/application` and `iidp.itema.no/environment`. It has two sources: the chart from GHCR (OCI) and the Platform repository as `ref: values`. Sync is automated with `prune` and `selfHeal`, and the resources finalizer is set (`docs/platform-repository.md`, "applications/<name>/<environment>/application.yaml"). Retry is `limit: -1`, `refresh: true`, with backoff from 10 s up to 3 min (`docs/implementation-notes/75-cutover-friction.md`).
- **Platform components are ArgoCD Applications too.** These are `argocd`, `cert-manager`, `external-dns`, `cloudnative-pg`, `cnpg-barman-cloud`, `deploy-gate`, `monitoring`, `oauth2-proxy` and `platform-tls` (`bootstrap/templates/`), plus the root Applications in the Platform repository. **Traefik is not.** k3s installs it through its own helm-controller (`bootstrap/versions.yaml`, `k3s.traefik`).
- **What one Environment renders** (`chart/application/templates/`):
  - a Deployment with one replica, and no `revisionHistoryLimit` or `progressDeadlineSeconds`, so the Kubernetes defaults of 10 and 600 s apply
  - a Service and an Ingress
  - with Postgres: a CNPG `Cluster`, an `ObjectStore` and a `ScheduledBackup`
  - a migration `Job`, as an ArgoCD `Sync` hook at wave −1 with `BeforeHookCreation`, so the last one stays until the next sync
  - a `PreDelete` final-backup `Job`, with `BeforeHookCreation,HookSucceeded`, created only when the Environment's Application is deleted
  - an extra `-http01` Ingress for custom domains outside the wildcard

  Every object and every pod template carries the `iidp.itema.no/*` labels. That includes the migration and final-backup Job pods, and the CNPG instance pods through `inheritedMetadata`. So one label selector finds an Environment's objects. There are no CronJobs yet: Scheduled tasks are Phase 2 design.
- **ArgoCD settings that matter here.**
  - `admin.enabled: false`, and there are no local accounts.
  - RBAC `policy.default: role:readonly` for every authenticated user (`bootstrap/templates/argocd.yaml`).
  - SSO through Dex with Entra.
  - No webhook from GitHub. ArgoCD relies on polling (`docs/implementation-notes/39-final-backup-predelete-hook.md`: "`bootstrap/applications.yaml` configures no webhook").
- **The Deploy gate has no Kubernetes API access today.** Its pod runs with `automountServiceAccountToken: false` and no Role (`bootstrap/components/deploy-gate/templates/deployment.yaml`, `docs/implementation-notes/60-deploy-gate.md`). The Phase 2 design on the unmerged branch `adriansberg/phase-2-design` changes that. ADR-0007 has `iidp app status` read "ArgoCD Applications, Pods, Jobs and CronJobs in Application namespaces" through the gate's service. ADR-0006 makes Preview Environments come from an ArgoCD ApplicationSet, which the gate never sees.

## Option A: ArgoCD's API

### The streaming endpoints

The API is gRPC with an HTTP/JSON gateway. The two server-streaming calls are ([`server/application/application.proto` L367–371, L470–473](https://github.com/argoproj/argo-cd/blob/v3.5.3/server/application/application.proto#L367-L473)):

| Call | HTTP | Carries |
|---|---|---|
| `Watch` | `GET /api/v1/stream/applications` | `ApplicationWatchEvent{type, application}`: the **whole Application**, both spec and status, on every change |
| `WatchResourceTree` | `GET /api/v1/stream/applications/{name}/resource-tree` | `ApplicationTree{nodes, orphanedNodes, hosts}` for **one** Application, sent whole on every change |

`Watch` accepts `name`, `projects`, `selector` (a label selector), `appNamespace`, `repo` and `resourceVersion` ([`ApplicationQuery`, proto L21–38](https://github.com/argoproj/argo-cd/blob/v3.5.3/server/application/application.proto#L21-L38)). Non-streaming companions are `GET /api/v1/applications`, `GET /api/v1/applications/{name}/resource-tree`, and `GET /api/v1/applications/{name}/events`. The last one returns the Kubernetes Events for an Application or resource.

**What an Application event carries** ([`ApplicationStatus`, `types.go` L1213–1244](https://github.com/argoproj/argo-cd/blob/v3.5.3/pkg/apis/application/v1alpha1/types.go#L1213-L1244)):

- `sync.status` and `sync.revisions`. The revisions are one per source, so for our Environments that is the chart version and the Platform repository commit.
- `health` (app-level status, message and `lastTransitionTime`).
- `operationState`: `phase`, `message`, `startedAt`, `finishedAt`, `retryCount`, and `syncResult.resources[]`. Each of those resources has `hookType` and `hookPhase`, which is how the migration hook's outcome shows up.
- `history[]`: past syncs with `revisions`, `deployStartedAt`, `deployedAt` and `initiatedBy`. This is capped by `revisionHistoryLimit`, [default 10](https://github.com/argoproj/argo-cd/blob/v3.5.3/pkg/apis/application/v1alpha1/types.go#L92-L96).
- `conditions` (for example `DeletionError` when the PreDelete hook fails).
- `summary.images` and `summary.externalURLs`.
- `resources[]`: the managed resources with their sync status.
- `metadata.deletionTimestamp`, set while an Environment is being deleted.

**Per-resource health is not in the CR in v3, but the API fills it in.** Since ArgoCD 3.0, "the health status is stored externally" and the CR carries `resourceHealthSource: appTree` instead ([upgrading 2.14→3.0, "Health status in the Application CR"](https://github.com/argoproj/argo-cd/blob/v3.5.3/docs/operator-manual/upgrading/2.14-3.0.md#health-status-in-the-application-cr)). `controller.resource.health.persist` defaults to `"false"` ([`argocd-cmd-params-cm.yaml` L84–87](https://github.com/argoproj/argo-cd/blob/v3.5.3/docs/operator-manual/argocd-cmd-params-cm.yaml#L84-L87)). Our bootstrap doesn't set it. The API server puts per-resource health back into each streamed Application from the tree in Redis (`inferResourcesStatusHealth`, [`application.go` L1281, L2905–2926](https://github.com/argoproj/argo-cd/blob/v3.5.3/server/application/application.go#L2905-L2926)). The app-level `status.health` is still in the CR.

**What the resource tree carries.** Every live object that belongs to the Environment is a node, including children found through owner references, such as ReplicaSets, Pods and ingress-shim's Certificates. Each node has health (ArgoCD's health checks, including the built-in Lua checks for [CNPG `Cluster`](https://github.com/argoproj/argo-cd/blob/v3.5.3/resource_customizations/postgresql.cnpg.io/Cluster/health.lua) and [cert-manager `Certificate`](https://github.com/argoproj/argo-cd/blob/v3.5.3/resource_customizations/cert-manager.io/Certificate/health.lua)), parent refs, images, and info items. For pods the info items are `Status Reason` (for example `CrashLoopBackOff`), `Containers` (ready/total) and `Restart Count` ([`controller/cache/info.go` L740–750](https://github.com/argoproj/argo-cd/blob/v3.5.3/controller/cache/info.go#L740-L750)).

### Wire format

- **Format.** The gateway writes newline-delimited JSON objects `{"result": …}`. With `Accept: text/event-stream` it writes Server-Sent Events (`data: …`) instead, plus a `:` keepalive comment every 15 s ([argoproj/pkg v2.0.1 `grpc/http/forwarders.go` L100, L187](https://github.com/argoproj/pkg/blob/v2.0.1/grpc/http/forwarders.go#L100); wired up in [`forwarder_overwrite.go` L159–166](https://github.com/argoproj/argo-cd/blob/v3.5.3/pkg/apiclient/application/forwarder_overwrite.go#L159-L166)).
- **`fields`.** A `fields` query parameter prunes each message on the server. Paths are relative to the wrapped message, so they start with `result.`. A leading `-` excludes the listed fields instead ([forwarders.go L137–150](https://github.com/argoproj/pkg/blob/v2.0.1/grpc/http/forwarders.go#L137-L150)).
- **Deduplication.** On `Watch`, the server drops a message identical, after pruning, to the last one sent for the same Application name ([forwarders.go L241–263](https://github.com/argoproj/pkg/blob/v2.0.1/grpc/http/forwarders.go#L241-L263)). So a narrow `fields` list also cuts the noise.

### How an in-cluster service would authenticate

- **The account.** Add a local account with only the `apiKey` capability (`accounts.argus: apiKey` in `argocd-cm`), then mint a token with `argocd account generate-token --account argus` ([user management](https://github.com/argoproj/argo-cd/blob/v3.5.3/docs/operator-manual/user-management/index.md#local-usersaccounts)). Argus sends it as `Authorization: Bearer <token>` ([API docs](https://github.com/argoproj/argo-cd/blob/v3.5.3/docs/developer-guide/api-docs.md)).
- **What the token is.** An HS256 JWT signed with `server.secretkey`. It is only valid while its id is listed on the account ([`sessionmanager.go` L295](https://github.com/argoproj/argo-cd/blob/v3.5.3/util/session/sessionmanager.go#L295)). So it is state inside `argocd-secret`, created imperatively by someone who may update accounts. That fits badly with a bootstrap where nobody logs in with a local account and the admin account is off.
- **Where the token would come from here [unconfirmed].** Whether `argocd --core` (the Platform admin's path, `bootstrap/values.yaml`) can mint account tokens, or whether cloud-init could do it, is unconfirmed.
- **RBAC.** `policy.default: role:readonly` applies to every authenticated subject ([rbac.md](https://github.com/argoproj/argo-cd/blob/v3.5.3/docs/operator-manual/rbac.md)). So an Argus token is at least `role:readonly`: `get` on applications, applicationsets, projects, clusters, repositories, accounts, certificates, gpgkeys, **and logs**, for everything ([`builtin-policy.csv`](https://github.com/argoproj/argo-cd/blob/v3.5.3/assets/builtin-policy.csv)). It can't be narrower without changing the default for everyone.
- **Projects.** Every Environment is in `default` today (`docs/platform-repository.md`). A project-scoped role token (`argocd proj role create-token`) would be the narrower alternative. It is also imperative state, stored in the AppProject.

### Reconnect semantics

- **No replay.** The server's broadcaster keeps no history. A `resourceVersion` on a multi-Application watch only skips the initial list and filters older objects ([`application.go` L1226–1233, L1293–1306](https://github.com/argoproj/argo-cd/blob/v3.5.3/server/application/application.go#L1293-L1306)). Anything that changed while Argus was disconnected is not sent. The only safe reconnect is without `resourceVersion`, which re-sends every Application as `ADDED`. That is what ArgoCD's own UI does: it reconnects with `repeat()`/`retry()` ([`applications-service.ts` L262–266](https://github.com/argoproj/argo-cd/blob/v3.5.3/ui/src/app/shared/services/applications-service.ts#L262-L266)).
- **Slow subscribers lose events.** Each subscriber gets a 1000-event buffer (`watchAPIBufferSize`). When it is full, events are **dropped** with a warning ([`broadcaster.go` L64–75](https://github.com/argoproj/argo-cd/blob/v3.5.3/server/broadcast/broadcaster.go#L64-L75)).
- **The tree stream sends nothing on connect.** `WatchResourceTree` is a Redis pub/sub subscription. It sends the tree only when the controller next publishes ([`util/cache/redis.go` L157–176](https://github.com/argoproj/argo-cd/blob/v3.5.3/util/cache/redis.go#L157-L176)). A client must `GET …/resource-tree` first, then stream. Pub/sub is fire-and-forget, so a gap means a re-GET.
- **One tree stream per Environment.** It takes one long-lived connection per Environment to cover them all.
- **Restarts drop every stream.** An argocd-server restart ends them all, and so does a Redis restart for the tree streams. A chart bump restarts ArgoCD (`docs/implementation-notes/04-bootstrap.md`).

## Option B: watching Kubernetes directly

### What each object tells Argus

Selectors below use the labels the chart and the Application CR already carry.

| Object | Where / selector | What it tells us | Notes |
|---|---|---|---|
| ArgoCD `Application` | `argocd` ns; `iidp.itema.no/application` (Environments) or all (Platform components) | Sync status, app health, operation phase/message/timestamps, sync revisions (the Platform repository commit), hook results, history, images, conditions, deletion in progress | Same object Option A streams. **No per-resource health** (above). |
| `Deployment` | label selector | Rollout: `observedGeneration`, `updatedReplicas`, `readyReplicas`, the `Progressing` condition (`ProgressDeadlineExceeded` after the 600 s default) | Enough for "rolling out" and "stuck". |
| `Pod` | label selector | `containerStatuses[].restartCount`, `state.waiting.reason` (CrashLoopBackOff, ImagePullBackOff), readiness | App, CNPG instance, migration and final-backup pods all carry the labels. |
| `ReplicaSet` | label selector | Not needed: Deployment and Pod cover rollout. It was the largest cache in the measurement (11 per Deployment by default). | Skip. |
| `Job` | label selector | Migration hook and final backup: `status.active/succeeded/failed`, `Complete`/`Failed` conditions | The final-backup Job exists only while an Environment is Leaving, which is a live signal of its own. |
| CNPG `Cluster` | label selector | `status.phase` / `phaseReason`, `instances` / `readyInstances`, conditions `Ready`, `ContinuousArchiving`, `LastBackupSucceeded` ([CNPG v1.30.0 `cluster_types.go` L895ff, L1173–1178](https://github.com/cloudnative-pg/cloudnative-pg/blob/v1.30.0/api/v1/cluster_types.go#L1173-L1178)) | ArgoCD's Lua check maps phase strings to health. Argus can use the same table ([health.lua](https://github.com/argoproj/argo-cd/blob/v3.5.3/resource_customizations/postgresql.cnpg.io/Cluster/health.lua)). |
| cert-manager `Certificate` | label selector for custom domains; `kube-system/wildcard` for the Platform | `Ready` / `Issuing` conditions | ingress-shim copies the Ingress's labels onto the Certificate and makes the Ingress its controller owner ([cert-manager v1.21.2 `certificate-shim/sync.go` L395–419](https://github.com/cert-manager/cert-manager/blob/v1.21.2/pkg/controller/certificate-shim/sync.go#L395-L419)). |
| Kubernetes `Event` | field selectors only (no labels); e.g. `type=Warning`, `involvedObject.kind=Application` | Warnings (BackOff, FailedScheduling, OOMKilling…), and ArgoCD's own `OperationStarted` / `OperationCompleted` / `ResourceUpdated` Events on each Application ([`audit_logger.go` L40–46](https://github.com/argoproj/argo-cd/blob/v3.5.3/util/argo/audit_logger.go#L40-L46)) | Selectable fields: `involvedObject.{kind,namespace,name,uid,apiVersion,fieldPath}`, `reason`, `reportingComponent`, `source`, `type` ([`event/strategy.go` L116–130](https://github.com/kubernetes/kubernetes/blob/v1.36.4/pkg/registry/core/event/strategy.go#L116-L130)). Kept **1 hour** by default (`--event-ttl`, [`options.go` L129](https://github.com/kubernetes/kubernetes/blob/v1.36.4/pkg/controlplane/apiserver/options/options.go#L129); k3s's control-plane setup doesn't override it, [`server.go`](https://github.com/k3s-io/k3s/blob/v1.36.4%2Bk3s1/pkg/daemons/control/server.go)). The API calls them "informative, best-effort, supplemental data" ([`events/v1/types.go` L28–33](https://github.com/kubernetes/api/blob/v0.36.1/events/v1/types.go#L28-L33)). |
| Platform parts outside ArgoCD | `kube-system` Deployments (Traefik, CoreDNS, metrics-server), `Node` | Traefik up or down, node conditions (`MemoryPressure` on a 4 GB node) | Invisible to Option A except where ArgoCD shows cluster info. |

### Memory controls in client-go v0.36.1

- **Transform funcs.** Set on an informer before it starts. They are "intended for you to take the opportunity to remove, transform, or normalize fields. One use case is to strip unused metadata fields out of objects to save on RAM cost" ([`shared_informer.go` L235–244](https://github.com/kubernetes/client-go/blob/v0.36.1/tools/cache/shared_informer.go#L235-L244)). Since v1.27 the transform "sees the object before any other actor, and it is now safe to mutate the object in place" ([`delta_fifo.go` L160–176](https://github.com/kubernetes/client-go/blob/v0.36.1/tools/cache/delta_fifo.go#L160-L176)). Factories take `WithTransform` ([`informers/factory.go` L107](https://github.com/kubernetes/client-go/blob/v0.36.1/informers/factory.go#L107)).
- **Label and field selectors** through `WithTweakListOptions` or `NewFilteredDynamicInformer`. Field selectors are limited to each type's selectable fields. For Pods those are `spec.nodeName`, `spec.restartPolicy`, `spec.schedulerName`, `spec.serviceAccountName`, `spec.hostNetwork`, `status.phase`, `status.podIP` and `status.nominatedNodeName` ([`pod/strategy.go` L510–527](https://github.com/kubernetes/kubernetes/blob/v1.36.4/pkg/registry/core/pod/strategy.go#L510-L527)). For Jobs it is `status.successful` ([`job/strategy.go` L451](https://github.com/kubernetes/kubernetes/blob/v1.36.4/pkg/registry/batch/job/strategy.go#L451)).
- **Metadata-only informers** ([`metadatainformer`](https://github.com/kubernetes/client-go/blob/v0.36.1/metadata/metadatainformer/informer.go#L185)) keep `PartialObjectMetadata`. They are useful only where existence, labels or deletion is the signal, such as Namespaces, Ingresses, PVCs and ReplicaSets. They are useless for health, which lives in `.status`.
- **Streaming initial lists.** client-go's `WatchListClient` is on by default from 1.35 ([`known_features.go` L141–143](https://github.com/kubernetes/client-go/blob/v0.36.1/features/known_features.go#L141-L143)). The apiserver's `WatchList` is on by default from 1.34 ([`kube_features.go` L536–541](https://github.com/kubernetes/kubernetes/blob/v1.36.4/staging/src/k8s.io/apiserver/pkg/features/kube_features.go#L536-L541)). So on k3s v1.36 the initial sync streams items one by one instead of decoding one big list, which lowers the startup peak **[inferred]**.
- **Dynamic client, not typed ArgoCD or CNPG clients.** Importing `github.com/argoproj/argo-cd/v3` for its types pulls in ArgoCD's whole module graph. The dynamic client with `unstructured` plus a transform avoids that. This repository keeps its dependencies to four modules today (`go.mod`), and the Deploy gate note rejected client-go once already for a smaller need (`docs/implementation-notes/60-deploy-gate.md`). Adding client-go is a real cost: it grew a stripped linux/amd64 binary from 4.3 MB to 25 MB **[measured]**.

### Memory measurement [measured]

**Method.** A throwaway program ran real client-go v0.36.1 dynamic informers (reflector, DeltaFIFO, indexer) against the fake dynamic client. The objects were synthetic but shaped like ours: managedFields, full status, 10-entry ArgoCD history and syncResult. Each informer's store was released one at a time and the heap was compared after GC. Go 1.27, darwin/arm64.

**The Platform modelled.** Environments with Postgres, plus 12 Platform Applications and 1500 Events. For each Environment: 1 Application, 1 Deployment, 11 ReplicaSets, 3 Pods, 1 CNPG Cluster and 1 Job. Average JSON sizes: Application 17 KB, Pod 6.6 KB, ReplicaSet 4 KB, Cluster 8.7 KB, Event 1.2 KB.

| Environments | Full `unstructured` caches | Stripped by transform funcs |
|---|---|---|
| 10 | 22.9 MiB (Events 10.4, ReplicaSets 4.9, Applications 2.9) | 3.2 MiB (Events 2.7) |
| 30 (≈15 Applications with staging) | **44.7 MiB** (ReplicaSets 14.6, Events 10.3, Pods 7.3, Applications 5.6) | **4.1 MiB** (Events 2.7, ReplicaSets 0.7, Pods 0.3, Applications 0.2) |
| 150 | 175.9 MiB | 9.5 MiB |

The transforms kept these fields:
- Application: sync status and revisions, health, summary, `operationState` phase, message and timestamps, conditions
- Pod: phase and `containerStatuses`
- Cluster: phase, reason, instance counts and conditions
- Event: `involvedObject`, reason, message, type, time and count
- ReplicaSet, Deployment and Job: status

Process baselines, idle, measured with `getrusage` on macOS: a Go binary linking only `net/http` and `encoding/json` used 11 MiB max RSS. One linking client-go's dynamic and metadata clients used 21 MiB. The Go GC lets the heap grow to about live heap × (1 + GOGC/100) before collecting ([Go GC guide](https://go.dev/doc/gc-guide)). `GOMEMLIMIT` caps that.

**Reading the numbers against the 64 MiB budget:**
- **Option B, stripped:** about 21 MiB baseline plus 4 MiB of caches with GC headroom, so roughly **30 MiB**. That leaves room for the browser fan-out and the in-memory event feed. Dropping ReplicaSets and selecting `type=Warning` Events makes it smaller still.
- **Option B, unstripped:** 45 MiB live plus the baseline already exceeds the budget before GC headroom. **Transform funcs are mandatory, not an optimisation.**
- **Option A:** Argus holds only what it keeps from the pruned streams. That is a few KB per Environment for summaries, plus the trees if kept, which I estimate at tens of KB each **[unconfirmed, not measured]**. So roughly **15–20 MiB** in total. The cost moves into argocd-server and Redis, which already run.

The objects were synthetic and the transport was fake. Re-measure on the node with the real process: container `memory` usage and `/memory/classes/*` runtime metrics.

### RBAC

Argus needs a ServiceAccount token (automounted) and a **ClusterRole** with only `get`, `list` and `watch` on:
- `argoproj.io` applications (in `argocd` only, so a Role there would do)
- `apps` deployments
- `""` pods, events, namespaces and nodes
- `batch` jobs
- `postgresql.cnpg.io` clusters
- `cert-manager.io` certificates

Nothing on secrets.

It has to be cluster-scoped because ArgoCD creates each Environment's namespace on first sync (`CreateNamespace=true`). A per-namespace Role would need something to create it first, such as the chart rendering a RoleBinding the way it already renders the final-backup Role. RBAC can't restrict by label: selectors narrow the data, not the permission.

### Reconnect behaviour

- **Handled by the reflector.** Watches time out on purpose after a randomized 5–10 minutes and resume from the last resourceVersion ([`reflector.go` L57–58](https://github.com/kubernetes/client-go/blob/v0.36.1/tools/cache/reflector.go#L57-L67)). An expired resourceVersion (HTTP 410) triggers a relist. Watch failures back off from 800 ms up to 30 s. A 429 backs off. All of this is handled inside the reflector ([L600–670](https://github.com/kubernetes/client-go/blob/v0.36.1/tools/cache/reflector.go#L600-L670)).
- **No silently stale cache.** Unlike ArgoCD's broadcaster, the cache never silently goes stale: either the watch resumes, or the informer relists and delivers the difference. States that come and go during a relist can still be missed. That matters for the event feed, not for current state.
- **When the apiserver is down,** so is everything else on the node.
- **When Argus restarts,** the informers relist and current state is complete within seconds. The in-memory event feed is lost. Much of it can be rebuilt from each Application's `status.history` (the last 10 syncs with timestamps and revisions), its `operationState`, and Events from the last hour.

## Side by side

| | A. ArgoCD API streams | B. Kubernetes watches (Application CRs + objects) |
|---|---|---|
| **Sync, operation, revisions, images** | Yes | Yes, the same CR |
| **App-level health** | Yes | Yes (in the CR) |
| **Per-resource health, pod restarts, resource tree** | Yes, ArgoCD's health checks | Not from the CR in v3. Yes by watching the objects and applying simple rules (CNPG and Certificate rules can copy ArgoCD's Lua). |
| **Migration hook / final backup** | Hook results in `operationState` and the tree | Same CR fields, plus the Job objects live |
| **Traefik, k3s parts, Node** | No | Yes |
| **Kubernetes Events** | Per-Application `GET`, no stream | Watch, field-selected, 1 h retention |
| **DNS records (external-dns)** | No | No (see below) |
| **The Deploy before ArgoCD sees it; refused Deploys** | No | No |
| **Preview Environments (Phase 2)** | Yes (ApplicationSet-generated Applications) | Yes, same |
| **Latency, cluster change → Argus** | CR changes are near-immediate. Tree changes from status-only updates that don't change health wait for the next refresh, up to about 3 min **[inferred]** (below). | Near-immediate for everything watched **[inferred]**: a watch delivers each committed change |
| **Latency, Platform repository commit → ArgoCD** | 2–3 min (poll) for both | same |
| **Memory in Argus** | ~15–20 MiB estimated | ~30 MiB estimated with transforms; over budget without |
| **Credential** | A hand-minted ArgoCD token, at least `role:readonly` (includes logs) | A ServiceAccount token, a read-only ClusterRole, all declared in the bootstrap |
| **Failure** | argocd-server or Redis restart drops the streams; full re-list on reconnect; a slow reader drops events | Reflector resumes or relists; the cache never goes silently stale |
| **Blind when ArgoCD is down** | Yes, completely | No: ArgoCD's pods and Deployments are watched too |

**Why ArgoCD's tree lags on status-only changes [inferred].** The argo-cd chart sets `resource.customizations.ignoreResourceUpdates.all: /status` ([chart 10.9.2 `values.yaml` L305–313](https://github.com/argoproj/argo-helm/blob/argo-cd-10.9.2/charts/argo-cd/values.yaml#L305-L313)). "When a resource update is ignored, if the resource's health status does not change, the Application … will not be reconciled" ([reconcile.md](https://github.com/argoproj/argo-cd/blob/v3.5.3/docs/operator-manual/reconcile.md)). So a pod's restart count ticking up while its health stays Healthy updates ArgoCD's tree only at the next periodic refresh.

**When ArgoCD sees a new commit.** `timeout.reconciliation: 120s` plus `timeout.reconciliation.jitter: 60s` ([chart `values.yaml` L213–218](https://github.com/argoproj/argo-helm/blob/argo-cd-10.9.2/charts/argo-cd/values.yaml#L213-L218), [`argocd-cm.yaml` L330–348](https://github.com/argoproj/argo-cd/blob/v3.5.3/docs/operator-manual/argocd-cm.yaml#L330-L348)). The webhook page still says "every three minutes" ([webhook.md L5](https://github.com/argoproj/argo-cd/blob/v3.5.3/docs/operator-manual/webhook.md)). The configured default is what applies, so between 2 and 3 minutes.

**What neither source can see: DNS.** external-dns here watches Ingresses (`sources: [ingress]`, `bootstrap/templates/external-dns.yaml`). It writes no status to them and has no CRD in this setup. Its metrics are aggregate: `last_sync_timestamp_seconds`, `endpoints_total` ([external-dns v0.22.0 metrics](https://github.com/kubernetes-sigs/external-dns/blob/v0.22.0/docs/monitoring/metrics.md)). They don't say whether one host's record exists. Argus could show "DNS: external-dns last synced N s ago" for the Platform, or resolve each custom domain itself, but it cannot read a per-host DNS state from the cluster.

## What the Deploy gate knows that neither source does

The gate is the first Platform component to learn about a Deploy or Promote. It learns synchronously, within the CI job (`internal/deploygate/gate.go`):

- **That a Deploy was asked for, and by whom.** The Application, the requested and resolved Environment, and the tag. Deploy versus Promote (`refs/heads/main` versus a `v*` tag). The calling repository and its id, the source commit SHA, the actor, the workflow ref, and when it happened (`serveDeploy`, `deploy`).
- **Refusals, and why.** Wrong org or ref, unbound Application, unknown Environment, an image tag missing from GHCR (422), a registry it couldn't check (503), and so on. None of these produce a commit, so neither git nor ArgoCD ever shows them. A developer whose Deploy was refused sees nothing change in either source.
- **The Platform repository commit it produced** (`Response.Commit`). It is the join key to ArgoCD: that SHA appears in the Environment's `status.sync.revisions` and `operationState.syncResult.revisions` at the Platform repository source's index, the second source (`application.yaml`). It is also the key for the Deploy → sync → rollout flow in #97's "Not yet specified".
- **No-ops and migration changes.** `unchanged` (the tag was already running, no commit) and `migrationCommandChanged`.
- **Queueing.** Concurrent Deploys wait on `writeMu`.
- **The 0–3 minutes before ArgoCD polls.** Between the gate's push and ArgoCD's next poll, only the gate knows a Deploy is under way.

**Where that knowledge is today.** One JSON log line per call on stdout: `"deployed"` or `"deploy refused"`, with the attributes above (`gate.go` L206–235). Alloy ships it to Grafana Cloud, off the node. For successes it is also in the Platform repository commit and its body. The gate keeps nothing in memory between calls.

**Cheap ways the gate could expose it** (not a design):
- **(a)** An in-memory ring buffer of recent calls, served read-only on the cluster-internal Service, as a `GET` or an SSE stream. It is lost on restart, which matches "no new datastore". ADR-0007 already adds a read endpoint to the gate's service.
- **(b)** A Kubernetes Event per call. That needs a ServiceAccount token and `create` on events, which the gate deliberately doesn't have.
- **(c)** Argus reads the Platform repository's commits by `iidp-deploy[bot]`. That catches successes only, and needs GitHub polling.

Two things the gate never sees: Preview Environments, which ArgoCD's ApplicationSet creates (ADR-0006 on `adriansberg/phase-2-design`), and anything the CLI writes directly.

## Which source marks each step of a Deploy

This is for #100 (state definitions) and the Deploy-flow item on #97. It lists signals, not states.

| Step | Earliest source | Signal |
|---|---|---|
| Deploy or Promote requested; refused | Deploy gate | The call and its outcome; the commit SHA |
| Platform repository changed | Deploy gate (immediately); ArgoCD (after its poll, 2–3 min) | `sync.status: OutOfSync`, then `operationState.phase: Running` with `syncResult.revisions` = the gate's commit |
| Migration hook runs, fails | Job object (B); `operationState.syncResult.resources[].hookPhase` (A and B) | Job `active` / `Failed`. ArgoCD retries the sync with backoff up to 3 min (`limit: -1`) |
| Rollout | Deployment and Pods (B); ArgoCD tree (A) | `updatedReplicas` / `readyReplicas`; `Progressing=False, ProgressDeadlineExceeded` after 600 s; pod `CrashLoopBackOff` |
| Done | Application CR | `operationState.phase: Succeeded`, `health.status: Healthy`, `summary.images` = new tag |
| Environment Arriving | Application CR created; namespace created | `ADDED` Application with no history |
| Environment Leaving | Application CR | `metadata.deletionTimestamp`; final-backup Job appears (B); `DeletionError` condition if it fails |

## Recommendation

> **Recommendation.** Build Argus's backend on **Kubernetes watches (Option B)**. Use the dynamic client, label selectors, and a transform func on every informer. Treat the ArgoCD `Application` CR as the source for sync, operations, revisions and history. Watch the Environments' own Deployments, Pods, Jobs, CNPG Clusters and Certificates for health. Add `type=Warning` Events plus ArgoCD's own Application Events for the feed. Add `kube-system` Deployments and the Node for Platform infrastructure. Take the pre-ArgoCD stage of a Deploy, and refused Deploys, from a small in-memory "recent Deploys" read feed on the Deploy gate. Don't depend on the ArgoCD API.

Why:

1. **No hand-made credential.** The ArgoCD API needs a token minted imperatively into `argocd-secret`, at least `role:readonly`, which includes all pod logs. A ServiceAccount and a read-only ClusterRole are declared in the bootstrap like everything else, and can be turned off with the same value that turns Argus off.
2. **No blind spots when ArgoCD is the problem.** Argus should show ArgoCD Degraded when ArgoCD is degraded. With Option A, Argus goes dark at the moment the Platform is least healthy. Option B also covers Traefik, the node, and k3s's own parts, which ArgoCD doesn't manage.
3. **Latency is equal or better.** The CR is the same object either way. For pods and rollouts, a direct watch beats ArgoCD's refresh, which skips status-only changes.
4. **Better failure semantics.** client-go resumes or relists. ArgoCD's stream can drop events for a slow reader and replays nothing on reconnect.
5. **It fits the budget, if the transforms are there:** about 4 MiB of caches for a 15-Application Platform, about 30 MiB process **[measured and estimated]**. Without transforms it doesn't fit.
6. **It shares ground with `iidp app status`.** ADR-0007 (Phase 2) has the Deploy gate's service read the same kinds with the same kind of read-only access. #101 should decide whether Argus and `app status` share one read model. This research doesn't.

Accepted costs:
- client-go and its dependency tree enter this repository (binary 4.3 → 25 MB).
- Argus must compute per-resource health itself for a handful of kinds. ArgoCD's Lua checks for CNPG and Certificate show the exact rules.
- DNS records stay invisible, with only external-dns's aggregate last-sync time available.

Where Option A would win: if Argus later needs ArgoCD's resource tree for arbitrary kinds, rather than the handful above. `GET …/resource-tree` on demand for the detail panel is a cheap addition, with a token, without streaming everything through ArgoCD.

## Unconfirmed, and where I looked

- **Memory on the node.** The figures come from a fake-client model on macOS arm64 with synthetic objects, not a real Argus on the k3s node (amd64). The real JSON sizes of our Applications and CNPG Clusters weren't sampled: I had no cluster access. Re-measure after the first build.
- **Option A's memory (~15–20 MiB)** is an estimate from the measured plain-Go baseline. It is not measured with real streams.
- **The tree lag for status-only changes** is inferred from `reconcile.md` and the chart's `ignoreResourceUpdates.all: /status`. It was not observed.
- **Whether `argocd --core` can mint account tokens,** or cloud-init could, was not checked. I looked at `docs/operator-manual/user-management/index.md` and `util/session/sessionmanager.go` at v3.5.3.
- **Which revision string ArgoCD records for the OCI chart source** (version or digest) was not checked. The Platform repository source's commit SHA at index 1 is what the gate correlates with.
- **Preview Environments' Application names and labels** are not designed yet (ADR-0006, unmerged). Whether they carry `iidp.itema.no/*` labels decides whether the same selectors find them.
- **Watch latency "near-immediate"** is general Kubernetes behaviour. I did not measure it on k3s with its SQLite/kine datastore.

## Sources

Repository: `CONTEXT.md`; `docs/adr/0004-lean-platform-stack.md`; `docs/platform-repository.md`; `docs/design.md`; `bootstrap/versions.yaml`, `bootstrap/values.yaml`, `bootstrap/templates/{argocd,deploy-gate,external-dns}.yaml`, `bootstrap/components/deploy-gate/`; `chart/application/templates/`; `internal/deploygate/gate.go`, `cmd/iidp-deploy-gate/main.go`; `docs/implementation-notes/{04,39,60,75}-*.md`. ADR-0006 and ADR-0007 are on `adriansberg/phase-2-design` (unmerged at the time of writing).

ArgoCD v3.5.3 (`github.com/argoproj/argo-cd/blob/v3.5.3/…`): `server/application/application.proto`, `server/application/application.go`, `server/broadcast/broadcaster.go`, `pkg/apiclient/application/forwarder_overwrite.go`, `pkg/apis/application/v1alpha1/types.go`, `util/cache/redis.go`, `util/cache/appstate/cache.go`, `util/session/sessionmanager.go`, `util/argo/audit_logger.go`, `controller/cache/info.go`, `assets/builtin-policy.csv`, `resource_customizations/{postgresql.cnpg.io/Cluster,cert-manager.io/Certificate}/health.lua`, `ui/src/app/shared/services/applications-service.ts`, and `docs/operator-manual/{user-management/index.md,rbac.md,argocd-cm.yaml,argocd-cmd-params-cm.yaml,reconcile.md,webhook.md,upgrading/2.14-3.0.md}`, `docs/developer-guide/api-docs.md`. argoproj/pkg v2.0.1 `grpc/http/forwarders.go`. argo-helm `argo-cd-10.9.2` `charts/argo-cd/{Chart,values}.yaml`.

Kubernetes: client-go v0.36.1 `tools/cache/{shared_informer,delta_fifo,reflector}.go`, `features/known_features.go`, `informers/factory.go`, `metadata/metadatainformer/informer.go`. kubernetes v1.36.4 `pkg/controlplane/apiserver/options/options.go`, `pkg/registry/{core/event,core/pod,batch/job}/strategy.go`, `staging/src/k8s.io/apiserver/pkg/features/kube_features.go`. k8s.io/api v0.36.1 `events/v1/types.go`. k3s v1.36.4+k3s1 `pkg/daemons/control/server.go`.

Others: CloudNativePG v1.30.0 `api/v1/cluster_types.go` (charts `cloudnative-pg-v0.29.0` `Chart.yaml` for the version). cert-manager v1.21.2 `pkg/controller/certificate-shim/sync.go`. external-dns v0.22.0 `docs/monitoring/metrics.md`. The [Go GC guide](https://go.dev/doc/gc-guide).
