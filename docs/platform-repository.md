# The Platform repository

The Platform repository (`Itema-as/iidp-platform`) holds the desired state of every Application on the Platform. The CLI writes to it, the deploy workflows write image tags into it, ArgoCD reconciles the Platform from it, and its git log is the audit trail ([ADR-0002](adr/0002-cli-writes-desired-state-to-platform-repository.md)). Developers do not edit it by hand; hand edits are tolerated but unsupported. This page is the contract between the three things that touch it: the CLI (`internal/platformrepo`), the bootstrap, and the deploy workflow.

## Layout

```
platform.yaml                 Platform-wide settings the CLI reads
bootstrap/                    ArgoCD Applications for the Platform components (the bootstrap ticket)
applications/
  .gitkeep
  <name>/                     one directory per Application
    prod/
      application.yaml        the ArgoCD Application for the prod Environment
      values.yaml             the chart values that define the prod Environment
    staging/                  the same two files, when the Application has a staging Environment
```

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

```yaml
baseDomain: app.itma.no
chartVersion: 0.3.1
chartRepository: oci://ghcr.io/itema-as/charts/application
argocdURL: https://argocd.platform.itma.no
grafanaURL: https://itema.grafana.net
```

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
- `repoURL` of the chart source is `chartRepository` from `platform.yaml` without the `oci://` scheme and without the last path segment, which becomes `chart`. `targetRevision` is `chartVersion` at the time the Environment was created.
- Sync is automated with prune and self-heal, so a commit is a deploy and a hand change in the cluster is reverted.
- The resources finalizer makes deleting the ArgoCD Application delete the Environment's resources, which is what `iidp app delete` will rely on.
- The labels carry the same Application and Environment identity the chart puts on every object.

What the bootstrap must provide for this to reconcile: the `default` ArgoCD project (or a stricter one, if the bootstrap changes `project` here and in the CLI together) allowed to use both source repositories and to deploy to any namespace on the in-cluster server; and ArgoCD credentials for the Platform repository and, if the chart package on GHCR is private, for the OCI registry. Nothing has to be replicated into Environment namespaces: the Platform's wildcard certificate is Traefik's default certificate (a `TLSStore` named `default`, see the chart README), so a new namespace needs no secret of its own.

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
```

`image.tag` is empty when the Application is created: no image exists yet. The deploy workflow writes the first tag (a commit SHA on `main`, a version on a `v*` tag), and until then ArgoCD reports the Environment as failing to render because the chart requires a tag. Later Capabilities (Postgres, custom domains, Itema login) add keys to this file; `env` is where plain environment variables go.

## How the CLI writes

1. Validates the flags (the Application name is a lowercase DNS-1035 label of at most 40 characters) before touching anything.
2. Takes the developer's GitHub token from the `gh` CLI (`gh auth token`). Write access to the Platform repository is the authorisation; a push refused for permissions says so and names the repository.
3. Clones `main` shallowly into a temporary directory, reads `platform.yaml`, and refuses if `applications/<name>/` already exists.
4. Writes the Environment's files, commits them with the author from the developer's git configuration and the message `iidp app create <name>`, and pushes to `main`.
5. If the push is rejected because `main` moved, it clones afresh and repeats once. If the Application's directory appeared in the meantime, it fails without writing; the other developer's Application wins.

The temporary directory is removed afterwards.
