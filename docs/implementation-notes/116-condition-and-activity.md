# #116 Condition and Activity in `platformstate`, and in `iidp app status`

This note records the decisions taken while building the interpretation that Argus and `iidp app status` share. The rules are the state decision's ([#100](https://github.com/Itema-as/iidp/issues/100), including its mapping from ArgoCD) and the Deploy-tracing decision's ([#106](https://github.com/Itema-as/iidp/issues/106), with its correction that the gate's Events live in `argocd`). This note names each place where a rule had to be read more precisely than those decisions say, and why. The command's output and `--json` shape are in the README ("`app status`").

Sources, checked on 2026-09-27:

- ArgoCD (Context7, `argoproj/argo-cd`, `pkg/apis/application/v1alpha1/types.go`): `ApplicationStatus` has `sync` (`status`, `revision`, `revisions`), `reconciledAt`, `operationState` (`operation.sync.revision(s)`, `syncResult.revision(s)`), `history` (`RevisionHistory`: `id`, `revision`, `revisions`, `deployedAt`), and `conditions` (`type`, `message`, `lastTransitionTime`). A multi-source Application, as every Environment is, reports `revisions`, one per source.
- CloudNativePG (Context7, `cloudnative-pg/cloudnative-pg`): `Cluster.status.conditions` has `Ready`, `LastBackupSucceeded` and `ContinuousArchiving`, each `False` with a reason and message when failing; the phase constants in `api/v1/cluster_types.go`, including `Setting up primary` for a starting Cluster.
- cert-manager (Context7, `cert-manager/website`, API reference): `CertificateStatus` has `conditions` (`Ready`, `Issuing`), `notAfter`, and `lastFailureTime`, which is set only while the latest issuance has failed.
- Kubernetes: `events.k8s.io/v1` Event (`reason`, `note`, `type`, `eventTime`, `deprecatedLastTimestamp`, `regarding`); a Deployment's `Progressing` condition with reasons `NewReplicaSetAvailable`, `ReplicaSetUpdated` and `ProgressDeadlineExceeded`; a container's `state.waiting.reason` (`CrashLoopBackOff`, `ErrImagePull`, `ImagePullBackOff`) and `lastState.terminated.reason` (`OOMKilled`); a pod's `PodScheduled` condition with reason `Unschedulable`.

## A pure layer on the structs the package already has

`internal/platformstate` gains three files and no I/O:

- `condition.go`: `Condition` (`Healthy`, `Degraded`, `Unknown`, plus a `Warning` only Capabilities set) and `Activity` (`Arriving`, `Unreleased`, `Deploying`, `Updating`, `Leaving`, each with `stuck` and a `reason`), and the rules for an Environment.
- `deploy.go`: `Deploy` and its five hops.
- `capability.go`: `Capability`, `ComponentObjects`/`Component` for Platform components, and `Application`/`EnvironmentState`, which put an Application's Environments, their Capabilities and their Deploys together for Argus.

Every function takes the objects and `now`, and reads nothing. `EnvironmentOf(application, objects, now)` now fills `condition` and `activity`, so `Read`, the List path the Deploy gate's service uses, passes `time.Now()` at the edge and nothing else changes for its caller. The timeouts are constants (`NotReadyGrace`, `UnreleasedAfter`, `OutOfSyncStuckAfter`, `CertificateWarningWithin`, `WatchLostAfter`), and the tests move the clock, never sleep.

The new cut-down structs follow the existing convention, the API's own JSON names: `PostgresCluster`, `Certificate` and `Event`, and more of `ArgoCDApplication` (the sync revisions, `reconciledAt`, the operation's revisions, `history`, `conditions`, and a Preview Environment's `helm.valuesObject.image.tag`), `Deployment` (`replicas`, `generation`, status and conditions), `Pod` (containers' images, states and last states, conditions' reasons and times) and `Job` (its image). `ObjectMeta` gains `annotations`, `deletionTimestamp` and `generation`. `Objects` gains the Clusters, Certificates and Events, and `OutOfSyncSince`, which a watch knows and a List does not.

## Condition: read as "are users short of anything?"

#100 says Degraded is "not ready for more than 60 s, or immediately on CrashLoopBackOff, OOMKilled or Unschedulable", and that Degraded is about users while stuck is about a change not landing. Read literally, a new version's pod that crash-loops during a one-replica rollout would make the Environment Degraded, while the old pod still serves. The mapping table says the opposite ("`Degraded` from `ProgressDeadlineExceeded`, with the old pod serving: Healthy + stuck"). So:

- **The Environment is judged on its serving capacity**: the Deployment's pods that are ready, against `spec.replicas`. While enough are ready, whatever version they run, it is Healthy, and a failing new pod shows as a stuck Deploy instead.
- **Short of ready pods, CrashLoopBackOff, OOMKilled and Unschedulable make it Degraded at once.** OOMKilled counts only while that container is not ready again. A pod that recovered is Healthy at once, as #100 says, and its restarts stay in the `pods` count and the feed.
- **Otherwise it is Degraded once the shortfall is older than 60 s.** The shortfall starts at the earliest not-ready pod's `Ready` transition (or its creation, if it never was ready), or, with too few pods at all, at the Deployment's `Available: False` transition.
- The mapping row "`Degraded` from crash-looping pods that are serving: Degraded, after the 60 s grace" is read together with "CrashLoopBackOff at once". Between crashes, before the kubelet reports CrashLoopBackOff, the pod is simply not ready and the grace applies. Once it is in CrashLoopBackOff, it is at once.
- **While an Environment is Arriving, the 60 s rule does not apply**, because nothing has served yet and no user is short of anything. A first image that takes two minutes to pull is not a Degraded Environment. The three immediate causes still apply.
- **Unknown** is a pod in phase `Unknown` or with `Ready: Unknown` (a lost node), or ArgoCD's health `Unknown` when the pods say nothing worse. Degraded wins over Unknown: a known failure says more than fog.
- An Environment without a Deployment (no image yet), or with `replicas: 0`, is Healthy.

"Has served" is not recorded anywhere, so it is read from the Deployment: `Progressing` with reason `NewReplicaSetAvailable` (a rollout completed, which stays until the next one starts), enough pods ready now, or pods of an earlier version still there during a rollout.

## Activity, and stuck

The order is Leaving, then Arriving or Unreleased, then Deploying, then Updating.

- **Leaving** is the ArgoCD Application's `deletionTimestamp`. It is stuck on a `DeletionError` condition or a failed `final-backup` Job. Since #132, also on a `final-backup` Pod that is `Unschedulable`, and after 45 minutes ([132-stuck-states.md](132-stuck-states.md)).
- **Arriving** runs until the workload has served. With no Deployment, it is Unreleased 30 minutes after the ArgoCD Application's creation. A Deploy into an Unreleased Environment makes it Arriving again, with the Deploy attached. Since #132, an arrival with a Deploy or a Deployment on its way is stuck after 15 minutes with no step forward.
- **Deploying** is a Deploy under way (below).
- **Updating** is anything else changing: ArgoCD `OutOfSync`, a sync running, or the Deployment mid-rollout.
- **A stuck signal with no Activity is Updating, stuck.** Something that is not landing is a change, even when ArgoCD shows nothing in progress, as after a failed sync of a Capability.

The stuck rules are #100's. Four points had to be pinned down:

- **"OutOfSync with no operation for 5 minutes": since when?** ArgoCD does not record when an Application turned OutOfSync. A watch does, and passes it as `OutOfSyncSince`. Without it, the start is the later of the ArgoCD Application's creation and its last operation's end, or, for a Deploy waiting for ArgoCD, its acceptance. `OutOfSyncSince` is used when it is later than that. This errs towards "not yet stuck".
- **A Deploy is stuck only by the rules of the hop it is at.** An older failed sync, of a commit before the Deploy's, does not make a newer Deploy stuck at Waiting for ArgoCD. ArgoCD tries each new commit afresh.
- **A failed migration** is the newest migration Job, `Failed`. For a Deploy it counts only when that Job ran the Deploy's tag. Since #132, so does a newest migration Job whose Pod is `Unschedulable`.
- **A failing image pull** is `ErrImagePull`, `ImagePullBackOff` or `InvalidImageName` on one of the workload's pods. For a Deploy, only on a pod of its tag.

## Where a Deploy is

**Accepted against Waiting for ArgoCD.** #106 gives both hops the same moment: the gate records `DeployAccepted` after its commit, so "the commit exists, but ArgoCD hasn't synced it yet" holds from the first instant. To make the two distinct, and each able to be stuck, the signal between them is whether ArgoCD has looked at the Platform repository since:

- **Accepted**: ArgoCD's `sync.revisions` do not name the Deploy's commit, and its `reconciledAt` is before the Deploy. It is stuck on a `ComparisonError`.
- **Waiting for ArgoCD**: ArgoCD has looked since, but has not started syncing the commit. It is stuck on a `ComparisonError`, or when OutOfSync with no sync for over 5 minutes since the Deploy.

With [#114](https://github.com/Itema-as/iidp/issues/114), the gate's refresh makes ArgoCD look within seconds, so Accepted is a moment and Waiting is ArgoCD's queue. Without it, the 3-minute polling wait shows as Accepted, not as Waiting as #106 pictured. Both are true of that wait. The one sure difference is whether ArgoCD has seen the commit.

The other hops:

- **Applying**: ArgoCD's operation is on the Deploy's commit and running, or a migration Job with the Deploy's tag has not succeeded. It is stuck on that operation failing, or on the migration failing.
- **Rolling out**: ArgoCD has synced the commit (it is in `history`, the last sync of it succeeded, or ArgoCD reports itself Synced to it), or the Deployment runs the tag, and not yet enough pods of the tag are ready. It is stuck on an image that cannot be pulled or `ProgressDeadlineExceeded`.
- **Serving**: as many ready pods run the tag as the Deployment wants. Deploying ends here.

A short commit (7 or more characters) matches the full one it abbreviates.

**Superseded.** For each Deploy the gate accepted before the newest, if ArgoCD synced its commit (as for Rolling out above) it landed, and is marked Serving. Otherwise ArgoCD skipped to a later commit, and it is superseded by the next Deploy's tag, not stuck. From one look at the cluster, a Deploy that reached ArgoCD but whose rollout the next one interrupted cannot be told from one that finished; both show as landed. The newest Deploy is superseded when ArgoCD synced it and the Deployment has since run another tag, which is a Deploy whose Event was missed.

**Refused.** `DeployRefused` gives a Deploy with `refused` and the reason, taken from the Event's note after "refused: ". It has no hop and never makes the Environment Deploying. It also does not hide a Deploy already under way.

**Joined by commit, then by tag.** The newest accepted Deploy is placed by its commit first. When ArgoCD no longer names that commit (it synced a later one together with it, or the history of 10 dropped it), the image tag places it: a migration Job running the tag means Applying, and the Deployment running it means Rolling out or Serving.

**A Deploy with no Event** (missed, expired, or never recorded) is found from the cluster alone. Either a migration Job runs a tag the Deployment does not run yet (Applying), or the Deployment runs a tag that is not yet serving while pods of another tag are still there (Rolling out), or it is the first rollout. The ticket says such a Deploy "starts at Waiting for ArgoCD or Applying". In practice it starts at **Applying or Rolling out**: ArgoCD's status names no image for a commit it has not synced, so nothing short of the Event or the Platform repository says that a pending commit is a Deploy and not some other change. A change that keeps the tag (a size, a restart) is Updating, not a Deploy.

**A Preview Environment's Deploy** is found from the tag its ApplicationSet sets in the ArgoCD Application's `helm.valuesObject.image.tag` (#95). When the Deployment does not run that tag yet, it is at Applying, whether or not ArgoCD has started. It never passes the gate, and #106 draws it from Applying on. The Environment is a preview when its name is `pr-<number>`.

**A Promote** is the gate's Event with `iidp.itema.no/kind: promote`, and travels the same hops, marked `promote`.

The Event's shape is #117's: reason `DeployAccepted` or `DeployRefused`, `regarding` the Environment's ArgoCD Application in `argocd`, and the annotations `iidp.itema.no/application`, `environment`, `tag`, `commit` (accepted only), `kind` and `refusal`. An Event counts for an Environment if it regards its ArgoCD Application, or if its application and environment annotations name it. Its time is `eventTime`, else `deprecatedLastTimestamp`, else its creation. The constants live in `deploy.go`, and the tests build Events of that shape. When #117 lands, the gate should use the same names, and a test on either side will show a mismatch.

## Capabilities and Platform components

A Capability's Condition is its own and never rolls up into its Environment's (`TestFailingBackupIsAWarningNotDegraded`).

- **Postgres** (the CNPG Cluster): its `Ready` condition, with the same 60 s grace. It is Arriving while CNPG's phase is `Setting up primary`. The Warning is `LastBackupSucceeded: False` ("backups are failing"), else `ContinuousArchiving: False` ("WAL archiving is failing"), with CNPG's message.
- **Custom domain** (its cert-manager Certificate, named by its first DNS name): Arriving until the first certificate is issued (`notAfter` unset), and stuck while `lastFailureTime` is set. After that, its `Ready` condition with the grace, so an expired certificate is Degraded. The Warning is `notAfter` within 14 days. cert-manager renews 30 days ahead, so this means renewal is failing.
- **Scheduled task**: Healthy, and Leaving while its CronJob is being deleted. #100 gives no rule for a Scheduled task, so a failed run is not made Degraded or a Warning here. Its last run stays in `tasks`. **Open question.** Answered by #118: a failed last run is a Warning ([118-argus-backend.md](118-argus-backend.md#additions-to-platformstate)).
- **Itema login is not modelled.** With sign-in groups it is a Middleware of the Environment's own, but without them it is only an annotation on the Ingress, pointing at the shared ForwardAuth. Argus's read list (#115) has no Ingresses, so nothing Argus reads marks it in every case. It has no state of its own to judge either: the ForwardAuth service is a Platform component. **Open question** for the Argus backend ticket: read Ingress annotations, or take it from ArgoCD's `status.resources`. Answered by #118: from the Ingresses' annotation ([118-argus-backend.md](118-argus-backend.md#additions-to-platformstate)).
- A Capability's Activity is only Arriving or Leaving. Adding or removing one is its Environment's Updating (#100).

**Platform components** (`ComponentOf`): each Deployment's pods are judged against its replicas, as for an Environment. Pods no Deployment selects, such as ArgoCD's application controller StatefulSet, are judged one by one. One ArgoCD manages is Updating when OutOfSync, syncing or rolling out, and stuck by the same rules. One ArgoCD does not manage (Traefik, k3s's own parts) has a Condition only.

**An Application** has no Condition or Activity of its own. #100 defines them for Environments, Capabilities and Platform components, and says nothing of rolling Environments up.

`WatchLost(brokenSince, now)` is #100's "Argus's watch has been broken for 30 s", for Argus to draw the fog.

## `iidp app status`

- The block gains `Condition:` and `Activity:` lines above `Status:`, such as `Deploying 1.0.2 (Promote), rolling out` or `Deploying sha-2, stuck at Applying: the migration failed`. An answer from a gate older than this has neither field, and prints as before.
- `--json` gains `condition` and `activity`. Nothing is renamed. `condition` is absent for an Environment ArgoCD has not picked up. `activity` is `null` when nothing is changing, the same as `migration`.
- **The gate's reads are unchanged**, and so is its RBAC. It lists no Events, CNPG Clusters or Certificates, so `iidp app status` shows no Capabilities and finds Deploys by tag, from Applying on. Listing Events in `argocd` means a new grant on the gate's service account, in the same RBAC file #117 and #114 change. That is left for after them: a Role with `list` on `events` in `argocd`, and `Read` listing them by field selector on the ArgoCD Application's name. The status service could also pass the tag the Platform repository wants, which it reads already for `deployedAt`, and so show Waiting for ArgoCD without Events.

## Glossary

Nothing is added to `CONTEXT.md`. Condition, Activity, Arriving, Unreleased, Stuck, Updating and Leaving are Argus's UI vocabulary (#100, #115). The README uses them to describe the command's output, not as Platform language.

## Tests

- `condition_test.go`: `TestArgoCDMappingTable` has one or more cases for every row of #100's mapping, each as the objects that make it. `TestStuckRules` covers every stuck rule, at once or after its timeout, on both sides of it and with a watch's `OutOfSyncSince`. `TestConditionRules` covers Degraded and Unknown, the grace on both sides, and a recovered pod. Also tested: a slow first start while Arriving, Degraded and stuck together, and `WatchLost`.
- `deploy_test.go`: each hop, and stuck at each hop but Serving; joined by commit (abbreviated too), by tag after ArgoCD moved on, and by tag without an Event (a migration, a rollout), and a same-tag change that is Updating; superseded; refused, alone and next to a Deploy under way; a Promote; a Preview Environment's Deploy from Applying, rolling out, serving, arriving and stuck; a first Deploy into an Unreleased Environment; and other Environments' Events ignored.
- `capability_test.go`: the backup and archiving Warnings with the Environment Healthy; Postgres ready, within the grace, Degraded, Unknown and being created; a certificate valid, expiring, expired, not yet issued and failing to issue; Capabilities' order; Platform components with and without ArgoCD; and decoding each new struct from the API's JSON.
- `kube_test.go`: `Read` fills Condition and Activity from what it lists.
- `internal/cli/app_status_test.go`, through `cli.RunWith` against the fake gate: the exact output with Condition and Activity (a Promote rolling out, a stuck Deploy, Unreleased, Updating stuck, Arriving waiting, Leaving stuck, a preview stuck at Rolling out), an Environment from an older gate, and `--json` with the new keys.

The fixtures are Kubernetes objects as the API server writes them, decoded into the structs. That also checks the JSON names. All times are relative to one fixed `now`.
