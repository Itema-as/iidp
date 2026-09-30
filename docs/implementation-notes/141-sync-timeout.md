# #141 ArgoCD ends a sync after an hour

A gap left by #131. #139 bounds a Preview Environment's sync when its migration hook can't run, but a sync also waits for every object of a wave to be healthy. A CNPG `Cluster` whose instance Pod never starts would keep the sync running for ever, and ArgoCD deletes no Application while its operation runs, so closing the pull request would leave the preview in place (`131-preview-sync-ends.md`, "Not covered"). Not seen live.

Decided in the Phase 3 grilling (2026-09-30): the `argocd` Application sets `configs.params."controller.sync.timeout.seconds": 3600` (`bootstrap/templates/argocd.yaml`), commented with why and with the assumption that nothing legitimate syncs for an hour: the migration Job has a 30-minute deadline (#139), and database restores are small. The README's paragraph on stuck Activities gives `argocd app terminate-op` as the way out of anything this doesn't cover, and `bootstrap/README.md` says the shared sync policy's unlimited retries now last an hour.

## What the chart and ArgoCD do

Checked on 2026-09-30 against the argo-cd chart **10.9.2** that `bootstrap/versions.yaml` pins, and ArgoCD **v3.5.3**, its appVersion. The chart was pulled with `helm pull argo-cd --repo https://argoproj.github.io/argo-helm --version 10.9.2`; ArgoCD's source was read at the tag.

- **The chart passes the value to the controller.** Every key of `configs.params` goes into the ConfigMap `argocd-cmd-params-cm` as a string (`templates/_helpers.tpl` L265, `argo-cd.config.params`, `toString`). The application controller's StatefulSet reads `controller.sync.timeout.seconds` from it into `ARGOCD_APPLICATION_CONTROLLER_SYNC_TIMEOUT` (`templates/argocd-application-controller/statefulset.yaml` L201–206), and its pod template carries a `checksum/cmd-params` of the ConfigMap's data (L28), so changing the value rolls the controller. `helm template` of the chart with and without the value, `fullnameOverride: argocd`, differs only in `controller.sync.timeout.seconds: "3600"` in the ConfigMap and in the `checksum/cmd-params` annotations. The key is also in upstream's documented `argocd-cmd-params-cm` ([`docs/operator-manual/argocd-cmd-params-cm.yaml` L77](https://github.com/argoproj/argo-cd/blob/v3.5.3/docs/operator-manual/argocd-cmd-params-cm.yaml#L77)): "Specifies a sync timeout for applications. "0" means no timeout".
- **The controller reads it as seconds.** `--sync-timeout` defaults to `ARGOCD_APPLICATION_CONTROLLER_SYNC_TIMEOUT`, parsed as a number from 0 to `math.MaxInt32` ([`cmd/argocd-application-controller/commands/argocd_application_controller.go` L280](https://github.com/argoproj/argo-cd/blob/v3.5.3/cmd/argocd-application-controller/commands/argocd_application_controller.go#L280)), and is passed on as `time.Duration(syncTimeout)*time.Second` (L206).
- **The hour runs from the operation's start, retries included.** When the controller processes a running operation and `time.Now().After(state.StartedAt.Add(ctrl.syncTimeout))`, it sets the phase to `Terminating` with "operation is terminating due to timeout" ([`controller/appcontroller.go` L1542](https://github.com/argoproj/argo-cd/blob/v3.5.3/controller/appcontroller.go#L1542)). A retry resets `FinishedAt`, `SyncResult` and, with `refresh`, the revision, but not `StartedAt` (L1566–1577). This answers #132's open question: a retry does not move `operationState.startedAt`.
- **It is checked soon after the hour.** A new operation schedules a check `syncTimeout` later (L1586–1589), and every refresh of the Application queues its operation again (L1782). The chart refreshes every 120 s plus up to 60 s of jitter (`timeout.reconciliation`), so a controller restart loses at most about three minutes.
- **A sync it ends is not retried.** After a terminated sync fails, the retry branch requires `!terminating` (L1620), so the operation completes as `Failed`, "…, triggered by controller sync timeout". With the operation completed, a deleted Application's deletion goes on (#131, L1134).
- **Automated sync starts again only on something new.** It skips an Application whose last sync failed at the same revisions and sources, with the condition "Failed last sync attempt to …" ([`alreadyAttemptedSync`, L2382](https://github.com/argoproj/argo-cd/blob/v3.5.3/controller/appcontroller.go#L2382)).
- **There is no timeout per Application.** `pkg/apis/application/v1alpha1/types.go` at the tag has no sync timeout field; `docs/proposals/sync-timeout.md` proposes one. Context7 (`/argoproj/argo-cd`, master) shows the same flag, the same ConfigMap key and `argocd app terminate-op`.

## Consequences

- **A Leaving Environment whose sync never ends is deleted** at most about an hour after that sync started, with nobody's cluster access. #132 shows it `Leaving … stuck` after 45 minutes, with the reason "ArgoCD's sync operation is still running", which can come before the timeout.
- **A preview whose migration Pod never starts** (#131) now ends in the second attempt, at one hour, instead of after four attempts, about two hours.
- **Staging, prod and the Platform's own Applications.** The issue expected a sync ended there to start again under their unlimited retry. It does not: ArgoCD does not retry an operation it ended, and the hour covers the retries. The Application syncs again when its revisions or sources change, or when someone syncs it by hand (`argocd app sync argocd/<name> --core`).
  - An Environment's Application takes its values from the Platform repository (a `ref` source), and each source's resolved revision is in `syncStatus.revisions` (`controller/state.go` L858–861, L1044). So the Platform repository's next commit, a Deploy included, should start a new sync. Read, not tried on a cluster.
  - A Platform component whose chart or path is pinned syncs again only when a bootstrap release changes it, or by hand.
  - So a sync that keeps failing for over an hour for a cause outside Git (an admission webhook that is down, a node with no room) no longer recovers by itself when the cause goes away. Before, it retried every 3 minutes for ever. A failed sync is stuck at once in `iidp app status` and Argus (#116), so it shows.
- **A staging or prod migration Pod that can't start** fails at 30 minutes, is retried, and the retry is ended at one hour. The Environment then stays failed until the next commit, instead of a new Pending Pod every half hour.
- **The final backup is not a sync.** It is a `PreDelete` hook, which ArgoCD runs while finalizing the deletion (`controller/appcontroller.go` L1322), after any operation has ended, so this timeout does not cut it short. `postgres.finalBackupTimeout` bounds it.
- The Platform gets this with the bootstrap release that has it. The first sync of the `argocd` Application then rolls the application controller; an operation that was running is resumed by the new one.

## Not covered

- **A sync that legitimately needs over an hour**, such as a restore of a large database. Nothing on the Platform does today. If one appears, the value is a small change, but it holds for every sync.
- **A deletion held up by something other than a sync**: a `DeletionError`, a finalizer, a final backup that never ends. `terminate-op` doesn't help there either.
- **Not reproduced on a cluster, and no kind check.** Showing the controller end a sync would take an hour-long sync, or a different value in the harness than on the Platform. The render test pins the value; the chart's wiring was checked with `helm template` as above.

## Proof

- `bootstrap/bootstrap_test.go`: `TestArgoCDEndsASyncAfterAnHour` renders the bootstrap and asserts `configs.params."controller.sync.timeout.seconds"` is 3600 in the `argocd` Application's values. It fails with the value set to 0.
- `helm template` of argo-cd 10.9.2 with and without the value, as above.
