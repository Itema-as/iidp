# #132 Stuck states that looked fine

Found during the Phase 2 rollout (#96), alongside #131. `internal/platformstate` read three stuck states as fine, and `iidp app status` and Argus both showed its reading:

- `hello-pr-2` showed `Condition: Healthy, Activity: Arriving, stuck: false` for over 20 minutes. Its sync was waiting on the migration hook Job, whose Pod was `PodScheduled=False`, reason `Unschedulable`: `0/1 nodes are available: 1 Insufficient cpu`.
- While its deletion hung (#131), it showed `Activity: Leaving, stuck: false`.
- That migration and a Scheduled task's run (`heartbeat`, in hello-staging and hello-prod), both with Pods Pending as Unschedulable, showed `"result": "running"`.

The fix is in the pure layer (`condition.go`, `deploy.go`, `capability.go`, `state.go`), so it reaches both readers. Everything is still a function of the objects and `now`.

## Seeing a Job's pods

**Question.** The rules need the pods of the migration, final backup and task Jobs. Do both readers have them?

- **Argus** has them already. Its `pods` informer selects `iidp.itema.no/application`, which the chart puts on every Job's pod template (`migration-job.yaml`, `cronjobs.yaml`, `final-backup-job.yaml`), and it keeps every label.
- **The Deploy gate's List path** did not. `Read` listed pods by the Deployment's `matchLabels`, and not at all before there is a Deployment, which is exactly `hello-pr-2`'s case. It now lists them by the application label, as Argus does. The pods count still takes only those the Deployment selects. The RBAC does not change: the gate already lists pods cluster-wide.

A pod belongs to a Job by the Job controller's label `batch.kubernetes.io/job-name`, or the older `job-name` it still sets. The cut-down `ObjectMeta` has no owner references or UID, and the label is enough: Jobs are matched within one namespace, and a pod of a deleted Job of the same name is garbage-collected with it.

## 1. An Unschedulable Pod of a hook Job or a task run

**The migration and the final backup: stuck at once.** Both are hooks ArgoCD waits for, with no deadline of their own (the migration Job has no `activeDeadlineSeconds`; the final backup's own timeout only starts once its Pod runs). An unschedulable Pod holds the Environment's change back for good. So the newest migration Job, not finished, with an `Unschedulable` Pod is a stuck rule next to "the migration failed", everywhere that one applies:

- a Deploy at Applying (by the gate's Event, by the migration's tag, or a Preview Environment's), when that Job ran the Deploy's tag;
- any other change (Updating), since the migration hook runs on every sync;
- and so an arrival, since the Activity is Arriving with the Deploy attached.

The reason is `the migration's Pod <name> is Unschedulable: <the scheduler's message>`. The newest `final-backup` Job's Pod makes Leaving stuck the same way: `the final backup's Pod <name> is Unschedulable: …`.

**No grace.** An Unschedulable Pod of the Deployment is Degraded at once (#100, `failingAtOnce`), and this follows it. On one node with no autoscaler, a Pod is only scheduled later if something else frees room. The scheduler retries as soon as a Pod is deleted, so a Pod that waits for another to finish terminating is scheduled within seconds, and a stuck flag that clears again says what happened. A grace would hide the case #132 is about for its whole length.

**A Scheduled task's run: a Warning on the task, not a stuck Environment.** A task's run does not block the Environment's change or its serving, which is why a failed run is a Warning and not Degraded (#118). A run whose Pod cannot be scheduled is the same kind of risk: the work is not being done. So the task Capability gets the Warning `the last run's Pod <name> is Unschedulable: …`, for its newest run, the one `tasks[].lastRun` shows. The Environment's Condition and Activity do not change. The chart's `activeDeadlineSeconds: 3600` fails such a run after an hour, and the Warning then becomes "the last run failed". Argus tags it `! TASK PENDING` instead of `! TASK FAILED`. `iidp app status` shows no Capabilities, so there it shows as `pending` (point 3).

## 2. Arriving and Leaving after a time limit

**`LeavingStuckAfter` = 45 minutes, from the `deletionTimestamp`.** Leaving includes the final backup. The chart gives it 30 minutes (`postgres.finalBackupTimeout`, 1800 s) before it fails, and a failed final backup is stuck at once. The other 15 minutes are for its Pod to start and for ArgoCD to delete the rest, which takes a minute or two. The reason says what is known: `Leaving for over 45 minutes; ArgoCD's sync operation is still running` (#131 saw ArgoCD finish the deletion only once the sync was terminated), or `…; the final backup has not finished`, or nothing more.

Options considered:

- **A shorter limit, not counting a running final backup.** The backup has its own timeout and fails loudly, so the rest could have 15 minutes. But the clock would then have to restart when the backup ends, or a deletion that backed up for 25 minutes would be stuck the moment the backup finished. Not chosen: one limit from the `deletionTimestamp` is simpler and never calls a real backup stuck.
- **A second, short rule for "Leaving with a sync running"**, which would have flagged #131 after minutes. Not chosen: it rests on ArgoCD's internals, and a deletion that begins during an ordinary sync is fine once the sync ends. The 45-minute rule catches #131, and its reason names the sync.

An Environment whose `finalBackupTimeout` is raised above 30 minutes shows as stuck while its backup is still running. The reason then says so.

**`ArrivingStuckAfter` = 15 minutes with no step forward.** Arriving can last a long time for good reasons: an Environment created with no image waits for its first Deploy. That case already has its own end, Unreleased after `UnreleasedAfter` (30 minutes), and it is not stuck. So the limit applies only to an arrival with something on its way, a Deploy or a Deployment, which are exactly the arrivals that never turn Unreleased. The two never meet: without a Deploy or a Deployment it is Arriving and then Unreleased, never stuck by time; with one, it is Arriving until it serves, stuck after the limit.

**Since when?** Measuring from the ArgoCD Application's creation would call a first Deploy into a long-Unreleased Environment stuck at once. So the clock runs from the latest step forward: the ArgoCD Application's creation, the gate accepting the Deploy (when its Event is seen), ArgoCD starting its sync, the newest migration Job's creation, and the Deployment's creation. As with OutOfSync, this errs towards "not yet stuck". **Not confirmed:** whether an ArgoCD retry moves `operationState.startedAt`. ArgoCD's `OperationState` counts retries in `retryCount` within one operation (Context7, `argoproj/argo-cd`, `types.go`), which suggests it does not, but its docs don't say. If it does, each retry restarts the clock. A retry follows a failed attempt, and the commonest one here, a failed migration, is stuck at once anyway.

**Why 15.** On a first arrival none of the waits between two steps takes more than a few minutes: ArgoCD picks a commit up within seconds of the gate's refresh (#114) or 3 minutes of polling, a new CNPG Cluster starts in a minute or two, and a first migration runs against an empty database. After the Deployment is created, Kubernetes' own 10-minute progress deadline fires first and says more precisely what is wrong. 15 minutes is well past all of these, and half of `UnreleasedAfter`.

The reason says what is known: `no progress for over 15 minutes; the migration's Pod has not started`, `…; the migration has not finished`, `…; 0 of 1 pods ready`, or `…; ArgoCD's sync is still running`. It says "no progress" rather than "Arriving for": the clock is the last step, not the start. The stuck flag and reason go on the Deploy too, in the Activity and in the list of Deploys, so Argus draws it frozen at its hop.

## 3. `pending`

A Run is `pending` while the Job has not finished and none of its pods has started: there is none yet, or each is still in phase `Pending`, waiting to be scheduled or for its image. It is `running` once one is past Pending, including a pod that has finished while the Job's condition is not yet set. `Complete` and `Failed` are unchanged. `startedAt` stays the Job's `startTime`, which the Job controller sets before any pod starts.

**Pending covers pulling the image too.** Kubernetes' own phase puts both under Pending, and neither is running the migration. A Pod that is Unschedulable is the case that matters, and it is also a stuck Environment (1) or a task Warning (1).

- `iidp app status` prints `last run pending since <startTime>`.
- `--json` gains the value `pending`. Nothing is renamed; a script that treated anything but `succeeded` and `failed` as running still does.
- Argus's card shows the result as it is (`pending 3 min ago`); its type gains the value.

## Left out

- **An Unschedulable Pod of a new version during a rollout.** The old version serves, so it is not Degraded, and the Deployment's progress deadline makes the Deploy stuck after 10 minutes. It could be stuck at once like the migration, but a Deployment, unlike a hook Job, has that deadline already.
- **A migration Pod that cannot pull its image** (a preview whose image is not built yet) is not stuck at once; the Arriving limit catches it after 15 minutes.
- **A reason on a Run.** `pending` says the Pod has not started; why is in the Environment's Activity (a migration) or the task's Warning (a task run). The Run keeps its three fields.
- **Deploying or Updating after a time limit.** #132 asked for Arriving and Leaving. A Deploy stuck at Applying on a migration that runs forever is still not flagged.
- #131's own fixes (a retry limit for previews, a migration deadline) are its ticket.

## Glossary

Nothing is added to `CONTEXT.md`: pending, Arriving and Leaving are the status vocabulary (#116), not Platform language.

## Tests

- `condition_test.go`: `TestUnschedulableHookPodIsStuck` has #132's observed `hello-pr-2` (a preview's first migration, Unschedulable, 20 minutes in), the same 30 seconds in, a gate-traced Deploy's migration, the migration of a change that is not a Deploy, a migration Pod pulling its image (not stuck), another Job's Unschedulable Pod (not counted), and the final backup's Pod. `TestArrivingAndLeavingAreStuckAfterTheirTimeLimits` has each limit on both sides, each known reason, #131 at 20 and at 46 minutes, a first Deploy into an Unreleased Environment counted from its acceptance, and an arrival with nothing on its way (never stuck by time), and checks the Deploy in the Activity and in the list agree.
- `capability_test.go`: `TestRunIsPendingUntilItsPodStarts` for a migration and a task run alike (no pod, Unschedulable, pulling, another Job's pod, running, by the older label, finished before the Job, a retry's pod, `Complete`, `Failed`); `TestUnschedulableScheduledTaskRunIsAWarning` is the `heartbeat` case, with the Environment untouched.
- `kube_test.go`: `TestReadSeesTheJobsPods` is `hello-pr-2` through `Read`, the List path: pods listed by the application label before there is a Deployment, the Activity stuck, the migration `pending`.
- `internal/deploygate/status_test.go`: the running task run now has its pod, which the gate lists by the application label.
- `internal/cli/app_status_test.go`: a `pr-2` block with the stuck Activity and `pending since`.
- `cmd/iidp-argus/webtest/card.test.js`: the task Warning's tag.
