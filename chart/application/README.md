# application

The generic Helm chart every Application on Itema's Platform is an instance of ([ADR-0003](../../docs/adr/0003-one-generic-helm-chart-per-application.md)). The Platform repository holds, per Environment, one ArgoCD Application pointing at a pinned version of this chart and one values file; that values file is the Environment's whole definition. The CLI writes it, developers do not edit it by hand, and every Platform convention lives in the templates here rather than in the CLI.

Today the chart renders both Kinds, Web service and Static site, as a Deployment, a Service and an Ingress on a Platform address served with the Platform's wildcard certificate; custom domains, each with the right certificate; and named Secrets as environment variables. With the Postgres Capability on it also renders the Environment's own CloudNativePG database with continuous backups, injects `DATABASE_URL`, and runs the migration command before every rollout. With the Itema login Capability on, the Platform address's Ingress is annotated for the bootstrap's shared oauth2-proxy, so an Entra ID sign-in is required to reach it.

## Values

| Value | Default | Meaning |
|---|---|---|
| `application.name` | required | The Application's name: lowercase letters, digits and dashes, starting with a letter, at most 55 characters. It names every object and forms the Platform address. |
| `environment` | `prod` | `prod` or `staging`. Anything else fails rendering. |
| `platform.baseDomain` | required | The Platform base domain, for example `app.itma.no`. |
| `platform.httpIssuer` | `letsencrypt-http01` | The cert-manager ClusterIssuer, created by the bootstrap, that issues a certificate over HTTP-01 for a custom domain the wildcard does not cover. |
| `platform.backupsBucket` | required with Postgres | The Object Storage bucket every Application database is backed up to, for example `itema-iidp-db-backups`. |
| `platform.objectStorageEndpoint` | required with Postgres | The S3 endpoint of the bucket's location, for example `https://hel1.your-objectstorage.com`. |
| `platform.backupsCredentialsSecret` | `backups-credentials` | The Secret holding the Object Storage access key (key `ACCESS_KEY_ID`) and secret key (key `ACCESS_SECRET_KEY`) the backups are written with. It must exist in the namespace the Environment is installed into. Nothing in this chart creates it: the bootstrap wizard writes it once, SOPS-encrypted, to the Platform repository's `bootstrap/templates/backups-credentials.enc.yaml`, and `iidp app create`/`add-capability --postgres` copies it, byte for byte, into the Environment's own `sops/` directory, applied the same way a developer's own `iidp secret set` secrets are (`docs/platform-repository.md`). |
| `kind` | `web-service` | What the Application is: `web-service` (a container listening on `port`) or `static-site` (an nginx image built by CI, listening on 80). Anything else fails. |
| `image.repository` | required | The image, for example `ghcr.io/itema-as/shop`. |
| `image.tag` | required | The tag CI wrote: a commit SHA on `main`, a version on a `v*` tag. |
| `size` | `small` | `small`, `medium` or `large`. See below. Anything else fails. |
| `port` | `3000` | The port a Web service listens on. Ignored by a Static site, which always listens on 80. |
| `probe.path` | `/` | The path the readiness and liveness probes request. |
| `env` | `{}` | Plain environment variables, name to value. Not for secrets, and it must not set `PORT` (a Web service gets it from `port`, and a Static site listens on 80 regardless) nor `DATABASE_URL` when Postgres is enabled. |
| `secrets` | `[]` | Names of Secrets in the Environment's namespace. Every key of each becomes an environment variable, of the Application and of the migration Job. The chart renders no Secret; the CLI writes them SOPS-encrypted next to the values file. |
| `domains` | `[]` | Custom domains, one hostname each, served beside the Platform address. A hostname that is not lowercase DNS, is listed twice, is the Environment's own Platform address, or is too long for its TLS secret's name fails rendering. See "Custom domains" below. |
| `postgres.enabled` | `false` | The Postgres Capability. See below. Needs `kind: web-service`; a Static site with it fails rendering. |
| `postgres.migrationCommand` | `""` | A shell line run from the Application image, with `DATABASE_URL` set, before every rollout. Empty means no migrations. Setting it without `postgres.enabled` fails rendering. |
| `postgres.backupRetention` | `30d` | How long backups and WAL are kept in the bucket: a number of days (`d`), weeks (`w`) or months (`m`). |
| `postgres.finalBackupTimeout` | `1800` | Seconds the final Backup PreDelete hook Job (see "Deleting an Environment" below) waits for its Backup to reach phase `completed` before it fails, blocking the deletion. |
| `login.enabled` | `false` | The Itema login Capability. See below. Platform addresses only: fails rendering when `domains` is non-empty. |

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

DNS is not the chart's business. external-dns creates the records for hosts in zones on Cloudflare; a host elsewhere needs a CNAME to the Platform address, which the CLI prints.

**Secrets.** Each name in `secrets` becomes an `envFrom.secretRef` on the container, so every key of the Secret is an environment variable. The chart never renders a Secret and never sees a value: the CLI writes them SOPS-encrypted next to the values file, and KSOPS decrypts them on the Platform.

**Itema login.** With `login.enabled`, the Platform address's Ingress (never the `-http01` one) carries `traefik.ingress.kubernetes.io/router.middlewares: oauth2-proxy-itema-login-errors@kubernetescrd,oauth2-proxy-itema-login-auth@kubernetescrd`, the bootstrap's ForwardAuth middleware for the shared oauth2-proxy plus the `errors` middleware that turns its 401/403 into a sign-in redirect (`bootstrap/README.md`). One sign-in against Entra ID, cookied for the Platform base domain, covers every protected Environment. Refused at render time when `domains` is non-empty: the cookie is scoped to the base domain, so Itema login only makes sense on Platform addresses, never a custom domain.

**Labels.** Every object, and the Pod template, carries `app.kubernetes.io/name` (the Application), `app.kubernetes.io/instance` (the Environment's object name, `<name>` or `<name>-staging`), `iidp.itema.no/application` and `iidp.itema.no/environment`. Grafana Alloy attributes logs and metrics by the last two. The Service and the Deployment select Pods by the first two, which never change between chart versions.

**Replicas.** One. The Platform is a single node; there is nothing to spread over.

**Postgres.** With `postgres.enabled`, the Environment gets a CloudNativePG `Cluster` named `<name>-db` (`shop-db`, `shop-staging-db`): one instance, 250m CPU and 256Mi memory as both requests and limits whatever the Application's size, a 5Gi volume, and a database and owner both named after the Application. The operator generates the owner's password and the Secret `<name>-db-app`; the container gets `DATABASE_URL` from that Secret's `uri` key, so neither the CLI nor the developer ever handles credentials, and the same key is the whole connection string in every Environment. Backups are continuous: every WAL segment is archived through the Barman Cloud Plugin to an `ObjectStore` of the same name at `s3://<platform.backupsBucket>/<name>/<environment>/` on `platform.objectStorageEndpoint`, gzip-compressed, with the keys from `platform.backupsCredentialsSecret`, and a `ScheduledBackup` takes a base backup every day at 03:00 UTC, the first one immediately. Backups and WAL older than `postgres.backupRetention` are deleted from the bucket. The database Pods carry the two `iidp.itema.no/*` labels so Alloy attributes their logs, but not the selector labels. The Platform must run CloudNativePG 1.30 or newer with the Barman Cloud Plugin installed (see [the implementation notes](../../docs/implementation-notes/07-chart-postgres.md) for the versions assumed).

**Migrations.** With `postgres.migrationCommand` set, a Job named `<name>-migrate` runs `sh -c "<command>"` from the Application image with the Application's `env`, its `secrets` as `envFrom`, and `DATABASE_URL`, at the Application's size. It is an ArgoCD `Sync` hook in sync wave -1: the `Cluster` and `ObjectStore` are applied in wave -2 and must be healthy first, the Job runs next, and the Deployment, Service and Ingress in wave 0 are applied only if it succeeds. A failed migration (`backoffLimit: 0`, `restartPolicy: Never`) therefore fails the sync and stops the rollout; the previous run's Job is deleted before the next is created (`BeforeHookCreation`). The migration Pod does not carry the selector labels, so the Service never routes to it.

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

`chart_test.go` is a Go test package that shells out to `helm template` with the fixtures in `testdata/`, parses the rendered manifests, and asserts on them: each size's resources, the prod and staging hosts, the default and an overridden probe path, the injected `PORT`, the labels, the wildcard host naming no secret, and that unknown sizes, unknown Kinds and bad names are refused. `static_site_test.go` covers the Static site Kind (port 80, no `PORT`, the probes) and `domains_test.go` covers custom domains under and outside the wildcard, the second Ingress, secrets as `envFrom`, and the refused domains. `postgres_test.go` covers the Postgres Capability with the same helpers: nothing database-related renders with it off, the Cluster's shape, `DATABASE_URL` from the app Secret, the backup configuration, the migration Job present only with a command and ordered before the Application's objects, that only the Deployment's Pods match the Service, and the refusals. `login_test.go` covers the Itema login Capability: the ForwardAuth middleware annotation present on the Platform address's Ingress only when `login.enabled`, absent otherwise, and the refusal with a custom domain. `final_backup_test.go` covers the final Backup PreDelete hook: the `ServiceAccount`, `Role`, `RoleBinding` and Job render only with Postgres, that only the Job carries the `PreDelete` hook and its delete policy (the RBAC carry neither, nor a sync-wave), the `Role`'s rules, the Job's pinned image and script (the Barman Cloud Plugin shape, the retain-until annotation, the completed/failed poll), and `postgres.finalBackupTimeout` reaching the script. Each runs `kubeconform -strict` on its fixtures' rendered output against the Kubernetes minor of the k3s release pinned in `infra/platform/variables.tf`, so the node's version is the only pin; the Postgres run adds the CloudNativePG and Barman Cloud CRD schemas from the datreeio CRDs-catalog as a second schema location (the final Backup hook's own objects are core/RBAC Kinds the default schema location already covers; the `Backup` it creates at run time is never rendered by the chart, so it needs no schema here). The tests skip themselves when `helm` or `kubeconform` is not on `PATH`, so `go test ./...` passes on any machine; the `Chart` job in CI installs both, sets `IIDP_REQUIRE_CHART_TOOLS` so a missing tool fails instead of skipping, and runs them on every pull request.

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
