# #66 The migration command lives in the Application repository and travels with every deploy

This note records the decisions taken while moving an Application's migration command out of the Platform repository and into the Application repository. The design was agreed on the issue; what follows is what the issue left open and what was chosen. The contract (the file, what a deploy carries, what the Deploy gate does with it) is in [`docs/platform-repository.md`](../platform-repository.md#the-migration-command-travels-with-the-deploy). ADR-0005 has a short note on why the gate may now set the command ([ADR-0005](../adr/0005-private-application-repositories-on-github-free.md#note-66-the-gate-also-sets-the-callers-own-migration-command)).

## The problem, in one paragraph

`postgres.migrationCommand` was stored per Environment in the Platform repository and set once, at `app create`. Nothing in the CLI could change it: `add-capability --postgres --migration-command` refuses Postgres that is already there. The command belongs to the code, though, and the two Environments run different code: after a push to `main`, staging runs the new image and prod the last release, until the next `v*` tag. Changing a Platform-side command for both at once runs it against the wrong code in one of them, and changing it before the image with the tooling is deployed fails that Environment's sync. That happened on the real Platform with `hello`.

## The file: `iidp.yaml` at the repository root

The issue proposed `iidp.yaml` and left the name open. Kept, for four reasons:

- It says which tool reads it, the way `platform.yaml` and `values.yaml` do on the Platform side.
- `.yaml`, not `.yml`, like every other YAML file iidp writes.
- Not hidden (`.iidp.yaml`). Developers are meant to edit it, and a dotfile is easy to miss in a listing and in GitHub's file view.
- At the root, not under `.github/`. It is not GitHub configuration, and the deploy workflow's checkout already has the root in its working directory.

`iidp.yml` is not read, but a repository that has one and no `iidp.yaml` fails the deploy with "rename it to iidp.yaml". Ignoring it silently would drop the command without a word.

**Format.** A YAML mapping whose only key today is `migrationCommand`, a string or null. The file is meant to take later code-coupled settings (port, probe path), which are out of scope here. Any other key is refused, naming the line, so a misspelt `migrationComand` fails the deploy instead of clearing the command. That also means a file written for a newer `iidp` fails an older one loudly rather than being half-read. The value is trimmed, so a folded block scalar (`>`) reads as the one line it folds to. A number or boolean that YAML would read as something else is refused with "quote it". An empty file, or one with only comments, is a file without the line.

**Rules on the command.** One line, with no control character but a tab, valid UTF-8, at most 1024 bytes (`appconfig.ValidateMigrationCommand`). It runs as `sh -c <command>`, so a shell line is what it is. One line keeps it readable in `values.yaml`, in the commit message the gate writes, and in the Job's spec; `&&` covers chaining. 1 KiB is far above any real migration command (`npx prisma migrate deploy && node scripts/seed.js` is 48 bytes), and anything longer belongs in a script shipped in the image. The same function runs in `ci set-image` before any request, in `app create` and `add-capability` on `--migration-command`, and in the gate, which is the authority.

The code is `internal/appconfig`: `Read`, `Parse`, `ValidateMigrationCommand`, `Render` (the file `app create` and Adopt write) and `MigrationCommandLine` (the line `add-capability` prints).

## Absent and empty are different on purpose

| The deployed commit has | `ci set-image` sends | The gate |
|---|---|---|
| no `iidp.yaml` | no `migrationCommand` key | keeps the Environment's command |
| `iidp.yaml` without the line, or `""`, or null | `"migrationCommand": ""` | clears it |
| `iidp.yaml` with `migrationCommand: <command>` | `"migrationCommand": "<command>"` | sets it |

On the wire, `deploygate.Request.MigrationCommand` is a `*string` with `omitempty`, so nil leaves the key out and a pointer to `""` sends `""`. `platformrepo.ImageTagChange.MigrationCommand` carries the same pointer to the write.

The file's presence is the switch. The issue asks for both "Applications without the file keep working" and "removing the line means no migration", and only the file can tell them apart: an Application created before this change, like `hello`, has a command on the Platform and no file, and must keep it. Once a repository has the file, the file is the whole truth, and deleting the line clears the command. Deleting the whole file goes back to "keep". That is documented in the file's own comment, which is the one place a developer is sure to read.

## The CLI no longer writes the Platform-side command

The issue makes `postgres.migrationCommand` an output the gate writes. So `platformrepo.Application` and `platformrepo.Capabilities` lost their `MigrationCommand`, `render.EnablePostgres` lost its parameter, and every Environment is created with `migrationCommand: ""` as before.

- **Create (`--path create`)** writes `iidp.yaml` into the generated repository with the given or detected command. The first deploy carries it to the Platform with the first image. Until then the Environment has no image and renders nothing ([47](47-unreleased-environment.md)), so nothing is lost by not writing it at create.
- **Adopt (`--path adopt`)** adds `iidp.yaml` in its pull request, next to the deploy workflow, and like the workflow it never writes over one the repository already has. The command is the given `--migration-command`, or, with `--postgres`, the one detected in the clone, or none (`apprepo.Detection.MigrationCommand`). Without `--postgres` nothing is written even when tooling is detected, since the gate would refuse it; the pull request body names the line to add later. When the repository already has an `iidp.yaml` and a command was resolved, the CLI prints the line to check for. An existing Dockerfile is still never touched. A repository with a Dockerfile and a workflow but no `iidp.yaml` now gets a pull request adding just `iidp.yaml`, where before Adopt refused with "nothing to add".
- **Without `--path`** there is no Application repository, so the CLI prints the line to add, the same way `add-capability` does.
- **`add-capability --postgres`** cannot write the developer's repository, and must not write the Platform's: the command would then run against the image each Environment already runs, which is exactly the failure behind this issue. It prints the line to add, filled with `--migration-command`, the detected command, or an example. `--migration-command` stays, for that line.

Seeding the Platform-side value at `app create` as well was considered. It would be harmless for a new Application but redundant, and a second writer is what the issue removes.

**`iidp.yaml` is written for every Kind**, a Static site included. The comment explains the file and the command, and the line stays commented out. That keeps one shape for every generated repository, and gives later settings (a port, a probe path, which do apply to Static sites) a file to go into.

## The deploy: `ci set-image` reads the file in its working directory

`ci set-image` reads `./iidp.yaml`. The deploy workflow runs it in its checkout, so that is the deployed commit's file. No flag was added: the workflow is the only caller, and the tests change directory (`t.Chdir`). A file it cannot read fails the command before it asks for a token, so nothing is deployed with a command that was misread.

It prints which case it is in ("Migration command from iidp.yaml: ...", "iidp.yaml sets no migration command", "No iidp.yaml here, so the migration command on the Platform is left as it is"), and "The migration command changed with it" when the gate's answer says so (`migrationCommandChanged`).

## The promote job checks out the tagged commit

The promote job only retagged the image before, so it had no checkout. It now starts with `actions/checkout@v7` and no `ref:`. On a `v*` tag push the triggering ref is the tag, so the checkout is the tagged commit, and promoting an older tag brings that tag's command. The job already had `contents: read`. The workflow-template test asserts a checkout before `iidp ci set-image` in both jobs, and no `ref:` override.

## The gate

The gate change is kept to the deploy call's new field and its rules, so it merges cleanly with #61's registry check:

- `Request.MigrationCommand` is validated with the tag, before anything is cloned: 400 when it is not one short line.
- The Postgres check needs the Environment's `values.yaml`, so it runs in `platformrepo.SetImageTag`, on the clone the write is made from, after the gate's own checks have chosen the Environment. A caller the gate would refuse never learns whether an Environment has Postgres, and the command only ever reaches the Environment that caller may deploy. `render.SetMigrationCommand` refuses a non-empty command where `postgres.enabled` is not `true`, and `platformrepo` wraps that as `ErrPostgresMissing`, which the gate answers with **409 Conflict**: "... Add the Postgres Capability first (iidp app add-capability <name> --postgres), or remove migrationCommand from iidp.yaml". 409, not 400 or 403: the request is well-formed and allowed, but conflicts with the Environment's current state, and the same request succeeds once Postgres is added.
- Clearing is always allowed. On an Environment without Postgres there is nothing to clear, so it changes nothing, and every Static site with a generated `iidp.yaml` deploys as before.
- One commit, "Deploy <app> <environment> <tag>", holds both edits. When the command changed, the body's first line says so ("Migration command, from iidp.yaml: ..." or "Clear the migration command: iidp.yaml sets none."), so the Platform repository's log shows when a command changed. The subject is unchanged, so anything matching on it keeps working.
- "Unchanged" now means the tag and the command. A call with the tag the Environment already runs and another command commits the command. The generated workflow never makes one, since every commit has its own tag. ArgoCD leaves hooks out of its diff, so such a commit alone would not start an automated sync, and the command would run with the next rollout. That is also why the kind e2e deploys a new tag with the command.
- `Response.MigrationCommandChanged` reports it. The existing fields are untouched.

**Old and new together.** The gate decodes with `DisallowUnknownFields`, so a gate from before this change answers 400 "unknown field migrationCommand" to a CLI that sends one. The CLI sends it only when the deployed commit has an `iidp.yaml`, so the order is: the Platform's bootstrap (and so its gate) first, then the workflows' `iidp` version, then the file. A new gate with an old CLI works: no key, nothing changes.

## Tests

- **The gate** (`internal/deploygate/migration_test.go`, through the HTTP boundary with the existing fake issuer, fake GitHub and bare Platform repository): the command is written with the tag in one commit touching only that Environment's `values.yaml`, with the rest of the file kept; a repeat is unchanged; a new command on the same tag commits; no command leaves the stored one alone; `""` clears it to `""` (not null); a main deploy sets staging's command and leaves prod's until a `v*` tag promotes; 409 without Postgres, with nothing committed, while `""` there is fine; 400 over 1024 bytes, on two lines and on a control character, and 1024 bytes exactly accepted; a caller from another repository is refused 403 before any of it. It sits in its own file next to `gate_test.go`, reusing its helpers, so #61's changes to that file and this one do not collide.
- **`render.SetMigrationCommand`** (`internal/render/edit_test.go`): set, repeat, clear, a command that looks like a boolean stays a string, and the refusal without Postgres.
- **`ci set-image`** (`internal/cli/ci_set_image_test.go`, through `cli.RunWith`): the command sent for a plain, a folded, a missing, an empty and a null line, the key left out with no file, and each case's message; refusals, before any token request or call, for an unknown setting, a list, two lines, invalid YAML and `iidp.yml`.
- **Create, Adopt and add-capability** (through `cli.Run`/`cli.RunWith`): `--path create` pushes `iidp.yaml` with the given command, or the commented example without Postgres; the wizard's accepted suggestion lands in `iidp.yaml`; Adopt's pull request adds `iidp.yaml` with the detected command and describes it, prefers a given command, leaves an existing file alone and prints the line to check for, and writes no command without `--postgres`; `add-capability --postgres` prints the line (given, detected or an example); a multi-line `--migration-command` is refused by both commands; and every one of them leaves `postgres.migrationCommand` at `""`.
- **The workflow template**: a checkout before `iidp ci set-image` in both jobs, without a `ref:`, and every framework gets `iidp.yaml` with its explanation.
- **The kind e2e** (`testMigrationCommandFromIidpYAML`): `shop` is now bound (`applications/shop/repository.yaml`, repository id 700000002). The test reads `test/e2e/fixtures/shop-repository/iidp.yaml` with `appconfig.Read`, the code `ci set-image` uses, and sends its command through Traefik with a deploy of `shop` from `main` (prod, since `testDeleteEnvironment` removed staging), tag `1.27.0-alpine`. The tag is new so the Deployment changes too and the automated sync starts, and it is a real Docker Hub tag, since the gate checks every new tag against its registry (#61). It asserts one commit touching only shop's prod `values.yaml` and naming the command, then that the recreated `shop-migrate` Job runs exactly that command, succeeds and logs the marker the command echoes, and that `shop-prod` is Healthy. Before that, the same command for `brochure`, which has no Postgres, is refused with 409. The e2e cannot run `ci set-image` itself: the gate is reached at `127.0.0.1` with a `Host` header and a self-signed certificate, which the CLI's HTTP client rightly refuses. It was not run locally; CI runs it.

## What was not built

- **Port, probe path and other settings in `iidp.yaml`.** Only the migration command is in scope; the format leaves room.
- **Checking that the command's tool exists in the image.** The migration Job finds out, and a failure stops the rollout with the previous version still running.
- **A flag on `ci set-image` for another directory.** The workflow is the only caller.

## What `hello` needs to use it

`hello` keeps working as it is: its workflow's pinned `iidp` does not read `iidp.yaml`, and a deploy without the file keeps the command it has on the Platform. To move its migration command into its repository:

1. **The Platform first.** Tag a release with this change, and point the Platform repository's `bootstrap/platform-components.yaml` `targetRevision` at it, so the Deploy gate understands `migrationCommand`. An older gate refuses a deploy that sends one, with 400 "unknown field".
2. **`hello`'s workflow.** Regenerate it from that release, or by hand in `.github/workflows/deploy.yaml`:
   - Change `version="<old>"` in both "Install iidp" steps to the release.
   - Add, as the first step of the `promote` job:
     ```yaml
     - name: Checkout the tagged commit
       uses: actions/checkout@v7
     ```
3. **`hello`'s `iidp.yaml`.** Add it at the repository root with the new command, `migrationCommand: <command>` (the comment `iidp app create` writes is in `internal/appconfig.Render`), and push to `main` together with the change that needs it. The push deploys staging, or prod when `hello` has no staging, with the new image and the new command in one Platform commit. With staging, prod keeps its old command until the next `v*` tag promotes the commit that has the file.
