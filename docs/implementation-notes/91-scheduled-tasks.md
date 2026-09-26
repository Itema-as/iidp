# #91 Scheduled tasks declared in iidp.yaml

This note records the choices made while building Scheduled tasks: commands a Web service runs on a schedule, declared in its Application repository's `iidp.yaml` and carried to the Platform with every deploy, the way the migration command is since #66 ([66-migration-command-in-repo.md](66-migration-command-in-repo.md)). The design is the ADR-0005 note from #89 ([ADR-0005](../adr/0005-private-application-repositories-on-github-free.md)); the contract is in [`docs/platform-repository.md`](../platform-repository.md#scheduled-tasks-travel-with-the-deploy), the chart's side in [`chart/application/README.md`](../../chart/application/README.md), and the developer's side in the README's "Scheduled tasks".

## The path a task takes

The same code paths as the migration command, with one new field on each:

1. `internal/appconfig`: `File.Tasks`, `Task{Name, Schedule, Command}`, `ValidateTasks`, `CheckTaskNamesFit`, `ValidateSchedule`. `Parse` reads `tasks:` strictly, like the rest of the file.
2. `iidp ci set-image` reads the deployed commit's `iidp.yaml`, checks the tasks (and their names against the Application's), prints them, and sends them as `Request.Tasks`.
3. The Deploy gate validates them again before it clones anything (400), then `platformrepo.SetImageTag` writes them with `render.SetTasks` into the Environment's `values.yaml` in the same commit as the tag, refusing a Static site (409).
4. The chart renders one CronJob per task (`templates/cronjobs.yaml`).
5. Promotion needs nothing new: the promote job already checks out the tagged commit (#66), so `ci set-image` there sends that commit's tasks.

## The format

```yaml
tasks:
  - name: nightly-cleanup
    schedule: "0 3 * * *"
    command: node scripts/cleanup.js
```

- **`name`**: a DNS-1035 label (lowercase letters, digits and dashes, starting with a letter, not ending with a dash), unique within the file. It ends the CronJob's name and is the `iidp.itema.no/task` label.
- **`schedule`**: exactly five cron fields. No `@daily` and the like, and no `TZ=`/`CRON_TZ=`: there is one way to say when, and tasks always run on Europe/Oslo time. Macros were left out even though Kubernetes accepts them, since "0 3 * * *" is as short as `@daily` and names the hour, which matters once there is a time zone.
- **`command`**: one line, at most 1024 bytes, the migration command's rule (`validateShellLine`, now shared). An empty command is refused.
- **At most 5 tasks** (`appconfig.MaxTasks`). Each run is a Pod of 250m CPU and 256Mi on a single node. Five covers nightly cleanups and reports; more belongs in one task that does several things.
- Unknown keys in a task (a misspelt `schedul`, or a `timeZone`) are refused with the line, like unknown top-level keys, so a typo fails the deploy instead of silently dropping a task.

**Schedules are parsed with the parser Kubernetes uses.** The API server validates `CronJob.spec.schedule` with `github.com/robfig/cron/v3`'s standard parser, and refuses any schedule containing `TZ` (checked in kubernetes/kubernetes `release-1.36`: `pkg/apis/batch/validation`'s `validateScheduleFormat`, through `ParseCronScheduleWithPanicRecovery`, with `go.mod` pinning robfig/cron v3.0.1; the same file refuses a CronJob name over 52 characters). `appconfig` uses the same module and version with the five standard fields and no descriptors, so what the gate accepts the API server accepts, and a schedule that would fail the Environment's sync (and with it the image's rollout) is refused at the deploy instead. This is the one new dependency; writing a cron parser that stays a subset of Kubernetes' was the alternative, and would have had to be tested against that parser anyway.

**YAML and `*`.** A plain YAML value cannot start with `*` (it reads as an alias), so `schedule: */5 * * * *` is a YAML error, not a schedule error. `Parse` adds "quote a schedule that starts with *" to that error, and the generated file's comment says to quote schedules.

**The name's length depends on the Application.** Kubernetes refuses a CronJob name over 52 characters (the controller appends 11 to name each Job, and a Job name is at most 63). The CronJob is `<app>-<task>` in prod and `<app>-staging-<task>` in staging. `CheckTaskNamesFit` checks against the staging form whichever Environment is deployed, so a task prod accepts is not refused later when a staging Environment is added. `ci set-image` and the gate both run it; `ValidateTasks` alone only knows the 52-character ceiling. Hashing or truncating the name was the alternative: it removes the limit, but the name is what a developer sees in `kubectl get cronjobs`, in logs and in `iidp app status`.

## No "keep" case

The migration command distinguishes "no file" (keep what the Platform has) from "no line" (clear it), because Applications made before #66 had a command on the Platform and no file. Tasks have no such history: only a deploy has ever written them. So any deploy without tasks, whether the file has no `tasks`, has `tasks: []`, or does not exist, removes them. Deleting a task from `iidp.yaml` stops it at the next deploy, and deleting the whole file does too.

On the wire `Request.Tasks` is `omitempty`: none leaves the key out. That keeps a new CLI working against a gate from before this change for every repository that declares no tasks; the older gate decodes with `DisallowUnknownFields` and answers 400 "unknown field tasks" only to one that does. The order to roll out is therefore the Platform (the gate) first, then the release the workflows install, then the tasks. A new gate with an old CLI works: no tasks sent, none written, and an old CLI refuses an `iidp.yaml` with `tasks` ("unknown setting").

In `values.yaml` no tasks means no `tasks` key at all, not `tasks: []`, so an Environment that never had tasks keeps the values file it has, and a deploy of a repository without tasks touches nothing but the tag. `render.SetTasks` compares the tasks, not the text, so a repeat with the same tasks is "unchanged" and commits nothing.

`add-capability --staging` drops `tasks` from the prod values it copies: staging runs the tasks of the commit its first deploy brings, and nothing renders before that anyway.

## The gate

- `ValidateTasks` and `CheckTaskNamesFit` run with the other request checks, before the token's repository is even compared with the binding: 400, "the request is malformed", naming the task and the rule.
- A Static site is refused with **409**, like a migration command without Postgres. The request is well formed and the caller allowed; it conflicts with the Environment's Kind. The message says to remove `tasks` or run them from a Web service. Unlike the Postgres case, no Capability resolves it, since the Kind is fixed.
- The commit is still one "Deploy <app> <environment> <tag>". When the tasks changed, a paragraph lists them ("Scheduled tasks, from iidp.yaml:" and one `- name (schedule): command` line each) or says "Remove the Scheduled tasks: iidp.yaml declares none.", after the migration command's paragraph, so the Platform repository's log shows when a task was added, changed or removed.
- `Response.TasksChanged` reports it; `ci set-image` prints "The Scheduled tasks changed with it."
- Tasks go only to the Environment the caller may deploy: `main` sets staging's (or prod's without staging), and prod keeps its own until a `v*` tag promotes, bringing that tag's.

## The chart

One CronJob per task, `<fullname>-<task>`, in the default wave with the Deployment. Its settings are chart constants, not values: a developer declares when and what, and the Platform decides how. What was checked against the Kubernetes documentation (kubernetes.io's CronJob concept page and the `batch/v1` CronJob and Job API reference, read 2026-09-26 for the current release; the node runs k3s v1.36) and chosen:

- **`timeZone: Europe/Oslo`.** `spec.timeZone` is stable (GA in 1.27) and takes an IANA name, validated by the API server and resolved by the controller manager from the system time zone database, with Go's bundled one as the fallback. Without it the schedule is in the controller manager's zone, UTC on the node. A `TZ`/`CRON_TZ` prefix in `schedule` is a validation error, which is one more reason `appconfig` refuses it. Daylight saving, checked with robfig/cron v3.0.1's `Next` in Europe/Oslo: `30 2 * * *` has no time on the last Sunday of March and two (02:30+02:00 and 02:30+01:00) on the last Sunday of October; `0 3 * * *` runs once on both days. The documentation does not describe this; the CronJob controller computes times with the same library, so the chart's comment and the README say to keep nightly work out of 02:00-02:59.
- **`concurrencyPolicy: Forbid`**: a run still going when the next is due makes the next one skip ("if it is time for a new Job run and the previous Job run hasn't finished yet, the CronJob skips the new Job run"). With `startingDeadlineSeconds` set, a skipped time can still start once the previous run finishes, if that is within the deadline. Overlapping runs of a cleanup or a report are rarely wanted, and `Replace` would kill work half done.
- **`startingDeadlineSeconds: 300`**: a time whose Job cannot be created within five minutes (controller down, node rebooting, a previous run still going) is skipped, not made up later. The documentation also says that without it the controller counts every missed time since the last run and stops scheduling with "too many missed start times" past 100, and that a value under 10 seconds may never schedule, since the controller checks every 10 seconds. Five minutes survives a k3s restart without running last night's 03:00 task at breakfast.
- **`successfulJobsHistoryLimit: 1`, `failedJobsHistoryLimit: 3`** (the defaults are 3 and 1). One success is enough to show the last good run and its log; three failures give something to compare. Each kept Job keeps its Pod object, not a running container, so the cost is etcd objects, not memory.
- **`backoffLimit: 0`**, like the migration Job. With 1, a failure would retry at once, and a task that half ran (sent some emails, deleted some rows) would do it again; tasks are expected to be idempotent across runs, since the documentation warns that a CronJob "might" create two Jobs or none for one time, but an immediate retry doubles the effect of a deterministic bug. The next time on the schedule is the retry. A Pod lost to a node reboot also counts as the run's failure; `podFailurePolicy` could ignore such disruptions, and was left out as more than a first version needs.
- **`activeDeadlineSeconds: 3600`**: a run is stopped after an hour and the Job fails with `DeadlineExceeded`; the documentation says it takes precedence over `backoffLimit`. An hour is well past a cleanup or a report and short enough that a hung task does not hold 250m CPU all day.
- **`restartPolicy: Never`**, so a failed container is a failed run rather than a restart loop inside the Pod.

**Resources: the smallest size's, 250m CPU and 256Mi memory, as requests and limits**, whatever the Application's size (`application.tasks.resources`, sharing the sizes table through the new `application.resourcesFor`). The issue asks for the smallest size; limits on every container are also what #90's admission policy will require. Requests equal to limits keep the Guaranteed QoS every other chart workload has. A task on a `large` Application that needs more memory is a signal to make the work smaller; making the task size a setting is left until someone needs it.

**Environment**: the Application's `env`, its `secrets` as `envFrom`, and `DATABASE_URL` with Postgres, exactly what the migration Job gets. No `PORT`: a task listens on nothing, and the migration Job does not get one either.

**Labels**: the CronJob gets the Application's labels plus `app.kubernetes.io/component: scheduled-task` and `iidp.itema.no/task: <name>`. The Job template and the Pod template get `app.kubernetes.io/name`, the component, `iidp.itema.no/application`, `iidp.itema.no/environment` and `iidp.itema.no/task`, but not `app.kubernetes.io/instance`, so the Service's selector never matches a task Pod (the migration Job's rule). Alloy attributes logs by the `iidp.itema.no` labels; `iidp app status` (#94) can find an Environment's runs by `iidp.itema.no/task`.

**`runTasks`** (default `true`) is the flag the previews ticket (#95) needs: a Preview Environment shares staging's values, tasks included, and must run none. With `runTasks: false` no CronJob renders and the CronJob name's length is not checked, since a preview's `<app>-pr-<n>` name must not refuse tasks it does not run. The other task checks (a Static site, a bad or repeated name, a missing schedule or command) still run, so a values file is refused the same way everywhere. The name was chosen to read as what it does in a preview's values; `tasks.enabled` would have meant nesting the list under a mapping, a different shape from `iidp.yaml`'s.

**The chart's own refusals**: tasks on a Static site (a `refuse-*` fixture), a name that is not a DNS label or is used twice, a CronJob name over 52 characters, a missing schedule or command. The gate refuses all of these first; the chart checks them so a hand-edited values file fails on render rather than on apply. It does not parse cron: that would mean a cron parser in a Helm template, and the API server refuses a bad schedule on apply anyway.

**ArgoCD**: its built-in CronJob health check (argo-cd `resource_customizations/batch/CronJob/health.lua`) reads Healthy until the first success, while a run is active, and after a success; it reads **Degraded** when the last run failed after an earlier success. So a failing task shows on the Environment's ArgoCD Application, which is wanted, and it does not block a sync: the CronJob is in wave 0, after the migration hook, with nothing waiting on it.

**#90's securityContext helper.** #90 (guardrails) adds a shared securityContext to every workload the chart renders. It was not merged when this was written, so the CronJob's Pod template has no securityContext yet; #90 must add its helper to `templates/cronjobs.yaml` alongside the Deployment and the Jobs. The CronJob already sets limits, which #90's policy requires.

## Tests

- **`internal/appconfig`** (`appconfig_test.go`): valid tasks (none, null, empty, one, a folded command, every cron field form Kubernetes accepts including names and steps, the maximum count), each refusal (not a list, not a mapping, a missing field, an unknown or repeated setting, a non-string value, each bad name, a repeated name, too many, an empty, 4- or 6-field, macro, time zone or out-of-range schedule, an unquoted `*` schedule, an empty, two-line, control-character or oversized command), a command exactly at the limit, `CheckTaskNamesFit` at the boundary, and that `Render`'s commented example parses once uncommented.
- **`internal/render`** (`tasks_test.go`): `SetTasks` writes, repeats unchanged, drops a task, changes a schedule, removes (nil and empty), leaves a file without tasks untouched, refuses a Static site (removing there is fine), and `CopyValuesForStaging` drops prod's tasks.
- **The gate** (`internal/deploygate/tasks_test.go`, through HTTP with the fake issuer, fake GitHub and bare Platform repository): tasks written with the tag in one commit touching only that Environment's `values.yaml`, with the message listing them; a repeat unchanged and a changed schedule on the same tag committed; tasks removed by a deploy without them (no key and an empty list), with the message saying so, and a later deploy without tasks saying nothing about them; a Static site refused with 409 and nothing committed, and deploying without tasks; a main deploy setting staging's tasks and leaving prod's, a `v*` promotion carrying them to prod, and promoting an older tag without tasks removing them; each 400 refusal, and the longest name that fits; a caller from another repository refused before any of it.
- **`ci set-image`** (`internal/cli/ci_set_image_tasks_test.go`, through `cli.RunWith`): the tasks sent and printed, the key left out with no tasks, an empty list or no file; refusals before any token request (a bad schedule, an unquoted `*`, an unknown task setting, a name too long for `shop`); "The Scheduled tasks changed with it."
- **The chart** (`chart/application/tasks_test.go`): each field above, the image, the command, env, `DATABASE_URL`, secrets, no `PORT`, the smallest size on a `medium` and a `large` Application, no env without any, the labels on the CronJob, Job and Pod templates and that the Pod does not match the Service, `runTasks: false` rendering everything but the CronJobs (and not checking the name's length), no CronJob without tasks, the refusals, and kubeconform on both fixtures (with the CRD schemas, for the Postgres one). `unreleased_test.go` now has tasks in its every-Capability fixture, and renders the CronJob from values the CLI and the gate's `SetTasks` wrote.
- **The kind e2e**: the fixture `test/e2e/fixtures/shop-repository/iidp.yaml` declares `heartbeat`, every minute, `test -n "$DATABASE_URL" && echo ...`. `testMigrationCommandFromIidpYAML` reads it with `appconfig.Read`, sends its tasks with the migration command in shop's deploy, checks the commit lists the task, and after `shop-prod` is Healthy waits (`WaitForScheduledTaskRun`) for a Job labelled `iidp.itema.no/task=heartbeat` to succeed and log the marker. Before that, brochure's tasks are refused with 409. One small Pod runs for a second each minute; by then shop-staging's 500m of requests are gone, so it fits the kind runner's CPU budget. `e2e.yaml`'s path filter now includes `internal/appconfig/**`, which the gate is built from.

## What was not built

- **A task size or timeout per task.** Constants until someone needs otherwise.
- **Running a task by hand.** `kubectl create job --from=cronjob/<name>` works for an admin; a CLI command is not in scope.
- **`podFailurePolicy`** for disruptions (above).
- **A status view.** #94 reads each CronJob's last schedule time and last Job's result.

## What `hello` needs to use it

Nothing in its repository until it wants a task. The Platform needs the release with this change (the gate first, since an older one refuses a deploy that sends tasks), and the chart version that renders CronJobs; then a `tasks:` list in `hello`'s `iidp.yaml`, pushed to `main`, starts it in staging, and the next `v*` tag in prod. #96 does this on the live Platform.
