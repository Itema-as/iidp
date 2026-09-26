# #74 Application deploy workflows call a reusable workflow

Decisions taken while moving the deploy logic out of every Application repository and into one reusable workflow here. Sources, checked on 2026-09-26:
- GitHub's docs: "Reusable workflows" (reference and how-to), "Contexts" (`job.workflow_sha`), "OpenID Connect" (claims), and the workflow-syntax filter patterns;
- the org's own Actions policy, read with `gh api orgs/Itema-as/actions/permissions`.

## What was checked, rather than assumed

- **A private repository can call a reusable workflow in a public one.** The reference lists, as one of the conditions for access: "The called workflow is stored in a public repository, and your organization allows you to use public reusable workflows". A private caller may use public or private workflows. `Itema-as` allows all actions and reusable workflows (`allowed_actions: all`), so nothing needs changing. Nothing in the docs ties this to a paid plan.
- **Permissions and OIDC.** "The `GITHUB_TOKEN` permissions passed from the caller workflow can be only downgraded (not elevated) by the called workflow." So the caller grants `contents: read`, `packages: write` and `id-token: write`, and each reusable job declares the same, so it never runs on a caller's defaults. `TestDeployWorkflowCallerMatchesTheReusableWorkflow` checks that the caller grants at least what every reusable job declares.
- **Checkout and the gate's claims.** "When a reusable workflow is triggered by a caller workflow, the `github` context is always associated with the caller workflow." `actions/checkout` with no `repository`/`ref` therefore checks out the Application repository at the triggering commit. In the OIDC token, `repository`, `repository_id`, `repository_owner_id`, `ref` and `actor` stay the caller's too, and those are all the gate checks (`internal/deploygate/gate.go`), so the gate needed no change. The token also gets `job_workflow_ref`/`job_workflow_sha`, naming this repository's workflow. The gate ignores them. It only puts `workflow_ref`, the caller's file, in the commit message.
- **A moving major tag.** The docs say `{ref}` "can be a SHA, a release tag, or a branch name", and that a tag wins over a branch of the same name. I found nothing in the docs about caching a moved tag. Not confirmed; the run records the commit it resolved to as `job.workflow_sha`, and that commit is what the install step keys on.
- **The tag filter.** `+` and `[]` are special in GitHub's filter patterns and `.` isn't. The cheat sheet has the example `v[12].[0-9]+.[0-9]+`, the same shape as the pattern used below. The cheat sheet page itself couldn't be retrieved in full.

## The shape

- **File name:** `.github/workflows/application-deploy.yaml`, not `deploy.yaml` as the issue sketched, so it isn't mistaken for a workflow that deploys this repository. `platform.DeployWorkflow` names it.
- **Inputs:** `application` and `deploy-gate-url`, both required strings.
  - The image, `ghcr.io/<owner>/<application>`, is derived from the caller's `github.repository_owner`, lowercased, which is what `iidp app create` writes to the Platform's `image.repository`.
  - The Application's name stays an input rather than coming from the repository's name, because Adopt allows `--name` to differ.
  - The gate URL stays an input rather than a default here, because the bootstrap is written for any `baseDomain`, not just `app.itma.no`.
- **The caller** (`internal/templates/deploy-workflow.yaml`): the triggers (a reusable workflow can't declare its own), one job with `uses:`, the three permissions and the two inputs. Create and Adopt both render it; the old template's `__IIDP_OWNER__`, `__IIDP_VERSION__` and `__IIDP_CLI_REPO__` placeholders, and `templates.Data`'s `Owner` and `IidpVersion`, are gone.
- **The ref the caller pins:** `@v<major>` of the iidp that rendered it (`apprepo.DeployWorkflowRef`). A dev build, or a version that isn't `X.Y.Z`-shaped, gets `@v0`.
- **Behaviour:** the same as the old full workflow. On `main`, the build job checks out, builds and pushes the SHA-tagged image, then deploys it with `iidp ci set-image <app> auto <sha>`. On a `v*` tag, the promote job checks out the tagged commit, re-tags the image without rebuilding and runs `iidp ci set-image <app> prod <version>`. Both run in a checkout of the deployed commit, so `iidp.yaml`'s migration command travels with them (#66).

## Which `iidp` runs

The install step uses `${{ job.workflow_sha }}`, which is "the commit SHA of the workflow file that defines the current job", and for a reusable workflow that is this repository's commit. It lists this repository's tags with `git ls-remote` (it's public, so no token), takes the `vX.Y.Z` tag pointing at that commit (annotated tags through their peeled `^{}` line), and downloads that release with `gh release download`. It picks a `vX.Y.Z` release, never `v0` itself, and never a pre-release. A commit no release points at (a caller pinning `@main`, or a SHA between releases) fails with a message saying to call it at `@v0` or `@vX.Y.Z`. The workflow and the CLI can't drift that way. `TestReusableDeployWorkflowInstallsItsOwnRelease` runs the step against stubbed `git`, `gh`, `tar` and `sudo`, covering annotated and lightweight tags, a pre-release-only commit and a non-release commit. The awk-and-grep resolution was also checked against the real repository: v0.2.2's commit resolves to `v0.2.2`.

The two jobs each have their own copy of the script. A reusable workflow can't share a step between its jobs without a separate action, and a local action path (`./…`) would resolve against the caller's checkout, not this repository. `TestReusableDeployWorkflowJobs` keeps the two copies identical.

## Releases move the major tag

A new `major-tag` job in `.github/workflows/release.yaml` runs after the GoReleaser and chart jobs succeed. It moves `v<major>` to the release commit, but only when the tag is absent or the release descends from where it points. A release that finishes after a newer one therefore leaves it alone, which is the out-of-order rollback other projects with moving tags have hit. Pre-releases don't move it. The job runs in a concurrency group of its own. `test/release`'s `TestMajorTagMovesForwardOnly` runs the step from `release.yaml` against throwaway repositories: first release, a newer release, an older one finishing late, and a pre-release.

The `Release` and `e2e` workflows' tag triggers were `v*`, which `v0` matches. A tag pushed with `GITHUB_TOKEN` starts no workflow run anyway, but a `v0` moved by hand would have started a release of "v0". Both now trigger on `v[0-9]+.[0-9]+.[0-9]+*` only.

**What counts as breaking for callers**, and so needs a new major tag (v1) rather than a release under v0:
- removing or renaming an input;
- adding a required input;
- needing a permission beyond `contents: read`, `packages: write` and `id-token: write`;
- needing a trigger other than a push to `main` and `v*` tags.

Everything else, including changing how a deploy works inside the reusable workflow, ships under the current major and reaches every Application on its next run. The README's "Releasing" section says the same.

## Moving existing Applications over (one time)

`hello` and `iprofil` still have the full workflow (#62's and Adopt's). Each one's `.github/workflows/deploy.yaml` is replaced whole, once there is a release carrying this change, so that `v0` points at a commit with `application-deploy.yaml`:

```yaml
name: Deploy

on:
  push:
    branches:
      - main
    tags:
      - "v*"

jobs:
  deploy:
    uses: Itema-as/iidp/.github/workflows/application-deploy.yaml@v0
    permissions:
      contents: read
      packages: write
      id-token: write
    with:
      application: <name>
      deploy-gate-url: https://deploy.app.itma.no
```

The push needs a token with the `workflow` scope, or SSH. The next push to `main` deploys through the reusable workflow. Its run shows a "deploy / Build, push and deploy" job, whose "Install iidp" step prints the release it resolved, and the Platform repository gets the usual `Deploy <name> <environment> <sha>` commit from `iidp-deploy[bot]`. That run is the proof the kind e2e can't give, since it never runs GitHub Actions.

## Tests, and what they don't cover

- `internal/templates`:
  - the caller's shape for every framework;
  - the caller checked against the reusable workflow's declared inputs and each job's permissions (the acceptance criterion's minimum);
  - the reusable jobs' order (checkout and install before `set-image`);
  - the install step's release resolution.
- `internal/apprepo`: the major tag from a version.
- `test/release`: the major-tag step.

Not covered: a real run of the reusable workflow. It needs GitHub Actions and a release tag, so the first deploy of `hello` or `iprofil` after the switch above is the end-to-end check.
