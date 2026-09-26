# application

The generic Helm chart every Application on Itema's Platform is an instance of ([ADR-0003](../../docs/adr/0003-one-generic-helm-chart-per-application.md)). The Platform repository holds, per Environment, one ArgoCD Application pointing at a pinned version of this chart and one values file; that values file is the Environment's whole definition. The CLI writes it, developers do not edit it by hand, and every Platform convention lives in the templates here rather than in the CLI.

Today the chart renders both Kinds, Web service and Static site, as a Deployment, a Service and an Ingress on a Platform address served with the Platform's wildcard certificate; custom domains, each with the right certificate; and named Secrets as environment variables. With the Postgres Capability on it also renders the Environment's own CloudNativePG database with continuous backups, injects `DATABASE_URL`, and runs the migration command before every rollout. With the Itema login Capability on, every Ingress of the Environment is annotated for the bootstrap's shared oauth2-proxy, so an Entra ID sign-in is required to reach the Platform address and every custom domain; with sign-in groups, only members of those Entra groups get in.

## Values

| Value | Default | Meaning |
|---|---|---|
| `application.name` | required | The Application's name: lowercase letters, digits and dashes, starting with a letter, at most 55 characters. It names every object and forms the Platform address. |
| `environment` | `prod` | `prod` or `staging`. Anything else fails rendering. |
| `platform.baseDomain` | required | The Platform base domain, for example `app.itma.no`. |
| `platform.httpIssuer` | `letsencrypt-http01` | The cert-manager ClusterIssuer, created by the bootstrap, that issues a certificate over HTTP-01 for a custom domain the wildcard does not cover. |
| `platform.loginCookieDomain` | `""` (the base domain) | The domain the Itema login cookie is set for, without the leading dot: `platform.yaml`'s `cloudflareZone`, or the base domain on a Platform without one, the same rule the bootstrap's oauth2-proxy follows. The CLI writes it with `login.enabled`. With login on, the Platform address and every custom domain must be inside it. |
| `platform.backupsBucket` | required with Postgres | The Object Storage bucket every Application database is backed up to, for example `itema-iidp-db-backups`. |
| `platform.objectStorageEndpoint` | required with Postgres | The S3 endpoint of the bucket's location, for example `https://hel1.your-objectstorage.com`. |
| `platform.backupsCredentialsSecret` | `backups-credentials` | The Secret holding the Object Storage access key (key `ACCESS_KEY_ID`) and secret key (key `ACCESS_SECRET_KEY`) the backups are written with. It must exist in the namespace the Environment is installed into. Nothing in this chart creates it: the bootstrap wizard writes it once, SOPS-encrypted, to the Platform repository's `bootstrap/templates/backups-credentials.enc.yaml`, and `iidp app create`/`add-capability --postgres` copies it, byte for byte, into the Environment's own `sops/` directory, applied the same way a developer's own `iidp secret set` secrets are (`docs/platform-repository.md`). |
| `kind` | `web-service` | What the Application is: `web-service` (a container listening on `port`) or `static-site` (an nginx image built by CI, listening on 80). Anything else fails. |
| `image.repository` | required | The image, for example `ghcr.io/itema-as/shop`. |
| `image.tag` | `""` | The tag CI wrote: a commit SHA on `main`, a version on a `v*` tag. Empty until the Environment's first image, and then the chart renders nothing; see "An Environment without an image" below. |
| `size` | `small` | `small`, `medium` or `large`. See below. Anything else fails. |
| `port` | `3000` | The port a Web service listens on. Ignored by a Static site, which always listens on 80. |
| `probe.path` | `/` | The path the readiness and liveness probes request. |
| `env` | `{}` | Plain environment variables, name to value. Not for secrets, and it must not set `PORT` (a Web service gets it from `port`, and a Static site listens on 80 regardless) nor `DATABASE_URL` when Postgres is enabled. |
| `secrets` | `[]` | Names of Secrets in the Environment's namespace. Every key of each becomes an environment variable, of the Application and of the migration Job. The chart renders no Secret; the CLI writes them SOPS-encrypted next to the values file. |
| `domains` | `[]` | Custom domains, one hostname each, served beside the Platform address. A hostname that is not lowercase DNS, is listed twice, is the Environment's own Platform address, or is too long for its TLS secret's name fails rendering. See "Custom domains" below. |
| `postgres.enabled` | `false` | The Postgres Capability. See below. Needs `kind: web-service`; a Static site with it fails rendering. |
| `postgres.migrationCommand` | `""` | A shell line run from the Application image, with `DATABASE_URL` set, before every rollout. Empty means no migrations. Setting it without `postgres.enabled` fails rendering. On the Platform the Deploy gate writes it with each deploy, from the Application repository's `iidp.yaml` ([`docs/platform-repository.md`](../../docs/platform-repository.md#the-migration-command-travels-with-the-deploy)). |
| `postgres.backupRetention` | `30d` | How long backups and WAL are kept in the bucket: a number of days (`d`), weeks (`w`) or months (`m`). |
| `postgres.finalBackupTimeout` | `1800` | Seconds the final Backup PreDelete hook Job (see "Deleting an Environment" below) waits for its Backup to reach phase `completed` before it fails, blocking the deletion. |
| `login.enabled` | `false` | The Itema login Capability. See below. Fails rendering when a custom domain, or the Platform address, is outside `platform.loginCookieDomain`, naming the domains. |
| `login.groups` | `[]` | Sign-in groups: Entra group object ids (GUIDs). Empty lets every Itema user in, and renders exactly what `login.enabled` alone does. With groups only their members get in; see "Itema login" below. Fails rendering without `login.enabled`, for an id that is not a GUID, and for an id listed twice (case aside). |

## Conventions the chart encodes

**Sizes.** Each size is a fixed CPU and memory amount, applied as both requests and limits so an Environment gets exactly what it was sized for and cannot starve its neighbours on the single node:

| Size | CPU | Memory |
|---|---|---|
| `small` | 250m | 256Mi |
| `medium` | 500m | 512Mi |
| `large` | 1 | 1Gi |

**Port and probes.** A Web service container is told its port through the `PORT` environment variable, taken from `port`, and the Service listens on the same number. A Static site is an nginx image: it listens on 80, `port` is ignored and no `PORT` is injected. Readiness and liveness probes are HTTP GETs to `probe.path` on the container's port for both Kinds; most Applications need no probe configuration because `/` answers.

**Names and addresses.** prod is the unadorned Application; staging carries a `-staging` suffix. The Deployment, Service and Ingress of prod are named `<name>` and reachable at `<name>.<baseDomain>`; staging's are named `<name>-staging` at `<name>-staging.<baseDomain>`. Both Environments can therefore share a namespace. The single container is named after the Application.

**Ingress.** `ingressClassName: traefik`, routed on Traefik's `websecure` entrypoint with TLS on. The Platform address names no TLS secret: the Platform's wildcard certificate is Traefik's default certificate (the bootstrap puts it in a `TLSStore` named `default` in Traefik's namespace), served for every host that brings no certificate of its own. The Ingress does not listen on plain HTTP; redirecting HTTP to HTTPS is an entrypoint setting on the Platform's Traefik, configured by the bootstrap, not something each Application repeats.

**Custom domains.** Each entry of `domains` becomes one more host routed to the same Service. Which certificate serves it depends only on whether the wildcard covers it:

- A host directly under the base domain (`<label>.<baseDomain>`, for example `butikk.app.itma.no`) is covered by `*.<baseDomain>`. It joins the Platform address on the Environment's Ingress and names no secret.
- Any other host is foreign to the wildcard (`shop.example.com`, `shop.itma.no`, or a deeper `test.shop.app.itma.no`, since a wildcard covers one label) and goes on a second Ingress named `<name>-http01`, annotated `cert-manager.io/cluster-issuer: <platform.httpIssuer>`, with one TLS entry per host and its own secret `<name>-<host with dots as dashes>-tls`. cert-manager's ingress-shim issues one Certificate per entry, solved over HTTP-01. The hosts sit on a second object because ingress-shim acts on every host of the Ingress it finds the annotation on; kept apart, it never tries to issue for the wildcard hosts.

DNS is not the chart's business. external-dns creates the records for hosts in the one zone it manages, `platform.yaml`'s `cloudflareZone`; a host anywhere else, another Cloudflare zone included, needs a CNAME to the Platform address, which the CLI prints.

**Secrets.** Each name in `secrets` becomes an `envFrom.secretRef` on the container, so every key of the Secret is an environment variable. The chart never renders a Secret and never sees a value: the CLI writes them SOPS-encrypted next to the values file, and KSOPS decrypts them on the Platform.

**Itema login.** With `login.enabled`, both Ingresses, the Platform address's and the `-http01` one, carry `traefik.ingress.kubernetes.io/router.middlewares: oauth2-proxy-itema-login-auth@kubernetescrd`, the bootstrap's ForwardAuth middleware for the shared oauth2-proxy. An unauthenticated browser is redirected straight to the Entra ID sign-in and back to the page it asked for (`bootstrap/README.md`). One sign-in against Entra ID, cookied for `platform.loginCookieDomain` (`itma.no`), covers every protected Environment on every host inside it: `shop.app.itma.no`, `butikk.app.itma.no`, `x.itma.no`. A custom domain outside it (`shop.example.com`) would never get the cookie, so it is refused at render time, named. The browser also sends the cookie to hosts in the zone the Platform does not run; see the trade-off in [`bootstrap/README.md`](../../bootstrap/README.md#itema-login).

With `login.groups`, the Environment also gets a Traefik `Middleware` of its own, `<name>-itema-login` in its namespace (sync wave -1, so it exists before the Ingresses name it). It is the bootstrap's `itema-login-auth` with `?allowed_groups=<ids>` added to the address, the ids lowercased and comma-separated, and both Ingresses name it (`<namespace>-<name>-itema-login@kubernetescrd`) instead of the shared one. It is the same oauth2-proxy at the same address, so an unauthenticated browser is still redirected straight to Entra ID. Once signed in, a user in any of the groups gets through, and one in none of them gets oauth2-proxy's plain 403, which Traefik returns as it is: no redirect, so no loop. Groups come from the `groups` claim of the Entra ID token (Microsoft Graph for a user in more than 200 groups), read at sign-in. [`docs/implementation-notes/92-sign-in-groups.md`](../../docs/implementation-notes/92-sign-in-groups.md) has the oauth2-proxy and Traefik source this was checked against.

cert-manager's HTTP-01 challenge for a protected host on the `-http01` Ingress is not sent to sign-in. The solver serves `/.well-known/acme-challenge/<token>` from an Ingress cert-manager creates for the challenge, with no middleware, and Traefik makes one router per Ingress path, with that Ingress's own middlewares, preferring the longer rule: the challenge path's router wins over the Application's `/`. The kind end-to-end test checks this with an Ingress of the solver's shape (`docs/implementation-notes/76-login-in-zone-domains.md`).

**Labels.** Every object, and the Pod template, carries `app.kubernetes.io/name` (the Application), `app.kubernetes.io/instance` (the Environment's object name, `<name>` or `<name>-staging`), `iidp.itema.no/application` and `iidp.itema.no/environment`. Grafana Alloy attributes logs and metrics by the last two. The Service and the Deployment select Pods by the first two, which never change between chart versions.

**Replicas.** One. The Platform is a single node; there is nothing to spread over.

**Postgres.** With `postgres.enabled`, the Environment gets a CloudNativePG `Cluster` named `<name>-db` (`shop-db`, `shop-staging-db`): one instance, 250m CPU and 256Mi memory as both requests and limits whatever the Application's size, a 5Gi volume, and a database and owner both named after the Application. The operator generates the owner's password and the Secret `<name>-db-app`; the container gets `DATABASE_URL` from that Secret's `uri` key, so neither the CLI nor the developer ever handles credentials, and the same key is the whole connection string in every Environment. Backups are continuous: every WAL segment is archived through the Barman Cloud Plugin to an `ObjectStore` of the same name at `s3://<platform.backupsBucket>/<name>/<environment>/` on `platform.objectStorageEndpoint`, gzip-compressed, with the keys from `platform.backupsCredentialsSecret`, and a `ScheduledBackup` takes a base backup every day at 03:00 UTC, the first one immediately. Backups and WAL older than `postgres.backupRetention` are deleted from the bucket. The database Pods carry the two `iidp.itema.no/*` labels so Alloy attributes their logs, but not the selector labels. The Platform must run CloudNativePG 1.30 or newer with the Barman Cloud Plugin installed (see [the implementation notes](../../docs/implementation-notes/07-chart-postgres.md) for the versions assumed).

**Migrations.** With `postgres.migrationCommand` set, a Job named `<name>-migrate` runs `sh -c "<command>"` from the Application image with the Application's `env`, its `secrets` as `envFrom`, and `DATABASE_URL`, at the Application's size. It is an ArgoCD `Sync` hook in sync wave -1: the `Cluster` and `ObjectStore` are applied in wave -2 and must be healthy first, the Job runs next, and the Deployment, Service and Ingress in wave 0 are applied only if it succeeds. A failed migration (`backoffLimit: 0`, `restartPolicy: Never`) therefore fails the sync and stops the rollout; the previous run's Job is deleted before the next is created (`BeforeHookCreation`). The migration Pod does not carry the selector labels, so the Service never routes to it.

## An Environment without an image

`iidp app create` writes every Environment with `image.tag: ""`: no image exists until the first deploy through the Deploy gate (`iidp ci set-image`), which for prod is the next push to `main`, or, with a staging Environment, the first `v*` tag, when the workflow promotes staging's image. An Adopt Environment waits for its pull request to be merged, and `iidp app add-capability --staging` starts staging the same way. The empty tag is the signal: while it is empty the chart renders **no objects at all**. Not the Deployment, Service or Ingresses, and not the Postgres `Cluster`, `ObjectStore`, `ScheduledBackup`, migration Job or final-backup hook either. Every template checks `application.released` (`_helpers.tpl`) after `application.validate`, so a broken values file (an unknown size, a missing `image.repository`, a missing bucket with Postgres on, ...) is still refused with the same message whether or not the Environment has an image yet.

ArgoCD shows such an Environment as **Synced** and **Healthy** with an empty resource tree (or, with Postgres on, only the `backups-credentials` Secret its `sops/` source applies), instead of the `ComparisonError` an `image.tag is required` render failure gave before. The first tag written makes the Environment OutOfSync and its automated sync applies everything in the usual waves: the database in wave -2, the migration in wave -1, the Application in wave 0, exactly like any new Environment's first sync. Deleting an Environment that never had an image renders no `PreDelete` hook (there is no database to back up), so it is deleted straight away.

Nothing is created ahead of the first image, the database included: a `Cluster` nothing uses would hold 256Mi of the node's memory and back up an empty database every day for as long as the first release takes, and its final-backup hook would then take a backup of nothing on deletion. See [the implementation notes](../../docs/implementation-notes/47-unreleased-environment.md) for the alternatives considered.

Only a deploy through the Deploy gate sets the tag, and it refuses an empty one. If someone empties the tag of an Environment that already has one by hand, the chart renders nothing and ArgoCD's automated prune removes the running Application. The database survives: see "Keeping the database" below.

## Keeping the database

The Postgres `Cluster`, its `ObjectStore` and its `ScheduledBackup` carry `argocd.argoproj.io/sync-options: Prune=false`, so **a sync never deletes the database**. A hand edit that stops rendering them, such as an emptied `image.tag` or `postgres.enabled: false`, leaves the database running, still archiving WAL and taking its daily backup. ArgoCD then shows the Environment **OutOfSync**, with those three objects marked as requiring pruning. The last sync still reads Succeeded, with the three listed as "ignored (no prune)". To leave that state, undo the edit, or delete the three objects deliberately. Deleting the Environment is not a prune: ArgoCD's cascade deletion honours `Delete=false`, which nothing here sets, not `Prune=false`, so `iidp app delete` still runs the final backup and removes the database as described below. A database kept this way gets no final backup if its Environment is deleted while the edit is still in place, because the hook is rendered from that same values file. Undo the edit before deleting. See [the implementation notes](../../docs/implementation-notes/47-unreleased-environment.md).

## Deleting an Environment

With `postgres.enabled`, the chart also renders a `ServiceAccount`, a `Role`, a `RoleBinding` and a Job, all named `<name>-final-backup`. Only the Job is annotated `argocd.argoproj.io/hook: PreDelete`; the `ServiceAccount`, `Role` and `RoleBinding` are ordinary resources, present whenever `postgres.enabled` and pruned with the rest of the Environment's resources, the same as the Cluster or the Deployment -- deliberately, so there is exactly one hook object to create and wait for, not several (see [the implementation notes](../../docs/implementation-notes/39-final-backup-predelete-hook.md) for the ArgoCD race that having more than one caused). ArgoCD creates a `PreDelete` hook only when the Environment's own ArgoCD Application (the one `iidp app delete` removes from the Platform repository) is itself deleted, never during an ordinary sync; it waits for the `PreDelete` hook to reach Healthy before it deletes anything else, and a failing hook blocks the deletion (a `DeletionError` condition) until it is fixed in git or the hook resource is removed by hand. `argocd.argoproj.io/hook-delete-policy: BeforeHookCreation,HookSucceeded` on the Job cleans up a successful attempt and replaces a previous attempt's Job before a retry creates a new one; the `ServiceAccount`, `Role` and `RoleBinding`, not being hooks, need no delete policy of their own.

The Job runs a pinned `kubectl` image (`bitnami/kubectl`, pinned by digest since Docker Hub no longer publishes floating version tags for it) whose script computes a run-time timestamp, creates a CloudNativePG `Backup` named `<name>-final-<timestamp>` (`spec.method: plugin`, `spec.pluginConfiguration.name: barman-cloud.cloudnative-pg.io`, the same shape as the `ScheduledBackup`, targeting this Environment's Cluster) annotated `iidp.itema.no/retain-until` with a date 30 days ahead computed when the Job runs, then polls `.status.phase` every ten seconds, printing progress, until it reaches `completed` (success) or `failed` (the Job fails, blocking the deletion), bounded by `postgres.finalBackupTimeout` seconds. Its `Role` is scoped to `create`, `get`, `list` and `watch` on `backups.postgresql.cnpg.io` in the Environment's own namespace only. See [the implementation notes](../../docs/implementation-notes/39-final-backup-predelete-hook.md) for why a PreDelete hook replaced the earlier two-commit design, and `docs/platform-repository.md` for what `iidp app delete` itself does.

## Rendering locally

From the repository root:

```sh
helm template shop chart/application --values chart/application/testdata/prod-small.yaml
helm template shop chart/application --values chart/application/testdata/staging-medium.yaml
helm template brochure chart/application --values chart/application/testdata/static-site.yaml
helm template shop chart/application --values chart/application/testdata/custom-domains-mixed.yaml
helm template shop chart/application --values chart/application/testdata/postgres-prod.yaml
helm template shop chart/application --values chart/application/testdata/login-enabled.yaml
helm template shop chart/application --values chart/application/testdata/login-groups.yaml --namespace shop-staging
helm template shop chart/application --values chart/application/testdata/unreleased-prod.yaml   # no image yet: renders nothing
```

Or with your own values:

```sh
helm template shop chart/application \
  --set application.name=shop \
  --set platform.baseDomain=app.itma.no \
  --set image.repository=ghcr.io/itema-as/shop \
  --set image.tag=1.4.2
```

Lint the chart with a fixture (the chart has required values, so a bare `helm lint` only warns):

```sh
helm lint --strict chart/application --values chart/application/testdata/prod-small.yaml
```

## Tests

`chart_test.go` is a Go test package that shells out to `helm template` with the fixtures in `testdata/`, parses the rendered manifests, and asserts on them: each size's resources, the prod and staging hosts, the default and an overridden probe path, the injected `PORT`, the labels, the wildcard host naming no secret, and that unknown sizes, unknown Kinds and bad names are refused. `static_site_test.go` covers the Static site Kind (port 80, no `PORT`, the probes) and `domains_test.go` covers custom domains under and outside the wildcard, the second Ingress, secrets as `envFrom`, and the refused domains. `postgres_test.go` covers the Postgres Capability with the same helpers: nothing database-related renders with it off, the Cluster's shape, `DATABASE_URL` from the app Secret, the backup configuration, the migration Job present only with a command and ordered before the Application's objects, that only the Deployment's Pods match the Service, `Prune=false` on exactly the `Cluster`, `ObjectStore` and `ScheduledBackup` with `Delete=false` on nothing, and the refusals. `login_test.go` covers the Itema login Capability: the ForwardAuth middleware annotation present on the Platform address's Ingress only when `login.enabled`, absent otherwise, and the refusal with a custom domain; and sign-in groups: no `Middleware` and the shared annotation without groups, the Environment's own `Middleware` (the shared one's `forwardAuth` with `allowed_groups` added, read from the bootstrap's file) named on both Ingresses with them, and the refusals. `unreleased_test.go` covers an Environment without an image: nothing renders from a values file with every Capability on and an empty tag, nor from any fixture with its tag emptied or removed; every `refuse-*` fixture is refused with the same message with and without a tag; every template file checks `application.released`; and the values the CLI's own `internal/render` writes render nothing before `SetImageTag` (the `iidp ci set-image` edit) and the full Environment after it, for a created prod and for a staging added by `add-capability --staging`. `final_backup_test.go` covers the final Backup PreDelete hook: the `ServiceAccount`, `Role`, `RoleBinding` and Job render only with Postgres, that only the Job carries the `PreDelete` hook and its delete policy (the RBAC carry neither, nor a sync-wave), the `Role`'s rules, the Job's pinned image and script (the Barman Cloud Plugin shape, the retain-until annotation, the completed/failed poll), and `postgres.finalBackupTimeout` reaching the script. Each runs `kubeconform -strict` on its fixtures' rendered output against the Kubernetes minor of the k3s release pinned in `infra/platform/variables.tf`, so the node's version is the only pin; the Postgres run, and the sign-in groups run for the Traefik `Middleware`, add the CRD schemas from the datreeio CRDs-catalog as a second schema location (the final Backup hook's own objects are core/RBAC Kinds the default schema location already covers; the `Backup` it creates at run time is never rendered by the chart, so it needs no schema here). The tests skip themselves when `helm` or `kubeconform` is not on `PATH`, so `go test ./...` passes on any machine; the `Chart` job in CI installs both, sets `IIDP_REQUIRE_CHART_TOOLS` so a missing tool fails instead of skipping, and runs them on every pull request.

```sh
go test ./chart/...
```

## Publishing

The `Release` workflow packages this chart on every `v*` tag with the tag's version (the `v` stripped, the same version the CLI reports) and pushes it to GHCR as an OCI artifact:

```
oci://ghcr.io/<owner>/charts/application
```

where `<owner>` is the lowercase owner of this repository, so `oci://ghcr.io/itema-as/charts/application` once the repository lives under `Itema-as`. An ArgoCD Application in the Platform repository pins it by version and supplies the Environment's values file:

```yaml
source:
  repoURL: ghcr.io/itema-as/charts
  chart: application
  targetRevision: 0.3.1
```

Pull it by hand with:

```sh
helm pull oci://ghcr.io/itema-as/charts/application --version 0.3.1
```

The first push creates the `charts/application` package as private. Make it public in the package's settings on GitHub, or give the Platform pull credentials, before ArgoCD can fetch it. The `version` field in `Chart.yaml` is only a placeholder for local rendering; the workflow overrides it.
