# #47 item 13: an Environment without its first image

Questions that came up while making an Environment that has not received its first image render cleanly, the options considered, and the answer chosen for each. ArgoCD's behaviour was checked against its source at `v3.5.3` (the version `bootstrap/versions.yaml` pins: `controller/health.go`, `controller/state.go`, `controller/appcontroller.go`) and its "Automated Sync Policy" documentation (context7, `/websites/argo-cd_readthedocs_io_en_stable`) on 2026-09-24.

## The problem

`iidp app create` writes every Environment with `image.tag: ""`, because no image exists yet; only the deploy workflow's write-back (`iidp ci set-image`) ever sets it. The chart's `required "image.tag is required"` (`application.image`) then failed the render, and ArgoCD showed the Environment as sync status `Unknown` with a `ComparisonError` ("failed to generate manifest ... image.tag is required") until the first write.

The issue names prod of an Application created with `--staging`, which waits for the first `v*` tag and can stay broken for weeks, but the same state exists in every new Environment:

- prod of an Application without staging, from creation until the deploy workflow's first run on `main`;
- every Environment of an Adopted Application, until its pull request is merged and the workflow has run;
- staging added later with `iidp app add-capability --staging` (`render.CopyValuesForStaging` resets the tag to `""`), until the next push to `main`;
- prod with staging, until the first `v*` tag promotes staging's image.

So the fix belongs to the "no image yet" state itself, not to prod-with-staging alone.

## Options

1. **The chart renders nothing while `image.tag` is empty.** Every template keeps its validation and gates its objects on the tag.
2. **The chart renders only what makes sense without an image: the database.** The Postgres `Cluster`, `ObjectStore` and `ScheduledBackup` (and, since it is gated on `postgres.enabled`, the final-backup hook with its RBAC) before the first release; the Deployment, Service, Ingresses and migration Job after it.
3. **The chart renders the Service and Ingress too, with no Deployment.** The address would exist and answer 503 until the first release, and a foreign custom domain would get its certificate early.
4. **The CLI writes prod's `application.yaml` only on the first promotion.** `iidp ci set-image` would create it when it first writes prod's tag.
5. **The CLI writes a placeholder image** ("coming soon") instead of an empty tag.
6. **An explicit `released: false` value** beside the empty tag.

## Choice: option 1, the chart renders nothing

`application.released` (`_helpers.tpl`) is `"true"` when `image.tag` is set, empty otherwise (an empty string or a key removed altogether, since the chart's own default is `""`; a value such as `0` counts as set). Every template includes `application.validate` first, exactly as before, and then renders its objects only inside the gate. With the tag set, every existing fixture renders byte for byte what it rendered before this change.

**Why not option 2 (the database first).** Nothing can use prod's database before the first release: the Application image does not exist, and nobody reaches prod's database from outside. A `Cluster` created weeks early would hold 250m CPU and 256Mi memory as Guaranteed QoS on the single node, claim a 5Gi volume, archive WAL and take a daily base backup of an empty database into the Object Storage bucket, all for nothing. It would not make the first release meaningfully faster either: the first sync of any new Environment with Postgres already waits a minute or two for its `Cluster` (`07-chart-postgres.md`), and the first image of an unreleased Environment makes its first sync exactly that. Deletion would get worse, not better: the final-backup `PreDelete` hook would back up an empty database and keep it 30 days. The database's lifecycle is sensible when it starts with the Application that uses it and ends with a backup of what that Application wrote, and option 1 gives exactly that.

**Why not option 3 (an address with nothing behind it).** An Ingress to a Service with no endpoints answers 503, which reads as broken just like the comparison error did. Issuing a foreign domain's certificate early is a small gain against a real cost: ingress-shim would start HTTP-01 challenges for a DNS name the developer may not have pointed anywhere yet.

**Why not option 4 (the CLI writes prod's Application later).** Many things read `application.yaml` as "this Environment exists": `bootstrap/applications.yaml`'s discovery glob, `checkApplicationAbsent` (an Application whose only Environment has no `application.yaml` counts as free, so `iidp app create` could clear an Adopted Application whose pull request is not merged yet), `iidp app delete`, `iidp secret set` and `add-capability --postgres` (both add a `sops/` source to it). Every one of them would need a "prod exists but has no Application yet" case. `iidp ci set-image` would need to render an ArgoCD Application in CI, with its chart pin taken at first release rather than at creation. And it would only fix prod-with-staging: every other case listed above would still show a comparison error.

**Why not option 5 (a placeholder image).** It would run an image the developer did not build, in their namespace, on the node; with a migration command, the migration Job would run it with `sh -c "npx prisma migrate deploy"`, and fail. The Environment would look released when it is not.

**Why not option 6 (an explicit flag).** Two values that must agree (a `released: false` with a tag, a `released: true` without one) are a new way for the file to be wrong. The empty tag is already written by the CLI, only ever set by the deploy workflow, and refused as empty by `iidp ci set-image`: it already is the signal. To make it readable in the Platform repository, the header comment the CLI writes into every `values.yaml` (`render.Values`) now says so ("until then it is empty and the Environment renders nothing"), and the chart's `values.yaml` documents it on `image.tag`.

## What ArgoCD shows for an Application that renders nothing

Checked in the source rather than assumed:

- **Sync status.** `CompareAppState` (`controller/state.go`) starts from `syncCode := v1alpha1.SyncStatusCodeSynced` and only moves to `OutOfSync` for a resource that is modified, missing live or extra live, or to `Unknown` when manifests fail to load or a resource is not permitted by the project. There is no special case, warning or condition for an empty target list. `CreateNamespace=true` adds the namespace to the compared objects only with `managedNamespaceMetadata`, which the CLI does not write.
- **Health.** `setApplicationHealth` (`controller/health.go`) starts from `health.HealthStatusHealthy` and only degrades it per resource; the one "no resources" case, `Missing`, applies only when the Application has non-hook resources in its target and none of them live. With no resources at all it stays `Healthy`.
- **Automated sync.** `autoSync` (`controller/appcontroller.go`) does nothing unless the sync status is `OutOfSync`, so an Application with nothing to apply is never synced, and its namespace is not created until there is something to put in it.

An unreleased Environment is therefore `Synced` and `Healthy`. With Postgres on it is not quite empty: `iidp app create --postgres` adds the `sops/` source with the `backups-credentials` Secret, which ArgoCD applies (creating the namespace) and which is Healthy as soon as it exists; secrets set with `iidp secret set` before the first release are applied the same way and are ready when the Application starts. The kind end-to-end test asserts both shapes (below).

## What happens at the first image

`iidp ci set-image` writes the tag into `values.yaml`. The chart now renders the whole Environment, the Application is `OutOfSync` (every object is missing live), and its automated sync applies it in the waves the chart already uses: the `Cluster` and `ObjectStore` (with the Secret) in wave -2, the migration Job and `ScheduledBackup` in wave -1 once the database is healthy, the Deployment, Service, Ingresses and the final-backup RBAC in wave 0. That is the first sync of a brand-new Environment today, just triggered by the first image instead of by creation. A migration runs against a new, empty database, which is what a first migration expects.

## The migration Job, the PreDelete hook and `iidp app delete`

- **The migration Job** is a `Sync` hook that runs the Application image, so it cannot run before there is one; it is inside the gate like everything else.
- **The final-backup `PreDelete` hook** is inside the gate too. ArgoCD discovers `PreDelete` hooks by rendering the Application at the moment deletion starts (`39-final-backup-predelete-hook.md`), from the `values.yaml` `iidp app delete` leaves in place. For a never-released Environment that render has an empty tag, so it has no hook, and there is no `Cluster` it could have backed up. Had the hook been rendered without its `Cluster` (the mirror image of option 2), it would have failed, and a failed `PreDelete` hook blocks the deletion with a `DeletionError` indefinitely.
- **`iidp app delete`** needs no change. It removes `application.yaml`; the resources finalizer deletes whatever the Environment's other sources applied (the `backups-credentials` Secret, developer secrets); the Application is gone. A released Environment is deleted exactly as before, hook included, since its `values.yaml` has a tag.

## `add-capability`, `secret set` and Adopt

All three work unchanged, because each edits files the chart reads, and the chart renders nothing until the first tag either way. `add-capability --postgres` on an unreleased Environment copies `backups-credentials` in (applied as above), and the database is created at the first image. `--domain`, `--size` and `--login` are validated at render time even while nothing is rendered (below), so a mistake does not wait for the first release to show. `--staging` gives the new staging Environment an empty tag, which now renders nothing until the next push to `main` instead of a comparison error. An Adopt Application's Environments sit Synced and Healthy until its pull request is merged.

## Refusing a broken values file still works without an image

Several checks used to run only inside the objects that use a value: an unknown `size` in the Deployment's resources, a missing `image.repository` in `application.image`, a missing `platform.backupsBucket`/`objectStorageEndpoint` in the `ObjectStore`. With those objects gated, a broken file would have rendered cleanly as long as it had no tag. `application.validate`, which every template runs before its gate, now also requires `image.repository`, resolves the size, and, with Postgres on, requires the bucket and the endpoint (the endpoint moved into a helper, `application.postgres.objectStorageEndpoint`, so the `ObjectStore` and the validation share one message). An unreleased Environment is refused for exactly the reasons a released one is: `unreleased_test.go` renders every `refuse-*` fixture with and without its tag and requires the same message. Two fixtures were added for refusals that had none: `refuse-missing-repository.yaml` and `refuse-postgres-missing-endpoint.yaml`.

## A released Environment whose tag is emptied by hand

Emptying the tag of a running Environment makes the chart render nothing, and automated sync with `prune: true` deletes what is no longer rendered, the `Cluster` and its volume included. ArgoCD's `allowEmpty` protection ("automated sync with pruning protects against automation or human errors by preventing an application from having zero resources", default off) does not help here in general: it only skips a sync that would prune every resource, and an Environment with Postgres keeps its `backups-credentials` Secret. This is not a new class of risk: a hand edit setting `postgres.enabled: false`, or removing the Environment's `application.yaml`, already deletes the database today, and the Platform repository is not edited by hand. Nothing in the Platform's own flow can do it: `iidp ci set-image` refuses an empty tag (`runCISetImage`), and the only CLI paths that write `""` create a new Environment. If this ever needs a hard guard, `argocd.argoproj.io/sync-options: Prune=false` on the `Cluster` would keep a sync from ever pruning the database (deleting the Application still removes it); that changes every released Environment and is left as a follow-up rather than done here.

## Chart version and existing Environments

The chart's version is the iidp release's (the Release workflow packages it with the tag's version; `Chart.yaml`'s `0.1.0` is a placeholder for local rendering, `06-chart-web-service.md`), so there is nothing to bump in `Chart.yaml`: the change ships as the chart of the next `v*` release. Then:

- **New Environments** get it once the Platform admin sets `platform.yaml`'s `chartVersion` to that release.
- **Existing released Environments** need nothing. With a tag, every template renders exactly what it rendered before (checked for every fixture), so moving their `targetRevision` to the new chart version is a no-op sync.
- **Existing unreleased Environments** (for example `hello-prod` on the real Platform) keep their comparison error until their first image, which then fixes them as it always did. To clear it sooner, move that Environment's `application.yaml` `targetRevision` to the new chart version. There is no CLI command for that yet, so it is an admin edit in the Platform repository.

## Testing

- **Chart** (`chart/application/unreleased_test.go`): `unreleased-prod.yaml` has every Capability that brings objects of its own (Postgres with a migration, a wildcard and a foreign domain, a Secret) and renders nothing, with and without `--namespace`. Every renderable fixture renders nothing with its tag emptied (`--set-string image.tag=`) or removed (`--set image.tag=null`). Every `refuse-*` fixture is refused with the same message with and without its tag. Every file in `templates/` references `application.released`, so a new template that forgets the gate fails with its name, even if no fixture switches it on.
- **The transition, through the CLI's own rendering**: the same test file builds the prod values `iidp app create --staging --postgres --domain` writes with `internal/render.Values`, renders nothing from them, applies `render.SetImageTag` (the edit `iidp ci set-image` makes) and requires the complete set of objects, with the image `<repository>:<tag>` in the Deployment and the migration Job. A second test takes a released prod through `render.CopyValuesForStaging` (`add-capability --staging`): the new staging renders nothing until its own first tag. These run in the `Chart` CI job, which has `helm`; a test in `internal/cli` would always be skipped in the `Go` job, which does not.
- **End to end** (`test/e2e`, `testUnreleasedEnvironments`): the fixture Platform repository has two more Applications with no image, `later` (prod only, Postgres on, the `sops/` source with `backups-credentials`, as `app create --postgres` writes it) and `brochure` (a Static site with nothing else, so no resources at all). The test requires both `Synced` and `Healthy` with no conditions, `later-prod` managing exactly `Secret/backups-credentials` and `brochure-prod` nothing, and no Deployment, Service, Ingress, Job, `Cluster`, `ObjectStore` or `ScheduledBackup` in either namespace. Neither adds a Pod to the kind node. The transition to released and the deletion of a never-released Environment are not repeated end to end: the first would add a database and a workload to a runner already near its CPU request ceiling, and the second rests on the chart rendering no hook, which the chart tests prove, plus ordinary ArgoCD deletion.

## Not done here

The CLI's closing line after `iidp app create` ("The Environment deploys once the deploy workflow writes the first image tag.") is still true and was left alone, since the CLI's output is being changed by other work on #47 in parallel. Saying there that prod waits for the first `v*` tag when `--staging` is on, and that ArgoCD shows it Synced and Healthy with nothing running until then, would be a small follow-up.
