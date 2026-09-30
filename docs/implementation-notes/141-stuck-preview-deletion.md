# #141 A preview whose sync never ends is not deleted

A gap left by #131. #139 bounds a Preview Environment's sync when its migration hook can't run, but a sync also waits for every object of a wave to be healthy. A CNPG `Cluster` whose instance Pod never starts would keep the sync running for ever, and ArgoCD deletes no Application while its operation runs, so closing the pull request would leave the preview in place (`131-preview-sync-ends.md`, "Not covered"). Not seen live.

The change is docs only. The README, next to the stuck rules, gives `argocd app terminate-op argocd/<name> --core` as the way out for an Environment stuck `Leaving` on a sync that never ends. #141 stays open until the case is seen live.

## Considered: ArgoCD's sync timeout, for the whole Platform

The Phase 3 grilling (2026-09-30) first chose `controller.sync.timeout.seconds: 3600` in the `argocd` Application's `configs.params`. ArgoCD has no sync timeout per Application, only this controller-wide one. The per-Application one is a proposal (`docs/proposals/sync-timeout.md`). The expectation was that previews would be deleted, while staging and prod would start a terminated sync again under their unlimited retry. The second half does not hold, so the maintainer rejected it.

Checked on 2026-09-30 against the argo-cd chart **10.9.2** that `bootstrap/versions.yaml` pins, and ArgoCD **v3.5.3**, its appVersion. Read at the tag:

- **The chart would honour it.** Every key of `configs.params` goes into `argocd-cmd-params-cm` (`templates/_helpers.tpl` L265). The application controller's StatefulSet reads `controller.sync.timeout.seconds` into `ARGOCD_APPLICATION_CONTROLLER_SYNC_TIMEOUT` (`templates/argocd-application-controller/statefulset.yaml` L201–206), which the controller parses as seconds ([`cmd/argocd-application-controller/commands/argocd_application_controller.go` L280](https://github.com/argoproj/argo-cd/blob/v3.5.3/cmd/argocd-application-controller/commands/argocd_application_controller.go#L280)).
- **The hour counts the retries.** The controller terminates an operation once `time.Now().After(state.StartedAt.Add(ctrl.syncTimeout))` ([`controller/appcontroller.go` L1542](https://github.com/argoproj/argo-cd/blob/v3.5.3/controller/appcontroller.go#L1542)). A retry resets `FinishedAt`, `SyncResult` and, with `refresh`, the revision, but not `StartedAt` (L1566–1577). `SyncResult` is set again at the start of each attempt ([`controller/sync.go` L119–120](https://github.com/argoproj/argo-cd/blob/v3.5.3/controller/sync.go#L119)). So a sync that keeps failing and retrying is also ended after an hour.
- **A terminated operation is not retried.** The retry branch requires `!terminating` ([L1620](https://github.com/argoproj/argo-cd/blob/v3.5.3/controller/appcontroller.go#L1620)), so the operation completes as `Failed`.
- **Automated sync then waits for a new revision.** It skips an Application whose last sync failed at the same revisions and sources, with the condition "Failed last sync attempt to …" ([`alreadyAttemptedSync`, L2379](https://github.com/argoproj/argo-cd/blob/v3.5.3/controller/appcontroller.go#L2379), L2382). Only a new revision, a change to the Application, or a sync by hand starts it again.

What that would cost:

- **Prod and staging** retry without limit so that they heal on their own (#75, `retry.refresh`). With the timeout, a sync failing for over an hour for a cause outside Git would stay failed until the Platform repository's next commit, or someone synced it by hand. The node running out of room is such a cause, as in #130 (`Insufficient cpu`). So would an admission webhook that is down.
- **Platform components whose chart or path is pinned** (`bootstrap/templates/`) get a new revision only from a bootstrap release. They would stay failed until one, or until someone synced them by hand.
- Only previews need the bound, so the timeout trades a cost on every Environment for a case not yet seen.

## #132's open question

`132-stuck-states.md` asked whether an ArgoCD retry moves `operationState.startedAt`. It does not: a retry keeps the operation state and resets only `FinishedAt`, `SyncResult` and, with `refresh`, the revision (`controller/appcontroller.go` L1566–1577). So a retry does not restart `ArrivingStuckAfter`'s clock: "ArgoCD starting its sync" is the first attempt's start.

## Still not covered

- **A preview whose sync never ends is not deleted** when its pull request closes. The known case is a database that never becomes healthy. #132 shows it `Leaving … stuck` after 45 minutes, with the reason "ArgoCD's sync operation is still running". Someone with the Platform's kubeconfig then runs `argocd app terminate-op argocd/<name> --core`, and ArgoCD finishes the deletion (#131). Its CPU bookings stay until then.
- The same holds for staging and prod, which a person deletes on purpose and can watch.

## If it's seen live

A targeted fix, to weigh then: **the Deploy gate terminates a Leaving preview's running operation after a while**, and leaves staging and prod alone. The gate already lists ArgoCD Applications (#94) and patches them in `argocd` (#114). `argocd app terminate-op` sets the Application's `status.operationState.phase` to `Terminating` ([`server/application/application.go` L2520](https://github.com/argoproj/argo-cd/blob/v3.5.3/server/application/application.go#L2520), `TerminateOperation`), which the gate could do too. The wait could follow `LeavingStuckAfter`. What to check then: the RBAC it needs, and whether a preview needs anything else to finish its deletion.
