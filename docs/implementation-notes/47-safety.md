# #47 Safety: saved OpenTofu plans, and retries pinned to a failed commit

Items 1 and 2 of #47, both found while running the acceptance of #19 against the real Platform. The other items of #47 are recorded in their own notes. ArgoCD's `retry.refresh` was checked against the upstream "Automated Sync Policy" documentation ("Automatic Retry Refresh on new revisions") and `RetryStrategy` in `pkg/apis/application/v1alpha1/types.go` on 2026-09-24; the Application CRD on the live Platform (ArgoCD v3.5.3) has the field.

## Saved plans are ignored everywhere, not just under `infra/`

**Problem.** `stage_opentofu` runs `tofu plan -out=tfplan` in `infra/state-bucket` and `infra/platform`. A saved plan holds every variable value in plaintext, sensitive ones included: the Hetzner token, the Object Storage keys and the GitHub App private key. `.gitignore` covered `*.tfvars` and state but not plans, so after the acceptance run `git status` listed `infra/state-bucket/tfplan` and `infra/platform/tfplan` as untracked, one `git add .` away from a public commit.

**Choice.** `.gitignore` now ignores `tfplan`, `tfplan.*`, `*.tfplan` and `*.tfplan.*`, unanchored. Unanchored, so a new OpenTofu root anywhere in the repository is covered without anyone remembering to extend the list. The `.*` variants cover the two usual derived files: `tofu show -json tfplan > tfplan.json` prints the same values, and `<name>.tfplan` is the other common naming. No other file in the repository matches these patterns. A wizard test runs `git check-ignore --no-index` on both existing roots, on a root that does not exist yet, and on the derived names.

## The wizard deletes its plan on every way out

**Question.** Should the wizard also delete the plan file, or is `.gitignore` enough?

**Choice.** Delete it. `stage_opentofu` now goes through one helper, `plan_and_apply DIR QUESTION ABORT_MESSAGE`. It saves the plan, asks, applies exactly that plan, and removes `DIR/tfplan` whether the apply succeeded, failed, or the admin declined. A plan has no use after any of these. Once applied, OpenTofu refuses to apply it again ("Saved plan is stale"). After a failed apply the state may have moved, so the plan is stale too. After a decline, the next run makes a fresh plan anyway. Keeping it only leaves one more plaintext copy of the secrets on disk. That copy is not the only one, since `terraform.tfvars` holds the same values (ignored, mode 600), but it is a copy nobody expects: `.gitignore` protects only against `git add`. It does nothing against `git add -f`, a zip of the directory, or a sync or backup tool that doesn't read `.gitignore`.

`.gitignore` remains the guarantee, because an interrupted run leaves the plan behind: Ctrl-C at the confirm prompt kills the wizard before any cleanup runs. An `EXIT` trap could catch that case, but the wizard has no trap today. One trap for a file that `.gitignore` already covers wasn't worth the extra machinery. `--dry-run` still only prints what it would run, the deletion included.

Wizard tests run `plan_and_apply` against a fake `tofu` on `PATH`. The fake writes a plan the way the real one does and logs its calls. The tests assert that the saved plan is what gets applied, and that no `tfplan` is left after a successful apply, a failed apply, or a decline, and that a decline applies nothing.

**Existing checkouts.** A checkout where the wizard already ran still holds the two old plans. After this change they are ignored, but they are still on disk: delete them by hand (`rm infra/state-bucket/tfplan infra/platform/tfplan`).

## `retry.refresh: true`, keeping `limit: -1`

**Problem.** Every bootstrap component Application (the shared `iidp-bootstrap.syncPolicy` in `bootstrap/templates/_helpers.tpl`) and the three Applications the wizard writes into the Platform repository (`platform-components`, `platform-secrets`, `applications`) retry a failed sync without limit. A retrying operation keeps the revision it started with, and ArgoCD starts no new automated sync while an operation is running. So a sync that fails because of a bad commit keeps retrying that commit. The fix pushed after it is never applied until someone terminates the operation by hand. That is what kept `platform-secrets` OutOfSync on the real Platform after the fix in #46 was already on `main`.

**Why not a finite limit.** A finite limit would also free the Application: once the retries run out the operation ends, and the next commit triggers a new automated sync. But the bootstrap depends on unlimited retries. Components arrive in any order: cert-manager's and ArgoCD's CRDs, the namespaces `platform-secrets` writes into, and KSOPS in the repo server all have to exist before the Applications that need them can sync. How long that takes depends on the node. `04-bootstrap.md` records syncs that stayed waiting for more than the ten-minute CI budget. With a finite limit, an Application whose dependency is slower than the limit ends up Failed on a correct commit. Nothing then syncs it again until someone clicks Sync or pushes an unrelated commit. That is the manual step the bootstrap exists to avoid, and it trades one hand intervention for another.

**Choice.** `retry.refresh: true` alongside `limit: -1`. With it, ArgoCD refreshes the Application before each retry and syncs the newest revision of `targetRevision` rather than the one the operation started with. Retries stay unlimited for the arrival-order case, and a pushed fix is picked up at the next retry, at most `maxDuration` (3m) later. `backoff` is unchanged.

**Where it is set.**

- `bootstrap/templates/_helpers.tpl`: the shared sync policy, used by all eight component Applications. `bootstrap/README.md` says so too.
- `scripts/bootstrap-wizard.sh`: `write_bootstrap_components`, `write_bootstrap_secrets`, `write_bootstrap_applications`, each with a comment in the written file, since the admin reads these in the Platform repository.
- `test/e2e/fixtures/platform-repo/bootstrap/*.yaml`: the hand-written copies the kind harness serves, so the e2e runs the same policy the wizard writes. The ArgoCD the harness installs is the chart pinned in `bootstrap/versions.yaml`, the same one as the Platform, so the kind run also proves the CRD accepts the field. A CRD without it would refuse the Application under server-side apply (`field not declared in schema`).

**Where it is not set, deliberately.** Only Applications that already have a retry policy get the field. `refresh` has no effect without `retry`, and adding a retry policy where there is none would change behaviour beyond this item.

- The root Application `platform` (cloud-init's `infra/platform/cloud-init/user-data.yaml.tftpl` and its mirror in `test/e2e/harness.go`) has no `retry`. A failed automated sync there is not retried against the old revision, and the next commit starts a new sync, so it never gets stuck the way the issue describes.
- Each Environment's Application, which the CLI writes (`internal/render`) and the e2e fixture copies (`applications/shop/*/application.yaml`), has no `retry` either, for the same reason. A failed migration stays failed until the next commit (`07-chart-postgres.md`), which is what the fix commit then applies.

**Tests.** `TestEveryApplicationRetriesForeverAgainstTheNewestRevision` in `bootstrap/bootstrap_test.go` renders the bootstrap and asserts `retry.limit == -1` and `retry.refresh == true` on every Application. With `refresh` removed from the helper, it fails for all eight. Wizard tests assert the same `retry` block in each of the three wizard-written files and in the e2e fixture's copies, so the two cannot drift apart.

**The live Platform.** Changing the wizard does not change what is already in `Itema-as/iidp-platform`. Its `bootstrap/platform-components.yaml`, `platform-secrets.yaml` and `applications.yaml` were written by an earlier wizard run and keep the old policy until the wizard is re-run or `refresh: true` is added under each `retry:` by hand. The component Applications pick up the new helper once `platform-components.yaml`'s `targetRevision` is moved to a release that contains this change.
