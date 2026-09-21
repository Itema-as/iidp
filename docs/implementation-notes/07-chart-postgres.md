# #7 Chart: Postgres Capability

Questions that came up while adding the Postgres Capability to `chart/application`, and the answer chosen for each. CloudNativePG and ArgoCD sources were checked on 2026-09-21.

## Which backup mechanism, and which CloudNativePG version is assumed?

**Question.** CloudNativePG has two ways to archive WAL and take base backups to object storage: the in-tree `spec.backup.barmanObjectStore` block on the Cluster, and the Barman Cloud Plugin (CNPG-I), configured through a separate `ObjectStore` resource.

**Options.**
1. In-tree `barmanObjectStore`. Fewer objects, but the CloudNativePG 1.30 documentation ("Backup") marks it deprecated since 1.26, "will be removed in a future release", and points at the plugin for retention policies.
2. The Barman Cloud Plugin. The path the documentation calls officially supported; one more resource per Environment, and one more component for the bootstrap to install.

**Choice.** The plugin. Building the deprecated path into the Platform's contract with every Application would mean a chart migration when it is removed. What the chart relies on, for the bootstrap ticket to pin:

| Component | Version assumed | API the chart renders |
|---|---|---|
| CloudNativePG operator | 1.30.0 (latest stable on 2026-09-21) | `postgresql.cnpg.io/v1` `Cluster`, `ScheduledBackup` |
| Barman Cloud Plugin | 0.15.0 (latest on 2026-09-21) | `barmancloud.cnpg.io/v1` `ObjectStore` |

The plugin is installed once, in the operator's namespace, and reached from the Cluster as `plugins[].name: barman-cloud.cloudnative-pg.io` with `isWALArchiver: true` and `parameters.barmanObjectName` naming the `ObjectStore`, which must be in the Cluster's namespace. The `ScheduledBackup` uses `method: plugin` with `pluginConfiguration.name` set to the same plugin. Field names were taken from the plugin's "Usage", "Object stores" and "Retention" pages and the operator's "Backup" and "Bootstrap" pages.

## What are the objects named?

Chosen: everything that belongs to the database is `<fullname>-db`, so `shop-db` for prod and `shop-staging-db` for staging: the `Cluster`, its `ObjectStore` and its `ScheduledBackup` (different Kinds, one name, one thing to look for). The operator derives the rest: the app Secret is `<cluster>-app` (`shop-db-app`) and the read-write Service `<cluster>-rw`. The migration Job is `<fullname>-migrate`. The database and its owner are both named after the Application (`shop`), so `DATABASE_URL` looks the same in every Environment; the operator quotes the identifiers, so the dashes an Application name may contain are fine.

## Resources, storage and image

**CPU.** The design fixes Postgres at 256 MiB and says nothing about CPU. Chosen: 250m, requests equal to limits, the same as a `small` Application. Equal requests and limits give the Pod the Guaranteed QoS class, which is what CloudNativePG recommends, and a quarter of a vCPU is enough for the databases of a handful of small Applications on a two-vCPU node. It is a chart constant, not a value, like the sizes.

**Storage.** Chosen: a 5Gi PVC. k3s's default `local-path` storage class neither enforces the size nor supports expansion, so the number is a nominal claim on the node's disk and cannot be grown in place later; 5Gi is generous enough that no Phase 1 Application should need to. Tuning it per Application is a Phase 2 concern.

**Image.** Not pinned in the chart. A Cluster without `imageName` gets the operator's default image, so the Postgres major an Environment runs is decided by the operator version the bootstrap pins, in one place, rather than by every values file. If a specific major is ever needed, `imageName` (or an `ImageCatalog`) is a chart change.

## Where the Object Storage credentials come from

**Question.** The `ObjectStore` needs the access and secret key of the Platform's Object Storage. The chart must not carry them.

**Choice.** The `ObjectStore` references a Secret named by `platform.backupsCredentialsSecret` (default `backups-credentials`) with the keys `ACCESS_KEY_ID` and `ACCESS_SECRET_KEY`, the names the plugin documentation uses. The Secret must exist in the Environment's namespace; the chart never sees the values. How it gets there is a follow-up for the bootstrap and CLI tickets: the same pair of keys the OpenTofu roots use, SOPS-encrypted into the Platform repository once per namespace (or once, if every Environment shares a namespace), the same way the wildcard TLS Secret is made available. The consequence to be aware of: one bucket-wide credential in every namespace means the `<application>/<environment>/` prefixes separate Environments by convention, not by access control.

## Backup layout, retention, schedule and compression

- **Path.** `s3://<platform.backupsBucket>/<application>/<environment>/` on `platform.objectStorageEndpoint`. One bucket, created by `infra/state-bucket/` (`itema-iidp-db-backups` by default), holds every database; the prefix is one Environment. Both values are `required` when Postgres is enabled and are expected in the Environment file from `platform.yaml`, next to `baseDomain`. The endpoint has the shape `https://<location>.your-objectstorage.com`, the same as `infra` uses.
- **Retention.** `postgres.backupRetention`, default `30d`, rendered as the `ObjectStore`'s `retentionPolicy` (days, weeks or months). A value rather than a constant so the Platform admin can change it per Environment (a staging database has little reason to keep a month), and because the delete ticket's "final backup kept 30 days" is the same number.
- **Schedule.** One base backup a day at 03:00 UTC, `"0 0 3 * * *"` in the six-field, seconds-first form CloudNativePG uses. `immediate: true` takes the first backup as soon as the `ScheduledBackup` exists: WAL archiving alone cannot restore anything until a base backup exists, and without it a new Environment would be unrecoverable for up to a day. `backupOwnerReference: self` makes the Backup objects the ScheduledBackup's, as in the documentation's example. All Environments back up at the same hour; the databases are small and the bucket is not on the node.
- **Compression.** gzip for both WAL and data. Object Storage is the Platform's second-largest cost line and the CPU it costs is negligible at this size.

## Sync hook in a wave, not PreSync

**Question.** The ticket and the spec say the migration Job is a PreSync hook. On an Environment's first sync that cannot work: PreSync hooks run before any regular resource is applied, the Cluster is a regular resource, so the `<cluster>-app` Secret the Job reads `DATABASE_URL` from does not exist yet. The Job's Pod sits in `CreateContainerConfigError`, the hook never completes, the sync never reaches the Cluster. The end-to-end test (one Web service with Postgres and a migration) would hit exactly this.

**Options.**
1. PreSync as written, and have the CLI write the Environment without the migration command first and add it in a second commit. The CLI cannot see whether the Cluster exists (ADR-0002) and the developer would have to run twice.
2. PreSync as written, with `optional: true` on the Secret reference. The Job starts, the command fails for lack of a database, same deadlock.
3. A `Sync`-phase hook ordered with sync waves: Cluster and ObjectStore in wave -2, the Job (and the ScheduledBackup) in wave -1, the Deployment, Service and Ingress in the default wave 0.

**Choice.** Option 3. ArgoCD's "Sync Phases and Waves" documentation (v3.5.3): hooks and resources are ordered first by phase, then by wave, and the next wave starts only when everything in the current one is in sync and healthy. ArgoCD 3.5.3 ships a health check for `postgresql.cnpg.io/Cluster` (`resource_customizations/postgresql.cnpg.io/Cluster/health.lua`, "Cluster in healthy state" is Healthy), so wave -1 starts only once Postgres is up and the app Secret exists. The intent of the ticket is kept: the Job runs before every rollout, and because a failed Sync hook fails the sync, the Deployment in wave 0 is not applied, which is what stops the rollout. `backoffLimit: 0` and `restartPolicy: Never` make the first failure the verdict; `BeforeHookCreation` removes the previous run's Job before the next. The Application's objects carry no wave annotation, so `deployment.yaml`, `service.yaml` and `ingress.yaml` are unchanged by this. On later syncs the Cluster is already healthy and the only visible difference from PreSync is the two-second wave delay. The cost is that the first sync of an Environment with Postgres waits a minute or two for the database before the Application appears; that is the correct order anyway. Reversing this is a one-word change in `migration-job.yaml` plus dropping the wave annotations.

## What the migration container runs with

Chosen: the Application image, `command: ["sh", "-c", "<migrationCommand>"]` so the command is a shell line (pipes, `&&`, environment expansion work, and the image's own entrypoint is bypassed), the Application's plain `env` plus `DATABASE_URL` from the same Secret as the Deployment, and the Application's size as resources. `PORT` is not set: nothing listens. The Pod template carries `app.kubernetes.io/name`, `app.kubernetes.io/component: migration` and the two `iidp.itema.no/*` labels, but not `app.kubernetes.io/instance`, so the Service selector never matches it and traffic cannot reach a migration Pod; the test checks that only the Deployment's Pods match the selector.

## Labels on the database Pods

Chosen: `spec.inheritedMetadata.labels` on the Cluster with the two `iidp.itema.no/*` labels only, so Grafana Alloy attributes the database's logs to the Application and Environment. The `app.kubernetes.io/*` selector labels are deliberately left out for the same reason as on the Job.

## What is refused at render time

- `postgres.migrationCommand` without `postgres.enabled: true`: "there is no database to migrate". A command that silently rendered nothing would be worse than an error.
- `env` setting `DATABASE_URL` while Postgres is enabled, the same rule as `PORT`: the chart injects it, a duplicate would be ambiguous. With Postgres off a plain `DATABASE_URL` in `env` is allowed; an Application may point at a database the Platform does not run.
- `platform.backupsBucket` and `platform.objectStorageEndpoint` missing while Postgres is enabled, via `required`.

## How kubeconform validates the CRDs

Chosen: a second `-schema-location` pointing at the datreeio CRDs-catalog on GitHub (`https://raw.githubusercontent.com/datreeio/CRDs-catalog/main/{{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json`) next to `default`. The catalog carries `postgresql.cnpg.io/cluster_v1.json`, `scheduledbackup_v1.json` and `barmancloud.cnpg.io/objectstore_v1.json`, all with the plugin-era fields, so every rendered object validates under `-strict` and `-ignore-missing-schemas` is not needed. The catalog follows upstream on its own schedule; if a future chart change uses a field the catalog does not have yet, the fix is to point at that release's CRDs rather than to ignore the kind. The existing `Chart` job needs no new flags; its `helm lint --strict` loop gained the two Postgres fixtures so the templates are linted with Postgres on as well as off.
