# #11 CLI: Create path

Questions that came up while adding the Create path to `iidp app create`, the options considered, and the answer chosen for each.

## The flag shape: `--path create|adopt`, not a `--create` boolean

The ticket offered either. Chosen: `--path` with the two values `create` and `adopt`, only `create` implemented; `--path adopt` is refused with a message naming issue #15, and any other value is refused as unknown. A boolean `--create` would need a second, differently-shaped flag (`--adopt`?) when #15 lands, and the two could both be set at once; an enum makes "exactly one path" a property of the flag's type rather than something validated by hand. Omitting `--path` entirely keeps the exact behaviour `app create` had before this ticket: only the Platform repository is written, nothing is created on GitHub. That is deliberate compatibility, not a design position on what the default *should* eventually be — once Adopt exists, making `--path` required is worth reopening.

## `--owner` is `org` or `user`, not an arbitrary name

The ticket's own flag sketch, `--owner <org|user>`, reads as an enum rather than a placeholder for a value, and the wizard-level user story ("under the org by default or under their personal account on request") never asks for a third-party org. Chosen: `--owner org` (default) uses the compiled-in `platform.Org`; `--owner user` resolves the developer's own login with `GET /user`. Creating under an arbitrary other org is out of scope; if it is ever wanted, `user` already shows the shape (`Owner{Login, Org}` in `internal/apprepo` does not assume the org is `platform.Org`).

## `--kind` becomes conditionally required, so it moves out of cobra's `MarkFlagRequired`

Whether `--kind` is required depends on `--framework`: required when there is no `--path create` (the pre-existing behaviour) or when `--framework other`, refused as redundant otherwise (the Kind is derived). cobra's `MarkFlagRequired` cannot express a condition, so both `--name` and `--kind`'s required-ness are now checked by hand in `checkRequiredFlags`, before flag values are otherwise validated, reproducing cobra's own message shape (`required flag(s) "kind" not set`) closely enough that the pre-existing tests asserting on that substring still pass unchanged. `--framework`, `--owner`, `--private` and `--public` are refused outright (`--owner requires --path create`) when passed without `--path create`, rather than silently ignored, so a script that sets them by mistake fails loudly instead of getting a Platform-repository-only write it did not ask for.

## Lifting the Static site refusal

The chart has rendered Static site since #8; the refusal in `app.go` was `iidp`'s own leftover, not a chart limitation. Removed outright, for both the legacy path (`--kind static-site` with no `--path`) and Create (`--framework vite-react` derives it). `TestAppCreateRefusesKindsAndSizesTheChartDoesNotRender`'s "static site not yet" case is replaced by `TestAppCreateAcceptsStaticSiteKind`.

## Versions pinned in the templates

Checked against current documentation (Context7, `/vercel/next.js` and `/vitejs/vite`) rather than recalled: Next.js `^16`, React `^19`, Vite `^8` with `@vitejs/plugin-react` `^6`. Both templates are plain JavaScript, not TypeScript: the create-next-app and create-vite generators default to TypeScript, but the extra devDependencies and a type-check step buy nothing for a Dockerfile-verification fixture and would only add build time to the `templates` CI job. The Next.js Dockerfile follows the documented `output: 'standalone'` shape (`vercel/next.js/examples/with-docker`): a deps stage, a build stage, and a runner stage that copies `.next/standalone` and `.next/static` and runs as a non-root user. `npm install` is used in the builder stage rather than `npm ci`, because Create does not ship a lockfile (generating one would mean running `npm install` — network access — during `iidp app create` itself, which the CLI's design keeps free of anything but GitHub and git).

## `other` is excluded from the build verification

Its Dockerfile is a stub that documents what is missing; "the first deploy fails until it is filled in" is the point, so building it is expected to fail, not something to assert against. Both the local Podman verification (see the pull request) and the `templates` CI job cover only `nextjs` and `vite-react`.

## The `templates` CI job runs on every pull request

The ticket allowed gating with `paths` filters or an `if` on changed files, or running unconditionally "if it takes under three minutes". Measured locally with Podman: `nextjs` builds in about 15s once `node:24-slim` is cached, `vite-react` in about 10s; GitHub's runners cache base images across jobs within a run but not across runs, so a cold pull of `node:24-slim` and `nginx:1.27-alpine` is the dominant cost. That stays comfortably under three minutes even uncached, so the job runs unconditionally rather than adding a path-filtering action (or hand-rolled `git diff`) whose own failure modes (a missed rename, a shared file neither pattern lists) cost more than the job's runtime. `cmd/renderfixture` (a tiny, unreleased binary gated by nothing but its own `main` package) does the rendering `go run` needs; it is not part of the CLI's build output.

## GitHub API client: one `do` method, a `*statusError` for status matching

`internal/github.Client` (`BaseURL`, `Token`, `HTTPClient`, all overridable) wraps `net/http` directly rather than adopting `google/go-github`: four endpoints (`GET /user`, `GET /repos/{owner}/{name}`, `POST /orgs/{org}/repos` or `POST /user/repos`, `PATCH /repos/{owner}/{name}`) do not justify a generated client's dependency weight, and an injectable `BaseURL`/`HTTPClient` is exactly what the ticket asked for. A private `*statusError` carries the HTTP status so `RepositoryExists` can tell "404, does not exist" from every other failure (`github.IsNotFound`) without parsing GitHub's error body.

## Ordering: Platform-repository check, then GitHub existence check, then create

`plan()` validates flags with no network calls; `runAppCreate` then calls `platformrepo.Writer.CheckAvailable` (clone-only, no write) before `apprepo.Creator.Create`, which itself checks `RepositoryExists` before rendering or creating anything. Both checks are point-in-time: a repository or an Application directory created by someone else between the check and the write is still possible, exactly as it already was for the pre-existing Platform-repository race (handled there by the retry-once-then-fail logic in `platformrepo.CreateApplication`; there is no equivalent retry for a GitHub repository name collision, because unlike a git push there is nothing to retry against — the fix is picking a different name).

## A failed Platform-repository write after the Application repository exists is not rolled back

Per the ticket: do not delete the repository. `apprepo.Creator.Create` returns a partial `Result` (at least `URL`) alongside an error once the repository exists, even though Go convention favours a zero value on error, specifically so `runAppCreate` can tell the developer what was created. On a later failure — the Platform repository push, in particular, the case most likely to be a real permissions problem rather than a bug — the command prints the Application repository's URL and the exact files to add by hand (`applications/<name>/prod/{application.yaml,values.yaml}`, per `docs/platform-repository.md`) rather than suggesting a plain re-run, which would fail immediately on the repository-already-exists check. This is a manual recovery path, not a resume command; building the latter is more machinery than one failure mode after GitHub API + git + GitHub API + git all having already succeeded once justifies.

## What the review changed

A two-axis review (repository standards, then issue #11's acceptance criteria) was run before the pull request. Fixed: `github.Client.CreateRepository` now returns the `default_branch` GitHub gave the new repository, and `apprepo.Creator.Create` only calls `SetDefaultBranch` when it differs from `main`, matching the ticket's "set the default branch if the API did not" rather than always calling it; the recovery message for a failure inside `apprepo.Creator.Create` itself (as opposed to the Platform-repository write, handled separately above) no longer tells the developer to "re-run", which would immediately hit the repository-already-exists refusal — it now says to either push the template by hand or delete the repository first; the two Kind strings (`web-service`, `static-site`) are now the single pair of constants `platformrepo.KindWebService`/`KindStaticSite` that both `internal/cli` and `internal/templates` read, rather than being spelled twice; and template README links to issue #12 stopped naming a specific GitHub owner (`docs/design.md`'s "nothing may hardcode the current owner" applies to a user-facing link the same as to code, even though this repository has not moved to `Itema-as` yet). Left as it was: `internal/cli/app.go`'s `runAppCreate` stays one function covering the whole Create-then-write sequence; it is the ticket's documented orchestration seam, and splitting it did not make any single piece easier to test given the seam is already the whole command.

## Existing-Dockerfile detection is Adopt's job, not Create's

The ticket's "target already has a Dockerfile" acceptance criterion is about Adopt (#15): Create only ever generates a fresh repository, so there is nothing to detect. No `--from <dir>` option was added; it would let someone point Create at a local checkout and reuse its Dockerfile, but that is a different feature (closer to Adopt without the pull request) than anything #11 asks for, and speculative flags are exactly what this repository's tickets try to avoid.

## Image repository: `ghcr.io/<owner lowercased>/<name>`, computed the same way for both paths

The legacy (no `--path`) default was `platform.Registry + "/" + name`, i.e. `ghcr.io/<lowercased compiled org>/<name>`. The Create path's default in the ticket is `ghcr.io/<owner lowercased>/<name>`. These are the same formula once "owner" for the legacy path is read as the compiled org, so `runAppCreate` computes the default image from whichever owner login is in scope (the compiled org unless `--path create --owner user` resolved a personal login) rather than keeping two code paths. `--image` still overrides it outright, unchanged from before this ticket.

## Package layout

`internal/templates` (the embedded templates and their rendering), `internal/apprepo` (the Application repository writer: GitHub API create + the same git init/add/commit/push shape `internal/git` already had for the Platform repository, now via the new `git.Init`), `internal/github` gains `Client` alongside the pre-existing `TokenSource`. Consistent with #10's convention, none of these three get their own `_test.go`: the ticket's agreed seam is the CLI (`internal/cli`), driven with an in-process fake GitHub server and local bare repositories for both the Platform repository and the newly-created Application repository, in `internal/cli/app_create_create_test.go`.
