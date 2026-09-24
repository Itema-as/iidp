# #58 Applications belong to an Itema-as repository, bound by id

This note covers the questions that came up while implementing [ADR-0005](../adr/0005-private-application-repositories-on-github-free.md)'s "only `Itema-as` repositories" and "bound by numeric id" decisions in the CLI. For each one it gives the options considered and the answer chosen. The Deploy gate (#60) reads what this ticket writes. The contract it reads against is in [`docs/platform-repository.md`](../platform-repository.md#applicationsnamerepositoryyaml-the-repository-binding).

## Where the ids live: `applications/<name>/repository.yaml`

The ticket left the place and shape open. Four options were considered.

- **`values.yaml`, per Environment.** Rejected. It is the chart's input, and the chart belongs to another change (#55), so the chart would receive keys it does not use. The ids would also be written twice, once for prod and once for staging. `app add-capability --staging` copies prod's values, so it would copy the binding too. And `ci set-image` rewrites the file on every deploy, so the binding would sit in the one file the deploy path edits.
- **Labels or annotations on each `application.yaml`.** Rejected. It also duplicates the ids per Environment, and ArgoCD applies that file to the cluster, so the gate would have to parse ArgoCD manifests to authorise a deploy.
- **One map in `platform.yaml`.** Rejected. That file is Platform-wide and the bootstrap wizard writes it (#59, in parallel). Every create, bind and delete would then edit a shared file outside `applications/<name>/`, which breaks the promise that the CLI only changes files under `applications/<name>/`.
- **Chosen: one small file per Application, `applications/<name>/repository.yaml`.** The binding belongs to the Application, not to an Environment, so it sits beside `prod/` and `staging/` rather than inside them. At that depth, `bootstrap/applications.yaml`'s `*/*/application.yaml` glob never matches it, so ArgoCD never sees it. The gate needs one read of one file from a clone, which is the same way `ci set-image` already reads the repository.

The shape is three keys: `repository` (`owner/name` when bound, for people only), `repositoryId` and `repositoryOwnerId`, both YAML integers. The keys are camelCase like every other key the Platform repository uses (`baseDomain`, `installationId`), and they spell out the OIDC claim each one matches (`repository_id`, `repository_owner_id`). Integers rather than strings, because the GitHub REST API returns them as numbers. The gate converts with `strconv.FormatInt` before comparing against the string claims, and the contract says so.

The org id is recorded in every Application's file rather than once in `platform.yaml`, for three reasons. The ticket asked for it per Application. It keeps each binding self-contained. And `platform.yaml` belongs to the bootstrap wizard. The same number appears in every binding the CLI writes, because the CLI binds only repositories in `Itema-as`.

`platformrepo.RepositoryBinding` and `platformrepo.ReadRepositoryBinding` are exported, so the gate can reuse them from the same module. Every case other than `Complete() == true` is "unbound": no file, invalid YAML, a quoted id, or a missing or zero id.

## Hand edits and Applications without ids

Nothing in the CLI reads the binding except `iidp app bind`. `app add-capability`, `secret set`, `ci set-image` and a second `app create` behave identically with no file, a valid one or a broken one. `TestCommandsWorkWithAHandEditedBinding` runs add-capability, create and delete next to an unparseable `repository.yaml`. Every pre-existing add-capability, secret, delete and set-image test already starts from an Application created without `--path`, which has no binding, so that part of the acceptance criterion is covered by the existing suite. `iidp app bind` treats a binding without both ids as binding nothing, and replaces it without `--rebind`.

## `app delete` removes the binding

`iidp app delete` now removes `repository.yaml` in the same commit as each Environment's `application.yaml`. This was not in the ticket's list, but it follows from what the file is for. `values.yaml` has to stay after a delete (#39, ArgoCD's PreDelete render), and `ci set-image` today still edits a leftover `values.yaml`. If the binding also stayed, the deleted Application's old repository could keep "deploying" into a directory nothing renders. Removing the binding makes a deleted Application unbound, so the gate refuses it at its first check. The PreDelete render never reads the file, so removing it cannot block a deletion. A later `app create` under the same name already clears the whole leftover directory.

## Refusing repositories outside `Itema-as`: checked twice

`apprepo.CheckInOrg` compares the owner case-insensitively with `platform.Org`, since GitHub logins are case-insensitive. It runs in two places.

- **On `--repo` as typed.** For Adopt this is in `createOptions.plan`, before the token is read, so a refusal makes no GitHub request at all. The tests assert the fake saw zero requests, no branch was pushed and no commit was made. `iidp app bind` does the same, and the wizard's repository question uses the same check as its validator, so a wrong answer is asked again rather than failing at the end.
- **On the owner GitHub reports** (`GET /repos/{owner}/{repo}`), in `Adopter.Preview`, `Adopter.Adopt` and `iidp app bind`. GitHub redirects the name of a repository that was transferred away, so `--repo Itema-as/shop` can answer with another owner. For Adopt this runs before the push-permission check, the branch check and the clone.

The message names the repository and its owner, and says to transfer it to `Itema-as` first (Settings, Danger Zone, Transfer ownership), with the `--repo` to use afterwards.

## `--owner` is removed outright, not narrowed to `org`

A flag with one allowed value is noise, and `--owner org` was already the default. `--owner` of any value is now cobra's `unknown flag: --owner`, and `TestAppCreatePathHasNoOwnerFlag` asserts that and that `--help` no longer mentions it. A script that passed `--owner org` explicitly now breaks. That was accepted: the CLI has no stable-interface promise yet, and a loud failure is better than a hidden alias. Everything that existed only for personal accounts went with the flag:

- `github.Client.CurrentUser` and the `/user/repos` branch of `CreateRepository`.
- `apprepo.Owner`.
- The personal-account secret-and-variable note in the CLI output and in Adopt's pull request body.
- The matching paragraphs in `README.md` and the three templates' READMEs.

The default image is now always `ghcr.io/itema-as/<name>` (`platform.Registry`). The owner-derived formula from #11 collapses to that once every owner is the org.

Earlier notes that describe `--owner user` (#11, #15, #47) are left as the record of what those tickets decided. This note supersedes them on that point.

## Create and Adopt take the ids from GitHub's own response

- **Create.** `POST /orgs/{org}/repos` already returns `id` and `owner.id`, so `CreateRepository` now returns them. A response without them is refused. By then the repository exists, so `Creator.Create` returns the partial result, and the CLI prints the repository URL and the recovery steps, the same way it handles any other failure after creation (#11).
- **Adopt.** The `GET /repos/{owner}/{repo}` that Adopt already made for `default_branch` and `permissions.push` also carries `id`, `owner.id` and `full_name`. The binding records GitHub's `full_name`, not `--repo` as typed.

In both cases the binding goes in the same commit as the Environments, so an Application is never half-created with Environments but no binding. When the Platform-repository write fails after the Application repository exists, the "finish by hand" message now ends with the `iidp app bind` command to run.

Adopt binds when it opens the pull request, not when the pull request is merged. That is harmless. Until the pull request is merged, the repository has no workflow that could call the gate. And the gate authorises by id, which does not change at merge.

## `app create` without `--path` writes no binding

The legacy path has no Application repository, so there is nothing to bind. It could have been refused, since ADR-0005 makes an unbound Application undeployable once the gate ships. That was not done: the path is load-bearing for the kind end-to-end test and for most of the CLI's tests, and removing it is a separate decision. Instead it prints the `iidp app bind` command to run once a repository exists.

## Backfill: a small subcommand, `iidp app bind`

Two options were considered: documenting a hand edit, or adding a subcommand.

- **A hand edit** means looking up two ids with `gh api repos/Itema-as/<repo> --jq '.id, .owner.id'`, writing the YAML exactly, committing and pushing. That is error-prone for a file whose whole job is authorisation. It also contradicts "developers do not edit the Platform repository by hand", and the live Platform's `hello` needs it right away.
- **Chosen: `iidp app bind <name> --repo Itema-as/<repository>`.** It is about 100 lines of CLI plus the `Writer.BindRepository` it calls, and it reuses the clone, commit and retry-once logic every other command already has. It commits only `repository.yaml`, as `iidp app bind <name> <owner>/<repository>`.

Decisions inside it:

- **`--repo` is required.** It does not default to `Itema-as/<name>`. A binding grants deploy rights, and Adopt's `--name` means an Application and its repository can have different names. A wrong guess would bind the wrong repository without anyone noticing.
- **It requires an Application with a live Environment** (an `application.yaml`). Binding a directory left over from a delete makes no sense.
- **A binding to the same ids changes nothing**, and makes no commit. If only the name changed after a rename, it rewrites the file to refresh `repository`.
- **A binding to a different repository is refused unless `--rebind` is given.** The refusal names the current repository and both ids. This keeps ADR-0005's "a repository deleted and recreated under the same name cannot deploy" true until someone deliberately says otherwise. `--rebind` output names the repository that lost the Application.
- **A binding that binds nothing is replaced without `--rebind`.** That covers invalid YAML, an id missing, or an empty file. Replacing it cannot take deploy rights from anyone.
- **The Platform repository is the authorisation**, as for every other command. There is no extra check that the developer can push to the Application repository. Reading it through the API already requires seeing it.

## Tests

All tests go through the CLI seam (`cli.RunWith`) against the fake GitHub API and local bare repositories, following the repository's convention. The fake now reports stable numeric `id`, `owner.id` and `full_name` on repository creation and on `GET /repos/{owner}/{repo}`. `fakeOrgID` is the org's id. Three helpers simulate GitHub's behaviour:

- `rename` keeps a repository's id under a new name.
- `recreate` gives a repository a new id, as deleting and recreating it does.
- `transferAway` makes GitHub report another owner, as its redirect does.

Its `/user` and `/user/repos` endpoints are gone.

New and converted tests:

- **Create:** `TestAppCreatePathBindsTheApplicationRepositoryByID` and `TestAppCreatePathHasNoOwnerFlag`. The two personal-owner tests were converted or removed, and so was the `--owner user` workflow-scope test.
- **Adopt:** `TestAppAdoptBindsTheApplicationRepositoryByID`, `TestAppAdoptRefusesARepositoryOutsideTheOrgBeforeAnything` (shorthand and URL), `TestAppAdoptRefusesARepositoryGitHubReportsOutsideTheOrg` and `TestAppAdoptWizardReasksForARepositoryOutsideTheOrg`. These replace `TestAppAdoptPersonalOwnerGetsTheSecretNote`.
- **`iidp app bind`:** `app_bind_test.go` covers the backfill, a URL `--repo`, no-op and rename, `--rebind` after a recreate, replacing hand-edited bindings that bind nothing, both org refusals, a missing Application, a missing repository and a missing `--repo`.
- **Other paths:** `TestAppCreateWithoutPathWritesNoBindingAndSaysHowToBind`, `TestAppDeleteRemovesTheRepositoryBinding` and `TestCommandsWorkWithAHandEditedBinding`.

## For #60

- Read the binding with `platformrepo.ReadRepositoryBinding`, and treat anything but `Complete()` as unbound. Compare the ids to the OIDC claims as decimal strings. Never compare names.
- Unbound covers four cases, and the gate should refuse all of them with the `iidp app bind` command:
  - Applications created before this change.
  - Applications created by `app create` without `--path`.
  - Deleted Applications.
  - Hand-broken bindings.
- On the live Platform, `hello` needs `iidp app bind hello --repo Itema-as/<its repository>` before the gate ships. If its repository is not in `Itema-as`, which the acceptance run's public repository may not have been, it must be transferred first.
