# application

The generic Helm chart every Application on Itema's Platform is an instance of ([ADR-0003](../../docs/adr/0003-one-generic-helm-chart-per-application.md)). The Platform repository holds, per Environment, one ArgoCD Application pointing at a pinned version of this chart and one values file; that values file is the Environment's whole definition. The CLI writes it, developers do not edit it by hand, and every Platform convention lives in the templates here rather than in the CLI.

Today the chart renders the Web service Kind: a Deployment, a Service and an Ingress on a Platform address with the Platform's wildcard certificate. The Static site Kind, Postgres, migrations, custom domains and Itema login are added by later tickets.

## Values

| Value | Default | Meaning |
|---|---|---|
| `application.name` | required | The Application's name: lowercase letters, digits and dashes, starting with a letter, at most 55 characters. It names every object and forms the Platform address. |
| `environment` | `prod` | `prod` or `staging`. Anything else fails rendering. |
| `platform.baseDomain` | required | The Platform base domain, for example `app.itma.no`. |
| `platform.wildcardTLSSecret` | `wildcard-tls` | The TLS Secret holding the Platform's wildcard certificate, written by the bootstrap's cert-manager Certificate. It must exist in the namespace the Environment is installed into. |
| `kind` | `web-service` | What the Application is. Only `web-service` renders; `static-site` is reserved and fails with a clear message until it is implemented. Anything else fails. |
| `image.repository` | required | The image, for example `ghcr.io/itema-as/shop`. |
| `image.tag` | required | The tag CI wrote: a commit SHA on `main`, a version on a `v*` tag. |
| `size` | `small` | `small`, `medium` or `large`. See below. Anything else fails. |
| `port` | `3000` | The port the container listens on. |
| `probe.path` | `/` | The path the readiness and liveness probes request. |
| `env` | `{}` | Plain environment variables, name to value. Not for secrets, and it must not set `PORT`. |

## Conventions the chart encodes

**Sizes.** Each size is a fixed CPU and memory amount, applied as both requests and limits so an Environment gets exactly what it was sized for and cannot starve its neighbours on the single node:

| Size | CPU | Memory |
|---|---|---|
| `small` | 250m | 256Mi |
| `medium` | 500m | 512Mi |
| `large` | 1 | 1Gi |

**Port and probes.** The container is told its port through the `PORT` environment variable, taken from `port`, and the Service listens on the same number. Readiness and liveness probes are HTTP GETs to `probe.path` on that port; most Applications need no probe configuration because `/` answers.

**Names and addresses.** prod is the unadorned Application; staging carries a `-staging` suffix. The Deployment, Service and Ingress of prod are named `<name>` and reachable at `<name>.<baseDomain>`; staging's are named `<name>-staging` at `<name>-staging.<baseDomain>`. Both Environments can therefore share a namespace. The single container is named after the Application.

**Ingress.** `ingressClassName: traefik`, routed on Traefik's `websecure` entrypoint with TLS on, terminated with the certificate in `platform.wildcardTLSSecret`. The Ingress does not listen on plain HTTP; redirecting HTTP to HTTPS is an entrypoint setting on the Platform's Traefik, configured by the bootstrap, not something each Application repeats.

**Labels.** Every object, and the Pod template, carries `app.kubernetes.io/name` (the Application), `app.kubernetes.io/instance` (the Environment's object name, `<name>` or `<name>-staging`), `iidp.itema.no/application` and `iidp.itema.no/environment`. Grafana Alloy attributes logs and metrics by the last two. The Service and the Deployment select Pods by the first two, which never change between chart versions.

**Replicas.** One. The Platform is a single node; there is nothing to spread over.

## Rendering locally

From the repository root:

```sh
helm template shop chart/application --values chart/application/testdata/prod-small.yaml
helm template shop chart/application --values chart/application/testdata/staging-medium.yaml
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

`chart_test.go` is a Go test package that shells out to `helm template` with the fixtures in `testdata/`, parses the rendered manifests, and asserts on them: each size's resources, the prod and staging hosts, the default and an overridden probe path, the injected `PORT`, the labels, the TLS secret name, and that unknown sizes, unknown Kinds and bad names are refused. It then runs `kubeconform -strict` on the rendered output against the Kubernetes minor of the k3s release pinned in `infra/platform/variables.tf`, so the node's version is the only pin. The tests skip themselves when `helm` or `kubeconform` is not on `PATH`, so `go test ./...` passes on any machine; the `Chart` job in CI installs both, sets `IIDP_REQUIRE_CHART_TOOLS` so a missing tool fails instead of skipping, and runs them on every pull request.

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
