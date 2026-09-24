# The Platform repository

The Platform repository (`Itema-as/iidp-platform`) holds the desired state of every Application on the Platform. The CLI writes to it, the deploy workflows write image tags into it, ArgoCD reconciles the Platform from it, and its git log is the audit trail ([ADR-0002](adr/0002-cli-writes-desired-state-to-platform-repository.md)). Developers do not edit it by hand; hand edits are tolerated but unsupported. This page is the contract between the things that touch it: the CLI (`internal/platformrepo`), the bootstrap, the deploy workflow, and the Deploy gate, which reads each Application's repository binding ([`applications/<name>/repository.yaml`](#applicationsnamerepositoryyaml-the-repository-binding), [ADR-0005](adr/0005-private-application-repositories-on-github-free.md)).

## Layout

```
platform.yaml                 Platform-wide settings the CLI reads
bootstrap/                    ArgoCD Applications for the Platform components (the bootstrap ticket)
  backups-credentials.enc.yaml  the Platform's Object Storage keys, written once by the wizard (below)
applications/
  .gitkeep
  <name>/                     one directory per Application
    repository.yaml           the Application repository it is bound to, by GitHub's numeric ids
                               (below); absent on an Application that is not bound yet
    prod/
      application.yaml        the ArgoCD Application for the prod Environment
      values.yaml             the chart values that define the prod Environment
      sops/                   SOPS-encrypted secrets for this Environment, once any are set
        kustomization.yaml    generators: [ksops.yaml]
        ksops.yaml            the KSOPS generator listing every *.enc.yaml below
        <key-slug>.enc.yaml   one SOPS document per secret KEY
        backups-credentials.enc.yaml  present once Postgres is enabled: a byte-for-byte
                               copy of bootstrap/templates/backups-credentials.enc.yaml (below)
    staging/                  the same files, when the Application has a staging Environment
                               (--staging on iidp app create), its own address
                               (<name>-staging.<baseDomain>) and its own database
```

`iidp app delete` no longer writes a `Backup` manifest or a `final-backup-<environment>` Application of its own: the final Postgres backup is now taken by an ArgoCD `PreDelete` hook the chart itself renders (`chart/application/templates/final-backup-job.yaml`). It removes only each Environment's `application.yaml` (and the Application's `repository.yaml`), deliberately leaving `values.yaml` (and any `sops/` secrets) behind -- ArgoCD needs `values.yaml` to still exist to render that PreDelete hook at deletion time; see "`iidp app delete`" below and [`docs/implementation-notes/39-final-backup-predelete-hook.md`](implementation-notes/39-final-backup-predelete-hook.md).

The CLI only ever adds and changes files under `applications/<name>/`. Anything else in the repository is left alone, so an emergency hand edit elsewhere does not break the next `iidp` run.

## `platform.yaml`

Read from the root of the repository on every run, so changing a Platform-wide setting is a commit, not a CLI release. The CLI parses it into a small typed struct and ignores fields it does not know, so other tickets can add fields without breaking older CLI releases.

| Field | Required | Meaning |
|---|---|---|
| `baseDomain` | yes | The Platform base domain, for example `app.itma.no`. prod Environments are reachable at `<name>.<baseDomain>`, staging at `<name>-staging.<baseDomain>`. Written into every values file as `platform.baseDomain`. |
| `chartVersion` | yes | The version of the generic chart new Environments are pinned to, for example `0.3.1`. Bumping it changes what new Applications get; existing Applications keep their pin until it is changed in their `application.yaml`. |
| `chartRepository` | no | The OCI reference of the generic chart. Default `oci://ghcr.io/itema-as/charts/application`, where the release workflow pushes it. |
| `argocdURL` | no | Printed by the CLI as where to look at an Application's status. |
| `grafanaURL` | no | Printed by the CLI as where to look at an Application's logs. |
| `agePublicKey` | only for `iidp secret set` | What `iidp secret set` encrypts values with. The matching private key exists only in the cluster (`bootstrap/README.md`, Secret `argocd/sops-age`); the CLI never sees it. Not required to create an Application. |
| `cloudflareZone` | only for `--domain` | The Cloudflare zone containing `baseDomain`. A `--domain` host inside it is fully automated: external-dns creates the record. Not required without `--domain`. |
| `backupsBucket` | only for `--postgres` | The Object Storage bucket every Application database is backed up to, written into the Environment's `values.yaml` as `platform.backupsBucket`. Not required without `--postgres`. |
| `objectStorageEndpoint` | only for `--postgres` | The S3 endpoint of `backupsBucket`'s location, for example `https://hel1.your-objectstorage.com`, written as `platform.objectStorageEndpoint`. Not required without `--postgres`. |
| `githubApp.id`, `githubApp.installationId` | documentation only | The org GitHub App the deploy workflow's write-back authenticates as (`bootstrap/README.md`), and the id of its installation on the org, recorded here for a human to see which App and installation are in play. Not required for anything: `iidp ci set-image` authenticates from `IIDP_DEPLOY_APP_ID` (an org Actions variable) and a GitHub API lookup instead, precisely so it never has to read this file before it has a credential to read it with (see "How the deploy workflow writes back" below). |

```yaml
baseDomain: app.itma.no
chartVersion: 0.3.1
chartRepository: oci://ghcr.io/itema-as/charts/application
argocdURL: https://argocd.platform.itma.no
grafanaURL: https://itema.grafana.net
agePublicKey: age1kpq9t46wreydm6dp2e9a6txzm88ymqj9ph38jvjlsjgff3k5vfqqqhee6v
cloudflareZone: itma.no
backupsBucket: itema-iidp-db-backups
objectStorageEndpoint: https://hel1.your-objectstorage.com
githubApp:
  id: 123456
  installationId: 78901234
```

## `applications/<name>/repository.yaml`: the repository binding

Binds the Application to its Application repository by GitHub's numeric ids, which survive a rename and a transfer. A repository deleted and recreated under the same name gets a new id, so it is a different repository. The Deploy gate lets a deploy through only when the caller's GitHub Actions OIDC token carries these ids ([ADR-0005](adr/0005-private-application-repositories-on-github-free.md)). There is one file per Application, not per Environment, because the binding belongs to the Application. It sits directly under `applications/<name>/`, one level above the Environments, so `bootstrap/applications.yaml`'s `*/*/application.yaml` glob never matches it and ArgoCD never applies it.

```yaml
# The Application repository shop is bound to. The Deploy gate lets only the
# repository with these GitHub ids (an Actions OIDC token's repository_id and
# repository_owner_id) deploy it; repository is the name when it was bound,
# for people to read, and goes stale on a rename.
# Written by iidp (iidp app create, iidp app bind); do not edit by hand.
repository: Itema-as/shop
repositoryId: 812345678
repositoryOwnerId: 123456789
```

| Field | Type | Meaning |
|---|---|---|
| `repositoryId` | YAML integer | The Application repository's `id` from the GitHub REST API (`GET /repos/{owner}/{repo}`). The same number an Actions OIDC token carries as `repository_id`. |
| `repositoryOwnerId` | YAML integer | The id of the org that owns it (`owner.id`), which an OIDC token carries as `repository_owner_id`. The CLI binds only repositories in `Itema-as`, so this is `Itema-as`'s id in every binding it writes. |
| `repository` | string | `owner/name` as GitHub reported it when the binding was written. It is only for people: it goes stale on a rename or transfer, and nothing may authorise with it. |

**Who writes it.** `iidp app create --path create` and `--path adopt` write it in the same commit as the Application's Environments. The ids come from GitHub's response to creating the repository, or to reading it (`GET /repos/{owner}/{repo}`). `iidp app bind` writes it for an Application that already exists (the backfill, below). `iidp app create` without `--path` has no Application repository and writes no binding; it prints the `iidp app bind` command instead. `iidp app delete` removes the binding in the same commit as the Environments' `application.yaml`, so a deleted Application is unbound. When `iidp app create` reuses the name of a deleted Application, it clears the leftover directory, as described under "`iidp app delete`" below. Nothing else in the CLI reads the file. `app add-capability`, `secret set`, `app delete` and `ci set-image` all work the same on an Application with no binding, or with a broken one.

**How the Deploy gate reads it (#60).** For the Application named in a deploy request, from a clone of `main`:

1. Read `applications/<name>/repository.yaml`. No file means the Application is **unbound**. Applications created before #58, by `app create` without `--path`, or deleted are all unbound.
2. Parse it as YAML into the three fields above, ignoring any other keys. A file that is not valid YAML, has an id quoted as a string, or has either id missing or zero is also **unbound**. `platformrepo.ReadRepositoryBinding` (`internal/platformrepo/binding.go`) implements this, and `RepositoryBinding.Complete()` reports whether both ids are present. The gate is built from this repository, so it can call them directly.
3. Refuse an unbound Application, naming the fix: `iidp app bind <name> --repo Itema-as/<repository>`.
4. OIDC claims are decimal strings. Allow the deploy only when `repository_id == strconv.FormatInt(repositoryId, 10)` **and** `repository_owner_id == strconv.FormatInt(repositoryOwnerId, 10)`. Never compare the `repository` or `repository_owner` names: they change on a rename, and a recreated repository reuses them.

The gate can also check `repository_owner_id` against `Itema-as`'s id from its own configuration. That defends against a hand edit that binds a repository outside the org, although anyone able to make that edit can already write to the Platform repository.

**Backfilling an existing Application.** An Application made before #58 has no binding, and neither does one made without `--path`. Once its repository is in `Itema-as`, bind it:

```sh
iidp app bind hello --repo Itema-as/hello
```

`--repo` takes `Itema-as/<name>` or a GitHub URL. The command refuses a repository outside `Itema-as` before it reads or writes anything, and tells you to transfer the repository first. It reads the repository's ids from GitHub with your `gh` login, refuses an Application with no live Environment, and commits `applications/<name>/repository.yaml` alone as `iidp app bind <name> <owner>/<repository>`, retrying once if `main` moved. If the Application is already bound to the same ids, nothing changes, or only `repository` is refreshed after a rename. A binding to a different repository, including one that was deleted and recreated under the same name, is refused unless you pass `--rebind`. A binding that binds nothing (not YAML, or an id missing) is replaced without `--rebind`. Writing the file by hand in the shape above also works, but the command reads the ids from GitHub rather than asking you to look them up. [`docs/implementation-notes/58-repository-binding.md`](implementation-notes/58-repository-binding.md) records why this is a subcommand.

## `applications/<name>/<environment>/application.yaml`

One ArgoCD `Application` per Environment, in the `argocd` namespace and the `default` ArgoCD project. It has two sources, ArgoCD's multi-source pattern for a Helm chart with values from git: the chart from the OCI registry at the pinned version, and the Platform repository itself under `ref: values`, so the chart's `valueFiles` can name the values file in the same directory as `$values/<path>`.

```yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: shop-prod
  namespace: argocd
  labels:
    iidp.itema.no/application: shop
    iidp.itema.no/environment: prod
  finalizers:
    - resources-finalizer.argocd.argoproj.io
spec:
  project: default
  sources:
    - repoURL: ghcr.io/itema-as/charts
      chart: application
      targetRevision: 0.3.1
      helm:
        valueFiles:
          - $values/applications/shop/prod/values.yaml
    - repoURL: https://github.com/Itema-as/iidp-platform.git
      targetRevision: main
      ref: values
  destination:
    server: https://kubernetes.default.svc
    namespace: shop-prod
  syncPolicy:
    automated:
      prune: true
      selfHeal: true
    syncOptions:
      - CreateNamespace=true
```

- The Application is named `<name>-<environment>` and installs into a namespace of the same name, created by ArgoCD on the first sync. Every Environment therefore has its own namespace.
- The kind end-to-end test's fixture Application deviates in one field only: the chart source's `repoURL`, `targetRevision` and `path` point at this repository's `iidp.git`, served in-cluster, at `path: chart/application`, instead of the OCI reference, because kind has no GHCR to pull it from. Everything else, including the second source and the `$values/...` valueFiles pattern, is exactly as below.
- `repoURL` of the chart source is `chartRepository` from `platform.yaml` without the `oci://` scheme and without the last path segment, which becomes `chart`. `targetRevision` is `chartVersion` at the time the Environment was created.
- Sync is automated with prune and self-heal, so a commit is a deploy and a hand change in the cluster is reverted.
- The resources finalizer makes deleting the ArgoCD Application delete the Environment's resources, which is what `iidp app delete` will rely on.
- The labels carry the same Application and Environment identity the chart puts on every object.
- Once the Environment has at least one secret, `iidp secret set` adds a third source, the same repository at `main` with `path: applications/<name>/<environment>/sops` and no `ref` and no `chart`: ArgoCD detects the `kustomization.yaml` there and renders it with KSOPS, the same way `bootstrap/platform-secrets.yaml` renders `bootstrap/sops/` (`bootstrap/README.md`). It is added once, after the first secret, and left alone after that.

What the bootstrap must provide for this to reconcile: the `default` ArgoCD project (or a stricter one, if the bootstrap changes `project` here and in the CLI together) allowed to use both source repositories and to deploy to any namespace on the in-cluster server; and ArgoCD credentials for the Platform repository and, if the chart package on GHCR is private, for the OCI registry. Nothing has to be replicated into Environment namespaces: the Platform's wildcard certificate is Traefik's default certificate (a `TLSStore` named `default`, see the chart README), so a new namespace needs no secret of its own.

Nothing makes ArgoCD notice a file under `applications/` by itself: the root Application only syncs `bootstrap/` (see [`bootstrap/README.md`](../bootstrap/README.md)). `bootstrap/applications.yaml` is the file that does, one more hand-written Application next to `platform-components.yaml` and `platform-secrets.yaml`: a directory source on the Platform repository itself, path `applications`, `directory: {recurse: true, include: '*/*/application.yaml'}`. Every Application repository written by the CLI is picked up as soon as it is pushed, with no further wiring; the fixture Platform repository ([`test/e2e/fixtures/platform-repo`](../test/e2e/fixtures/platform-repo)) carries this file, and the kind end-to-end test proves it works. See [`docs/implementation-notes/09-e2e-fixture-application.md`](implementation-notes/09-e2e-fixture-application.md) for the alternatives considered.

## `applications/<name>/<environment>/values.yaml`

The values file of the generic chart ([`chart/application/README.md`](../chart/application/README.md)); the Environment's whole definition. The CLI writes every value it knows, including the ones equal to the chart defaults, so the file reads as a complete description:

```yaml
application:
  name: shop
environment: prod
platform:
  baseDomain: app.itma.no
kind: web-service
image:
  repository: ghcr.io/itema-as/shop
  tag: ""
size: small
port: 3000
probe:
  path: /
env: {}
domains: []
postgres:
  enabled: false
  migrationCommand: ""
  backupRetention: 30d
login:
  enabled: false
```

`image.tag` is empty when the Application is created: no image exists yet. The deploy workflow's write-back, `iidp ci set-image <app> <environment> <tag>` (see "How the CLI writes" below), sets it in place, every other key and comment untouched: a commit SHA (the full SHA GitHub gives `github.sha`, not a short one) on every push to `main`, a version (the `v*` tag with its leading `v` stripped, for example tag `v1.2.3` writes `1.2.3`) on a `v*` tag. Until the first write the empty tag is the Environment's "not released yet" state, and the chart renders nothing for it: no workload, no database, no migration Job, no final-backup hook ([`chart/application/README.md`](../chart/application/README.md#an-environment-without-an-image)). ArgoCD shows it as Synced and Healthy with no resources (with Postgres on, only the `backups-credentials` Secret from its `sops/` source, below), not as a comparison error. The first write renders the whole Environment and its automated sync applies it like any new Environment's: the database, then the migration, then the Application. `env` is where plain environment variables go.

When the first write comes depends on the Application: prod without staging gets it on the next push to `main`; with `--staging`, staging gets it on the next push to `main` and prod only on the first `v*` tag, when the deploy workflow promotes staging's image, which can be weeks later; an Adopt Environment waits for its pull request to be merged; and a staging Environment added later with `add-capability --staging` starts empty again. `iidp ci set-image` refuses an empty tag, so nothing in the Platform's own flow ever turns a released Environment back into an unreleased one. Doing so by hand would make ArgoCD prune everything the Environment runs except its database. The chart marks the Postgres `Cluster`, `ObjectStore` and `ScheduledBackup` `Prune=false`, so they stay running and the Environment shows OutOfSync until the edit is undone. The same applies to a hand-set `postgres.enabled: false`. `iidp app delete` is a cascade deletion, not a prune, and still removes the database after its final backup. See [`docs/implementation-notes/47-unreleased-environment.md`](implementation-notes/47-unreleased-environment.md).

`secrets` (a list of Secret names, empty until `iidp secret set` adds to it) is documented below.

### What the Capabilities write

- **`--postgres`** sets `postgres.enabled: true` in every Environment (prod and, with `--staging`, staging too: each gets its own database by construction, since the chart names the CloudNativePG `Cluster` after the Environment's own object name) and fills `platform.backupsBucket` and `platform.objectStorageEndpoint` from `platform.yaml`. `--migration-command` sets `postgres.migrationCommand` directly; left unset, the CLI looks for a Prisma schema, a Drizzle config or an npm `migrate` script in the Application repository (the generated template with `--path create`, or the current directory when it has a `package.json`) and proposes the matching command, printed before anything is written. A migration command without `--postgres` is refused: there is nothing to migrate. It also copies `bootstrap/templates/backups-credentials.enc.yaml` into every Environment that gets Postgres (below); a Platform repository without that file refuses `--postgres` before anything is written.
- **`--staging`** writes `applications/<name>/staging/{application.yaml,values.yaml}` next to `prod`, in the same commit: `environment: staging`, the ArgoCD Application `<name>-staging` in namespace `<name>-staging`, and the address `<name>-staging.<baseDomain>` (the chart derives it from `environment`). Every other Capability is the same in both Environments.
- **`--domain`** (repeatable) validates each host the way the chart does at render time (a lowercase DNS hostname of at least two labels, no duplicates) plus one only the CLI can check: none may equal a Platform address of either Environment. It then classifies each host: one label directly under `baseDomain` is covered by the Platform's wildcard certificate; any host inside `platform.yaml`'s `cloudflareZone` (the wildcard-covered ones included) is fully automatic, since external-dns can create its DNS record; anything else needs a CNAME to the prod address, which the closing summary prints (`CNAME <host> -> <name>.<baseDomain>`). `domains` is written for prod only: custom domains apply there, staging keeps its Platform address.
- **`--size`** (`small`, `medium` or `large`) applies to every Environment; any other value is refused.
- **`--login`** sets `login.enabled: true` in every Environment (prod and, with `--staging`, staging too: one oauth2-proxy cookie for the Platform base domain covers both, `docs/implementation-notes/18-itema-login.md`). Refused together with `--domain`: Itema login is for Platform addresses only, since its cookie is scoped to the base domain.

## `applications/<name>/<environment>/sops/`

Written and updated by `iidp secret set <app> <env> KEY=value`, never by `iidp app create`. One SOPS-encrypted Kubernetes Secret per key, because the CLI never holds the Platform's age private key and so cannot decrypt an existing document to merge a change into it: setting a key writes that key's whole file, whichever of its keys changed.

```
applications/shop/prod/sops/
  kustomization.yaml       generators: [ksops.yaml]
  ksops.yaml                the KSOPS generator, one entry per file below
  api-key.enc.yaml          Secret shop-api-key: stringData.API_KEY
  db-password.enc.yaml      Secret shop-db-password: stringData.DB_PASSWORD
```

`kustomization.yaml` is always `generators: [ksops.yaml]`, the same shape [`bootstrap/sops/kustomization.yaml`](../bootstrap/sops/kustomization.yaml) uses. `ksops.yaml` lists every `*.enc.yaml` file present, sorted, and is rewritten (not always changed) on every `secret set` so a key added later is picked up:

```yaml
apiVersion: viaduct.ai/v1
kind: ksops
metadata:
  name: shop-secrets
  annotations:
    config.kubernetes.io/function: |
      exec:
        path: ksops
files:
  - api-key.enc.yaml
  - db-password.enc.yaml
```

Each `<key-slug>.enc.yaml` (the KEY lowercased, `_` to `-`) is a SOPS document encrypted with `platform.yaml`'s `agePublicKey`, in the same shape `sops --encrypt` produces (only `data`/`stringData` encrypted; an unencrypted `sops` metadata block naming the recipient), decrypted in the cluster by KSOPS with the private key from Secret `argocd/sops-age` (`bootstrap/README.md`):

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: shop-api-key
  labels:
    app.kubernetes.io/name: shop
    iidp.itema.no/application: shop
    iidp.itema.no/environment: prod
  annotations:
    argocd.argoproj.io/sync-wave: "-2"
    kustomize.config.k8s.io/needs-hash: "false"
type: Opaque
stringData:
  API_KEY: ENC[AES256_GCM,data:...,iv:...,tag:...,type:str]
sops:
  age:
    - recipient: age1kpq9t46wreydm6dp2e9a6txzm88ymqj9ph38jvjlsjgff3k5vfqqqhee6v
      enc: |
        -----BEGIN AGE ENCRYPTED FILE-----
        ...
        -----END AGE ENCRYPTED FILE-----
  encrypted_regex: ^(data|stringData)$
  mac: ENC[...]
  version: 3.13.3
```

The Secret's name (`<fullname>-<key-slug>`: the bare Application name for prod, `<name>-staging` for staging, so both Environments can share a namespace without their Secrets colliding, the same `fullname` the chart's own objects use) is added to the `secrets` list of `values.yaml`; the chart mounts every listed Secret's keys as container environment variables (`chart/application/README.md`, `values.yaml`'s `secrets:` field). `needs-hash: "false"` keeps that name stable; `sync-wave: "-2"` puts the Secret a wave before the migration Job's `"-1"` (notes for #7), so a migration or the Application container never starts before the Secret it needs exists. No `namespace`: the ArgoCD Application's `spec.destination.namespace` applies.

`iidp secret set` validates the Application name, the Environment (`prod` or `staging`) and every `KEY` before cloning anything. It then clones `main` to read `platform.yaml` and check that the Environment (`applications/<app>/<env>/`) already exists; a missing `agePublicKey` or a missing Environment is refused there, with a clear error, before anything is written. The private key never reaches the CLI or the Platform repository; only the cluster (`bootstrap/README.md`, Secret `argocd/sops-age`) can decrypt.

## `bootstrap/templates/backups-credentials.enc.yaml` and the Postgres Capability's own Secret

The chart's Postgres Capability needs the Platform's Object Storage access and secret key in a Secret named `backups-credentials` (`platform.backupsCredentialsSecret`'s default) in every Environment's own namespace (`chart/application/README.md`, `docs/implementation-notes/07-chart-postgres.md`). Unlike a developer's own secrets (`iidp secret set`, above), the CLI never encrypts this one -- it does not hold the Platform's age private key or the Object Storage keys, and this Secret's plaintext is the same in every Environment, not per-Application.

The bootstrap wizard writes it once, `bootstrap/templates/backups-credentials.enc.yaml`: a Kubernetes Secret named `backups-credentials`, `stringData` `ACCESS_KEY_ID` and `ACCESS_SECRET_KEY` from the Hetzner stage's Object Storage keys, **no `namespace`** (so the document is valid copied into any Environment), annotated `kustomize.config.k8s.io/needs-hash: "false"`, SOPS-encrypted with `sops --encrypt` for the Platform's age key. It lives in its own subdirectory, `bootstrap/templates/`, deliberately neither directly under `bootstrap/` nor under `bootstrap/sops/`: the root Application `platform` reads every file directly under `bootstrap/` non-recursively and applies each one as a plain manifest (`bootstrap/README.md`), so a SOPS document placed there is itself applied, as ciphertext, and fails (`.sops: field not declared in schema`, confirmed against a real kind cluster); `bootstrap/sops/` is applied by `bootstrap/platform-secrets.yaml`, decrypted, into the Platform's own component namespaces (`argocd`, `cert-manager`, ...), which is not what this file is for either. `bootstrap/templates/` is invisible to both: a template the CLI reads and copies, applied only where an Environment's own `sops/` directory carries a copy of it. `.sops.yaml`'s creation rules cover its path the same way they cover `bootstrap/sops/*.enc.yaml`.

`iidp app create --postgres` and `iidp app add-capability --postgres` copy that file byte for byte into `applications/<name>/<environment>/sops/backups-credentials.enc.yaml` for every Environment that gets Postgres, register it in that directory's `ksops.yaml`/`kustomization.yaml` (created exactly as `iidp secret set` creates them, if a Postgres-enabled Environment has no other secret yet) and add the `sops/` kustomize source to the Environment's `application.yaml`, exactly once -- the same `render.AddKustomizeSource` `iidp secret set` uses, which is why an Environment that already had another secret set gets no duplicate source. It is deliberately **not** added to `values.yaml`'s `secrets:` list: the chart reads it through `platform.backupsCredentialsSecret`, not through `envFrom`, so listing it there would mean nothing.

The copy is never decrypted, and this is safe: SOPS's MAC covers the encrypted values, not the document's file name or path, so a byte-for-byte copy to a new location still decrypts correctly in the cluster (confirmed directly -- decrypting a copy of the file from a different path -- in `internal/cli/app_postgres_backups_credentials_test.go` and in `test/wizard/run.sh`). The destination namespace comes from the Environment's own ArgoCD Application (`spec.destination.namespace`), never from anything inside the Secret document, the same reasoning every other SOPS-encrypted Secret this repository writes already relies on (no `namespace` field, above). A Platform repository with no `bootstrap/templates/backups-credentials.enc.yaml` refuses `--postgres` before anything is written, naming the file and pointing at the bootstrap wizard.

Idempotent: re-running `iidp app create`/`add-capability --postgres` on an Environment that already has the file overwrites it with the same bytes (a no-op as far as `git diff` is concerned), and re-running the bootstrap wizard keeps the existing encrypted file unless the Object Storage keys it collects actually changed -- the same "kept means untouched" idempotency the wizard's other secrets already follow, since it cannot decrypt the existing file to compare its contents. See [`docs/implementation-notes/42-backups-credentials.md`](implementation-notes/42-backups-credentials.md).

## `iidp app add-capability`

Edits an Application's existing Environment files in place with the yaml.v3 node helpers `internal/render` already uses for `secret set` (`AddSecretName`, `AddKustomizeSource`), so unrelated keys, comments and `secrets:` survive. It refuses an Application with no directory under `applications/`, and refuses a Capability already present (Postgres already `enabled`, a `staging` directory that already exists, a domain already in `domains`, the requested size equal to the current one, Itema login already `enabled`), naming it; nothing is written when any check fails.

- **`--postgres`** sets `postgres.enabled: true` (and `postgres.migrationCommand`, when given) and `platform.backupsBucket`/`objectStorageEndpoint` in every Environment the Application already has, exactly the fields `app create` writes, and copies `bootstrap/templates/backups-credentials.enc.yaml` into each of them (above). **`--staging`**, below, also copies it into the new Environment when Postgres is already enabled (on prod, from this run or an earlier one) -- Postgres is uniform across an Application's Environments by construction.
- **`--staging`** copies `prod`'s values.yaml into a new `applications/<name>/staging/values.yaml` (`environment: staging`, `image.tag` reset to `""`, so staging renders nothing until the next push to `main` writes its first image, `domains: []`, and no `secrets:` list — prod's secrets are not copied, since the CLI cannot decrypt them to move them, and the command logs that) plus the same `application.yaml` shape `app create` writes.
- **`--domain`** (repeatable) validates and classifies each host exactly as `app create` does (`ValidateDomains`) and appends it to `prod`'s `domains`.
- **`--size`** rewrites `size` in every Environment the Application already has.
- **`--login`** sets `login.enabled: true` in every Environment the Application already has. Refused together with `--domain`, and refused when `prod` already lists a custom domain: Itema login is for Platform addresses only.

Committed as `iidp app add-capability <name> <capabilities>` (space-separated Capability names: `postgres`, `staging`, `domain`, `size`, `login`), pushed with the same retry-once-on-a-moved-`main` behaviour as `app create`.

## `iidp app delete`

Removes an Application in one commit, after the developer types the Application name back (or `--force` on a script): each Environment's `applications/<name>/<environment>/application.yaml` (`prod` and `staging`, whichever exist) is removed, along with `applications/<name>/repository.yaml` when the Application is bound, and the commit `iidp app delete <name>` is pushed. The binding goes because nothing renders from it, so the PreDelete hook does not need it, and a deleted Application must not stay deployable through the Deploy gate. `values.yaml` (and any `sops/` secrets) are deliberately **not** removed -- see below. Unlike before #39, the CLI no longer commits a `Backup` manifest or a `final-backup-<environment>` ArgoCD Application of its own.

What guarantees the final backup now is the chart, not commit ordering. Each Environment's own ArgoCD Application carries the resources finalizer, so ArgoCD deletes its resources once it notices `application.yaml` is gone — but a `postgres.enabled` Environment's chart also renders a `ServiceAccount`, `Role`, `RoleBinding` and Job (all named `<fullname>-final-backup`, `chart/application/templates/final-backup-job.yaml`). Only the Job is annotated `argocd.argoproj.io/hook: PreDelete`; the `ServiceAccount`, `Role` and `RoleBinding` are ordinary resources, present whenever `postgres.enabled` and pruned with the rest of the Environment's resources, the same as the Cluster or the Deployment (see the implementation notes for why more than one hook object was found to be unsafe). ArgoCD creates a `PreDelete` hook only when the Application itself is deleted, waits for it to reach Healthy before deleting anything else, and blocks the deletion (a `DeletionError` condition) if it fails. The Job runs a pinned `kubectl` image, creates a CloudNativePG `Backup` targeting the Environment's Cluster (`spec.method: plugin`, `spec.pluginConfiguration.name: barman-cloud.cloudnative-pg.io`, the same shape as the chart's `ScheduledBackup`) named `<fullname>-final-<timestamp>` with the annotation `iidp.itema.no/retain-until` computed 30 days ahead at run time, and polls its `.status.phase` until `completed` (failing, and so blocking the deletion, on `failed` or on timing out after `postgres.finalBackupTimeout` seconds).

**Why `values.yaml` stays.** ArgoCD only discovers an Application's `PreDelete` hooks by rendering its manifest (chart plus values) at the moment deletion starts, and that render uses the source `targetRevision`s the Application already has — `main`, a moving branch, for both the chart and the values file, so continuous deployment keeps working. Removing `values.yaml` in the same commit that triggers deletion means, by the time ArgoCD renders, `main` no longer has it: the render fails, and the resulting `DeletionError` blocks the deletion forever (confirmed against a real kind cluster; ArgoCD's own FAQ names this exact class of problem, "I've deleted/corrupted my repo and can't delete my app"). Leaving `values.yaml` (and `sops/`) behind costs nothing at rest: nothing renders it once `application.yaml` is gone (`bootstrap/applications.yaml`'s own glob matches only `application.yaml`), and developers never edit the Platform repository by hand, so reusing the same Application name later must not need one either: `checkApplicationAbsent` (`internal/platformrepo/writer.go`) treats a directory with no live `application.yaml` anywhere in it as available, not taken, and `iidp app create` clears it itself — naming it in its own output — in the same commit as the new Environment's files. A directory that does have a live `application.yaml` is still refused, so a human's own hand-placed files are never silently clobbered. An Environment that never received an image (`image.tag: ""`) renders no `PreDelete` hook, since the chart renders nothing for it, so its deletion is not held up by a backup of a database that was never created: ArgoCD removes whatever its other sources applied (the `backups-credentials` Secret, any `iidp secret set` secrets) and the Application is gone. See [`docs/implementation-notes/39-final-backup-predelete-hook.md`](implementation-notes/39-final-backup-predelete-hook.md) for the decisions behind this design, [`docs/implementation-notes/17-cli-add-capability-delete.md`](implementation-notes/17-cli-add-capability-delete.md) for the two-commit design it replaced, and [`chart/application/README.md`](../chart/application/README.md#deleting-an-environment) for the hook's own detail. `iidp app delete` never touches the Application repository.

## How the CLI writes

1. Validates the flags (the Application name is a lowercase DNS-1035 label of at most 40 characters) before touching anything. `--repo` outside `Itema-as` (Adopt, `iidp app bind`) is refused here, naming the transfer.
2. Takes the developer's GitHub token from the `gh` CLI (`gh auth token`). Write access to the Platform repository is the authorisation; a push refused for permissions says so and names the repository. With `--path create` or `--path adopt`, which also push `.github/workflows/deploy.yaml` to the Application repository, it first reads the token's scopes and refuses one without `workflow`, naming `gh auth refresh -s workflow`, before anything is created ([`docs/implementation-notes/47-workflow-scope.md`](implementation-notes/47-workflow-scope.md)).
3. Clones `main` shallowly into a temporary directory, reads `platform.yaml`, and refuses if `applications/<name>/` already exists.
4. Writes the Environment's files (and, with `--path create` or `--path adopt`, `applications/<name>/repository.yaml`), commits them with the author from the developer's git configuration and the message `iidp app create <name>`, and pushes to `main`.
5. If the push is rejected because `main` moved, it clones afresh and repeats once. If the Application's directory appeared in the meantime, it fails without writing; the other developer's Application wins.

The temporary directory is removed afterwards.

## How the deploy workflow writes back: `iidp ci set-image`

`iidp ci set-image <app> <prod|staging|auto> <tag>` is the subcommand the deploy workflow (`.github/workflows/deploy.yaml` in every Created Application repository, `internal/templates`) runs after it pushes an image to GHCR. Unlike every other command, it is not meant to be run by a developer and does not use the `gh` CLI's login. The Platform repository is private (like every other command, write access to it is the authorisation), so nothing here may read it before a real credential exists — every id this command needs comes from somewhere else first:

1. Reads the GitHub App's id from `IIDP_DEPLOY_APP_ID`, an org Actions variable the bootstrap wizard creates (`docs/implementation-notes/05-bootstrap-wizard.md`) and the deploy workflow passes through (`IIDP_DEPLOY_APP_ID: ${{ vars.IIDP_DEPLOY_APP_ID }}`). Missing or unparsable is a clear error naming the variable.
2. Signs a short-lived RS256 JWT for that app id with the standard library (no JWT dependency), using the private key from `IIDP_DEPLOY_APP_PRIVATE_KEY` (a PEM) or `IIDP_DEPLOY_APP_PRIVATE_KEY_FILE` (a path to one) — the org Actions secret the bootstrap wizard creates, available to the workflow but never written to the Platform repository.
3. Discovers the installation id by listing the App's own installations (`GET /app/installations`, JWT-authenticated) and matching the one whose account is the compiled-in org (`internal/platform.Org`) case-insensitively. No installation for the org is a clear error. This is a GitHub-App-level call, not a Platform-repository read.
4. Exchanges the JWT for a GitHub App installation token (`POST /app/installations/{id}/access_tokens`), using the same `internal/github` client and injectable base URL every other command's GitHub calls use.
5. Only now clones the Platform repository, with that installation token — the first and only time it is read, exactly the way every other command reads it with its own credential. `auto` resolves to `staging` when `applications/<app>/staging/` exists in that clone and to `prod` otherwise (the decision is made here, from the Platform repository, never by the workflow); refuses clearly if the Application or the resolved Environment does not exist. It then edits `image.tag` in place in the Environment's `values.yaml` (`render.SetImageTag`, the same node-level YAML editing `iidp secret set` uses for `secrets:` and `spec.sources`, so every other key and every comment survives), commits `Deploy <app> <environment> <tag>` and pushes to `main`, retrying once after a fresh clone if the push is rejected because `main` moved — the same logic `iidp app create` uses.

The GitHub App needs `contents: write` on the Platform repository for step 5 to succeed; the bootstrap wizard's manifest already requests it (`docs/implementation-notes/05-bootstrap-wizard.md`).

`platform.yaml`'s `githubApp.id`/`githubApp.installationId` (the table above) are documentation only: the bootstrap wizard records them there for a human to see which App and installation a Platform repository is wired to, but `iidp ci set-image` never reads either to authenticate — that would recreate the very "read before any credential exists" problem step 5 avoids. Once the (only) authenticated clone in step 5 happens, the command does a best-effort, tolerant read of `githubApp.installationId` purely to log it for cross-checking; a missing or absent value is not an error.
