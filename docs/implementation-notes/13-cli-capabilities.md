# #13 CLI: Postgres, staging, custom domain and size Capabilities

Questions that came up while adding the Postgres, staging, custom domain and size Capabilities to `iidp app create`, and the answer chosen for each. `docs/design.md`'s wizard order and `chart/application`'s own rules (`docs/implementation-notes/07-chart-postgres.md`, `08-chart-static-domains-secrets.md`) were the sources checked; Prisma's and Drizzle's current documentation was checked with Context7 for "run migrations in production" (`prisma migrate deploy`, `drizzle-kit migrate`), confirming `docs/design.md`'s wizard text was already accurate.

## `postgres` and `domains` are written in full, even off or empty

`render.Values` was already writing every value equal to a chart default (`size`, `port`, `probe.path`) so the file reads as a complete description (#10's notes). Chosen: extend that to the two new blocks, `postgres: {enabled: false, migrationCommand: "", backupRetention: 30d}` and `domains: []`, on every Environment regardless of whether `--postgres` or `--domain` was given, rather than omitting the keys until a Capability adds them.

**Why not omit them, the way `secrets` is (absent until the first `iidp secret set`)?** `secrets` is edited into an existing file with `render.AddSecretName`, a node-level YAML edit, because the CLI cannot re-derive the whole values file at that point without knowing every other value already written. `postgres` and `domains` are written by `app create` itself, which already knows the whole shape; there is nothing to preserve by leaving them out. Writing them in full also means an Environment without Postgres or a custom domain still shows a developer reading `values.yaml` in the Platform repository exactly what the chart accepts, matching #10's stated reason for writing `size`/`port`/`probe.path` even at their defaults.

**Consequence for issue #17 (add-capability).** Because every Environment's `values.yaml` already has `postgres.enabled` and `domains`, add-capability's "refuses Capabilities already present" check is a read of those two keys, not a presence check; it needs no change to this ticket's file shape.

## `platform.backupsBucket`/`objectStorageEndpoint` are the exception: written only with `--postgres`

Unlike `postgres`/`domains`, `platform.httpIssuer` and `platform.backupsCredentialsSecret` were never written by the CLI at all (#7, #8): they are Platform-wide constants the chart defaults on its own, not something a developer's flags decide. `platform.backupsBucket` and `platform.objectStorageEndpoint` are the same kind of value — read from `platform.yaml`, not chosen per Application — so they follow that precedent: present only when `--postgres` needs them (`platformValues` uses `omitempty`), absent otherwise. Chosen over writing them unconditionally (which would put a Platform-wide setting in every Application's file whether or not it uses a database, the opposite of #8's reasoning for leaving `httpIssuer` out).

## `objectStorageEndpoint` is a new `platform.yaml` field; `cloudflareZone` and `backupsBucket` were not

`bootstrap/README.md`'s `platform.yaml` table already documented `cloudflareZone` and `backupsBucket` as "Read by: ... CLI" (the bootstrap ticket anticipated this one), but `internal/platformrepo.Config`, the CLI's typed struct, had neither field yet — this ticket adds both, plus `objectStorageEndpoint`, which no document had: the chart's `platform.objectStorageEndpoint` (`chart/application/values.yaml`, already present since #7) needs an S3 endpoint the CLI did not previously have anywhere to read from. Added to `bootstrap/README.md`'s table, `docs/platform-repository.md`'s table, `internal/platformrepo.Config`, and the e2e fixture `test/e2e/fixtures/platform-repo/platform.yaml` (which already carried `backupsBucket` and `cloudflareZone` but not `objectStorageEndpoint`). None of the three is required by `LoadConfig`: an Application that uses neither Capability needs neither field, the same reasoning `agePublicKey` already gets ("not required to create an Application").

## Where Postgres/domain validation runs: after the clone, like `chartVersion`

`--size`, `--port`, `--probe-path` and the new `--migration-command`-without-`--postgres` and `--postgres`-on-a-static-site checks are flag-only and run in `createOptions.plan`, before anything is cloned, matching the existing pattern. The Postgres bucket/endpoint check and all of `--domain`'s validation need `platform.yaml` (`baseDomain`, `cloudflareZone`) and, for the "equal to a Platform address" rule, whether `--staging` is also set — none of which is known before the Platform repository is cloned. Chosen: validate them in `platformrepo.Writer.attemptCreate`, in the same place and the same way the pre-existing `chartVersion` check already runs, after `LoadConfig` and before anything is written. The consequence is the one #11 already documented for `chartVersion`: with `--path create`, a domain or Postgres-configuration error is reported after the Application repository exists, through the same "here is what was created, finish by hand" message `runAppCreate` already prints on any Platform-repository-write failure. No new reporting path was needed.

## Capability logic lives in functions that take plain values, not an `Application`

Issue #17 (add-capability) will need to add a Capability to an Environment that already exists, not only a fresh one. Two functions were kept independent of "creating a new Application" so #17 can call them again later:

- `platformrepo.ValidateDomains(domains []string, baseDomain, cloudflareZone string, platformAddresses []string) ([]DomainPlan, error)` takes the Platform addresses it must not collide with as plain strings, not a `platformrepo.Application`, so it works the same whether those addresses come from a fresh Create plan or from an Environment `add-capability` reads off disk.
- `migrate.Detect(dir string) (Detection, bool, error)` takes a directory, not an `apprepo.Result` or a `platformrepo.Application`: it works against the Create path's freshly rendered template today and, unchanged, against an Adopt or add-capability checkout later.

`platformrepo.Writer.attemptCreate`/`writeEnvironment` still assemble a fresh `render.Environment` from a `platformrepo.Application` on every call, rather than reading an existing `values.yaml` back into one — building that read-modify-write path is #17's job, once there is a second caller to design it against.

## Migration detection: a scratch render for `--path create`, not the repository Create already pushed

`apprepo.Creator.Create` renders the framework template into a temporary directory, pushes it, and removes the directory (`defer os.RemoveAll`) before `runAppCreate` gets a `Result` back — there is no window in which to inspect it for Prisma/Drizzle files afterwards. Chosen: `detectMigrationCommand` (in `internal/cli`) renders the same framework template into a second, throwaway temporary directory purely to look for migration tooling, before any GitHub call is made, rather than changing `apprepo.Creator`'s lifecycle to keep its directory around longer. This is cheap (the templates are a handful of embedded files) and keeps `apprepo.Creator` unchanged.

In practice this rarely finds anything: neither built-in template (`internal/templates/nextjs`, `internal/templates/vite-react`) ships a Prisma schema, a Drizzle config or a `migrate` script, so `--path create` detection almost always reports "none detected" today. Detection is mainly useful with `--app-dir` (a hidden flag pointing at an existing local checkout) or, without `--path create`, the current directory when it has a `package.json` — the shape issue #17 and the eventual Adopt path (#15) will run detection against for real.

## `--app-dir` is hidden, the same reasoning as `--platform-repo`

Not a wizard question (`docs/design.md`'s wizard has none for it), but a real and honest way to point detection at a directory other than the current one or the Create path's template, the same justification #10 gives for keeping `--platform-repo` around as a hidden flag rather than removing it.

## Detection order and commands match `docs/design.md`'s wizard text

Prisma (`prisma/schema.prisma` or `schema.prisma`) before Drizzle (`drizzle.config.*`) before an npm `migrate` script, `npx prisma migrate deploy` and `npx drizzle-kit migrate` respectively — the exact order and commands `docs/design.md`'s "The wizard" section already specified. Context7 (`/prisma/skills`, `/drizzle-team/drizzle-orm-docs`) confirmed both remain the documented commands for applying migrations in production/CI as of 2026-09-21, so no change was needed to what was already designed, only independent confirmation.

## Domain classification: `Wildcard` and `Automated` are independent

`chart/application/templates/_helpers.tpl`'s `application.domains` decides, per host, only whether the wildcard certificate or `platform.httpIssuer` serves it (`Wildcard` here, computed with the identical rule: one label directly under `baseDomain`). Whether external-dns can create the DNS record at all is a separate question the chart never answers, because it has no `cloudflareZone` value — only the CLI does. `Automated` is `Wildcard` (baseDomain's own zone) or the host sitting inside `cloudflareZone`; a host can be foreign to the wildcard (its own certificate) yet still automated (`shop.itma.no` when `cloudflareZone: itma.no` — the same example `08-chart-static-domains-secrets.md` uses for "foreign to the wildcard but on the Platform's own zone"), and the closing summary reports the two independently: which certificate branch, and whether DNS needs a CNAME.

## Domain equal to a Platform address: both Environments, checked by the CLI

The chart itself only refuses a domain equal to *that Environment's own* Platform address (08's notes: the other Environment's address is not the chart's problem, since a single values file does not see both). `--domain` only ever writes to prod, but a value equal to *staging's* address is still wrong once `--staging` is also given, and only the CLI knows both Environments exist at once. `ValidateDomains` takes every requested Environment's address as `platformAddresses` for exactly this reason.

## `--size` needed no new code

The three-value validation the ticket asks for (`small`, `medium`, `large`, clear error otherwise) already existed since #10/#11 (`createOptions.plan`, the `sizes` slice) and already applies to every Environment `writeEnvironment` writes, staging included, once this ticket makes writing staging possible. Covered by the pre-existing `TestAppCreateRefusesKindsAndSizesTheChartDoesNotRender/unknown_size` case; no new test was needed to establish it, though `TestAppCreateAllCapabilitiesCombined` exercises `--size medium` alongside every other Capability.

## Testing

Every new behaviour is driven through `cli.Run`/`cli.RunWith` (`internal/cli/app_create_capabilities_test.go`), the pre-agreed CLI seam, against the same bare-Platform-repository and fake-GitHub-server fixtures #10/#11 established. No package gained its own `_test.go`: `internal/migrate` and the domain logic in `internal/platformrepo` follow the layout `apprepo`/`templates` already set (#11's notes) — tested at the seam the ticket names, not unit-by-unit. `internal/render`'s `ExampleValues` was updated in place for the new always-written keys, the same way its literal output is meant to change whenever the file's shape does.
