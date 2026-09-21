# #6 Chart: Web service Kind

Questions that came up while building `chart/application` for the Web service Kind, and the answer chosen for each.

## What are the objects named?

Options: the Helm release name (`.Release.Name`, the usual chart convention, which under ArgoCD is whatever the ArgoCD Application sets), the bare Application name, or the Application name with the Environment folded in.

Chosen: `<name>` for prod and `<name>-staging` for staging, computed from the values alone. ADR-0003 says the values file is the Environment's whole definition, so names should not depend on how ArgoCD happens to name the release. Folding the Environment in the same way the address does (`shop` and `shop-staging`, `shop.app.itma.no` and `shop-staging.app.itma.no`) lets both Environments of an Application share a namespace without colliding and makes the host simply `<object name>.<baseDomain>`. `app.kubernetes.io/instance` carries that same value, so the standard label reads as "the Environment", which is what CONTEXT.md defines an Environment to be: one running instance of an Application. `app.kubernetes.io/name` is the Application.

## Which Traefik annotations, and what about plain HTTP?

Checked against the current Traefik documentation ("Routing configuration, Kubernetes Ingress"): the annotations are `traefik.ingress.kubernetes.io/router.entrypoints` and `traefik.ingress.kubernetes.io/router.tls`, and the class is selected with `spec.ingressClassName: traefik`.

Chosen: `router.entrypoints: websecure` and `router.tls: "true"`, so the Ingress only listens on the TLS entrypoint. Listing `web,websecure` with TLS on would make the plain-HTTP router expect TLS too, and listing `web` without TLS would serve every Application over HTTP as well. Redirecting HTTP to HTTPS is a Traefik entrypoint setting; the bootstrap ticket configures it once on the Platform's Traefik. The chart README says so.

## Where does the wildcard secret live?

cert-manager writes a Certificate's secret into the Certificate's own namespace, and Traefik reads an Ingress's TLS secret from the Ingress's namespace (cert-manager documentation, "Syncing Secrets Across Namespaces"). The chart references `platform.wildcardTLSSecret` (default `wildcard-tls`) in the Environment's namespace, as the ticket asks; making that secret exist there (a Certificate per namespace, replication, or one shared Application namespace) belongs to the bootstrap and the Platform repository layout, not to the chart. The README states the requirement. If the bootstrap instead sets the wildcard as Traefik's default certificate (a `TLSStore` named `default`), the `spec.tls.secretName` can be dropped in a later chart version without changing values.

## Which Kubernetes version does kubeconform validate against?

Options: a fixed recent minor written into the test, or the minor of the k3s release the Platform node runs, read from where infra pins it.

Chosen: read from infra. The test parses the `default` of `variable "k3s_version"` in `infra/platform/variables.tf` (`v1.36.4+k3s1` as of #3) and validates against `1.36.0`; kubeconform publishes schemas per minor, so the patch level is dropped. A second constant in the test would have been a second pin that a k3s bump could silently leave behind. The cost is that the chart tests fail when that file moves or its default changes shape, which is the right failure: it means the pin has to be found again.

## Which YAML library parses the rendered manifests?

Options: `sigs.k8s.io/yaml` or `gopkg.in/yaml.v3`.

Chosen: `gopkg.in/yaml.v3`. Its `Decoder` reads the multi-document stream `helm template` prints natively, it decodes into plain `map[string]any` with no Kubernetes types, and it brings no transitive dependencies; `sigs.k8s.io/yaml` converts through JSON and has no stream decoder, so the output would have to be split by hand first. The skeleton notes recorded cobra as the one dependency beyond the standard library; this adds a second, imported only by the chart test package and never by the binary, because the standard library has no YAML parser and the spec asks the chart tests to parse rendered YAML.

## A liveness probe as well as readiness?

`docs/design.md` names only the readiness probe ("The readiness probe hits `/` unless the values file overrides it"); the ticket instructions ask for readiness and liveness probes on `probe.path`.

Chosen: both, on the same path, with Kubernetes' default timings (three failures ten seconds apart before a restart). A Web service that stops answering its own root for half a minute is better restarted than left wedged, and the single node has no other recovery. If an Application needs a slower liveness check, that is a chart change, not a values one, and design.md should then say so; until then the design doc's sentence is read as describing the default path, not the number of probes.

## Chart tests: which job, and how is lint run?

Chosen: a separate `Chart` job in CI that installs Helm (`azure/setup-helm`, pinned to `v4.3.0`, the version installed locally) and kubeconform (`v0.8.0`, downloaded from its GitHub release and verified against the published SHA-256 since there is no official setup action), runs `helm lint --strict` against each rendering fixture, and runs `go test ./chart/...` with `IIDP_REQUIRE_CHART_TOOLS` set. The `Go` job is untouched; the chart tests skip themselves there because neither tool is on its PATH, so `go test ./...` stays green everywhere. In the `Chart` job the same skip would hide a broken tool install behind a green run, so the variable turns it into a failure there.

`helm lint` renders with `values.yaml`, whose required values are empty, and reports each `required` failure as a warning while still exiting 0. Linting with the fixtures under `--strict` is the form that actually fails on a broken template, so that is what CI runs and what the README documents.

## How is the chart published?

Options: a GoReleaser `publishers` hook inside the existing GoReleaser job, or a second job in the Release workflow.

Chosen: a `chart` job in `.github/workflows/release.yaml` that packages `chart/application` with `--version "${GITHUB_REF_NAME#v}"` and pushes it with `helm push` to `oci://ghcr.io/<owner>/charts`, logged in with `GITHUB_TOKEN` (`packages: write` on that job only). The owner is `GITHUB_REPOSITORY_OWNER` lowercased, the same read-from-the-repository approach the skeleton used for the release owner, so the move to `Itema-as` needs no change. A plain job is easier to read and to re-run than a hook buried in `.goreleaser.yaml`, and it fails independently of the binary release. `version` in `Chart.yaml` stays `0.1.0` as a placeholder for local rendering; `appVersion` is not set because it carries no meaning for a generic chart.

## What is refused at render time?

Chosen: a `fail` with a message naming the value and the accepted ones for an unknown `size`, an unknown `kind`, an `environment` other than `prod` or `staging`, an `env` that sets `PORT` (it is injected from `port`, and a duplicate would be silently ambiguous), and an `application.name` that is not a DNS-1035 label of at most 55 characters (a Service name must start with a letter, and `-staging` must fit inside 63). `kind: static-site` is accepted as a value but fails with "not implemented yet", so the value is already in place for the Static site ticket and a premature use is caught rather than rendered as a Web service. `application.name`, `platform.baseDomain`, `image.repository` and `image.tag` use `required`.

## What is in the package?

Chosen: a `.helmignore` that leaves `chart_test.go` and `testdata/` out of the packaged chart. They sit next to the templates so the repository has one test runner, but the Platform repository has no use for them.
