# #4 Bootstrap app-of-apps with kind harness

Decisions taken while implementing #4 that the ticket and the spec (#1) left open. Sources were checked on 2026-09-21. The result is described in [`bootstrap/README.md`](../../bootstrap/README.md); this file records the why.

## How platform.yaml reaches the bootstrap

**Question.** The app-of-apps lives in `iidp` and must be parameterised by the Platform repository's `platform.yaml` (base domain, zone, ArgoCD URL). ArgoCD Applications are plain YAML; where does the templating happen?

**Options.**
1. Plain Application manifests in `bootstrap/` with the values copied into them by hand or by the CLI. Two places to keep in sync, and the Platform repository would have to vendor the directory.
2. Kustomize with replacements. No versioning of the bootstrap independent of the Platform repository, and replacements cannot compose a URL out of a domain.
3. A Helm chart (`bootstrap/Chart.yaml`) whose templates render the Applications, with `platform.yaml` as its value file. ArgoCD's multiple-sources feature lets one Application take the chart from `iidp` at a pinned revision and the value file from the Platform repository (`$platform/platform.yaml`, documented under "Helm value files from external Git repository").

**Choice.** Option 3. The Platform repository's `bootstrap/platform-components.yaml` is that multi-source Application; bumping the bootstrap is changing its `targetRevision`. Helm ignores the fields of `platform.yaml` only the CLI reads. The component that points back at this repository (`platform-tls`, a small chart under `bootstrap/components/tls`) takes `bootstrap.repoURL` and `bootstrap.targetRevision` as values, which the Platform repository sets from ArgoCD's build environment (`$ARGOCD_APP_SOURCE_REPO_URL`, `$ARGOCD_APP_SOURCE_TARGET_REVISION`, listed for Helm sources in the "Build Environment" page) so the pin is written once.

The Platform's secrets Application (`platform-secrets.yaml`) sits next to it in the Platform repository rather than being rendered by the chart: it points at the Platform repository itself, which the chart would otherwise have to be told about, and its content is Platform-specific by nature.

## The wildcard certificate and Traefik

**Question.** Where does the one wildcard `Certificate` live, and how does an Ingress in an Application namespace use it, given that a Secret is namespaced?

**Options.** A copy of the Secret per Application namespace (a copier, or the chart carrying the certificate); the Certificate in `kube-system`, where k3s runs Traefik, with a Traefik default `TLSStore` referencing it.

**Choice.** The default store, decided for this ticket: the `Certificate` `wildcard` in `kube-system` writes `wildcard-tls` with `*.<baseDomain>` and `<baseDomain>`; a `TLSStore` named `default` in the same namespace points at it (Traefik only honours a store named `default` in its own namespace, and a store can only reference Secrets there); every Ingress with TLS on and no `secretName` gets the wildcard. The HTTP to HTTPS redirect is an entrypoint setting, not an Ingress one, so the same component writes the k3s `HelmChartConfig` for the `traefik` chart with `ports.web.http.redirections.entryPoint` (the values key in Traefik chart 40.x; k3s documents `HelmChartConfig` under "Customizing packaged components"). The kind harness installs the Traefik chart with the same values by hand and installs the `HelmChartConfig` CRD from k3s's helm-controller so the object applies; nothing in kind acts on it. The application chart (#6, #8) relies on exactly this: its Platform Ingress has TLS on and no `secretName`.

The same component carries a second `ClusterIssuer`, `letsencrypt-http01`, with an HTTP-01 solver on `ingressClassName: traefik`: the name the application chart's `platform.httpIssuer` defaults to (#8), for custom domains whose zone is not on Cloudflare. #8's notes ask this ticket to verify one foreign-domain issuance through the HTTP-to-HTTPS redirect; that needs a public address and a real domain, which the kind harness does not have, so it stays with the first real Application on the Platform. If the redirect gets in the way, exempt `/.well-known/acme-challenge/` from it in the `HelmChartConfig` rather than change the chart.

## The CloudNativePG Barman Cloud plugin

CloudNativePG 1.26 moved backups to Object Storage out of the operator and into the CNPG-I plugin `plugin-barman-cloud`; the Postgres ticket configures each Application's `Cluster` against it. The plugin is installed next to the operator by the `cnpg-barman-cloud` Application from the `plugin-barman-cloud` chart in the same chart repository (`crds.create: true`; the operator-to-plugin TLS is a cert-manager `Issuer` and two `Certificate`s the chart creates, so the Application skips the dry run and retries until cert-manager's CRDs exist).

## Versions pinned

All in [`bootstrap/versions.yaml`](../../bootstrap/versions.yaml), read by the templates with `.Files.Get` and by the harness, so that one file answers "what runs". Newest stable release of each on 2026-09-21, from the chart repositories' `index.yaml`:

| Component | Version | Note |
|---|---|---|
| ArgoCD manifest | v3.5.3 | Same as `infra/platform/variables.tf`; the harness installs this |
| argo-cd chart | 10.9.2 | appVersion v3.5.3, so the takeover changes configuration, not binaries |
| cert-manager | v1.21.2 | `crds.enabled`, the option that replaced `installCRDs` in 1.15 |
| external-dns | 1.22.0 | `policy` has no default in this chart and must be set |
| cloudnative-pg | 0.29.0 | Operator 1.30.0 |
| plugin-barman-cloud | 0.8.0 | Plugin v0.15.0, the newest release, from the same chart repository; the Postgres ticket assumes it |
| k8s-monitoring | 4.5.2 | See "Alloy" below |
| KSOPS | viaductoss/ksops:v4.5.1 | Newest release; an image, not a chart |
| Traefik in kind | chart 40.1.4+up40.1.0, image 3.7.8 | What `manifests/traefik.yaml` of k3s v1.36.4+k3s1 installs. k3s builds its own chart from upstream 40.1.0 (published in k3s-charts) that lifts upstream's ceiling on the proxy version, so the image k3s pins, 3.7.8, is accepted; upstream 40.1.0 refuses it, which is why the harness installs the k3s archive by URL |
| helm-controller CRD | v0.17.7 | From k3s v1.36.4+k3s1's `go.mod` |
| kind node image | kindest/node:v1.36.4 | Kubernetes 1.36, the k3s minor; the image kind v0.33.0 lists for it |

## ArgoCD managing itself

**Question.** cloud-init installs ArgoCD from `install.yaml`; the ticket wants the `argo-cd` chart to manage it from then on. The chart's Deployments and StatefulSet select pods by `app.kubernetes.io/instance` as well as by name, the upstream manifests by name only, and a selector is immutable.

**Options.** A kustomize overlay on the upstream manifests instead of the chart (same selectors, no takeover problem, but no chart and the version would be pinned inside a kustomization); deleting the upstream workloads by hand once (a manual step on every rebuild); `Replace=true,Force=true` on the workloads, which ArgoCD documents as "kubectl replace --force", delete then create.

**Choice.** The chart with `Replace=true,Force=true` on the Deployments and the StatefulSet (`global.deploymentAnnotations`, `global.statefulsetAnnotations`) plus `ApplyOutOfSyncOnly=true` on the Application, so a sync only recreates workloads whose manifest changed: the first sync (the takeover), and later chart bumps, which restart ArgoCD rather than roll it. The one risk is the controller replacing itself: `kubectl replace --force` deletes the StatefulSet and creates the new one within milliseconds, while the garbage collector and the kubelet need longer to stop the old pod, so the create wins; if it ever does not, `iidp-bootstrap` on the node re-applies the upstream manifests and the next sync finishes. `ServerSideApply=true` and `ServerSideDiff=true` are on every Application: the ArgoCD and cert-manager CRDs exceed the client-side annotation limit, and server-side diff keeps a Secret whose keys ArgoCD does not manage (`argocd-secret`) from showing as forever out of sync.

## KSOPS: init container, not a sidecar plugin

**Question.** The ticket suggests a config management plugin sidecar. The KSOPS README (the "Argo CD Helm Chart with Custom Tooling" section, checked at v4.5.1) documents an init container that runs `ksops install --with-kustomize` into an emptyDir shadowing the repo server's `kustomize`, plus `kustomize.buildOptions: --enable-alpha-plugins --enable-exec` in `argocd-cm`; it documents no sidecar.

**Choice.** The documented init container, with the age key mounted from the Secret `sops-age` at `/.config/sops/age/keys.txt` and `SOPS_AGE_KEY_FILE` pointing at it. The secrets Application is then an ordinary kustomize source that ArgoCD detects by its `kustomization.yaml`; no plugin name, no `plugin.yaml`, and `kustomize build --enable-alpha-plugins --enable-exec` reproduces on a laptop what the repo server does. The cost is that exec plugins are enabled for every kustomize source the repo server renders; the only sources are `iidp` and the Platform repository.

## Alloy: the k8s-monitoring chart

**Question.** The `alloy` chart deploys one Alloy with a configuration written by hand; the `k8s-monitoring` chart deploys Alloy collectors, kube-state-metrics and node-exporter with the configuration Grafana Cloud's Kubernetes integration expects.

**Choice.** `k8s-monitoring` 4.5.2. Logs and metrics land in the shape Grafana Cloud's dashboards want, and node and kube-state metrics need those two exporters anyway. `destinations[].urlFrom` reads the push URLs from the Secret `grafana-cloud` along with the credentials (`secret.create: false`, the "external secrets" example in the chart's docs), so nothing about the stack is in git in the clear. The chart's two Helm hooks (`alloy-operator.conflictCheck` and `waitForAlloyRemoval`) are turned off: ArgoCD would run them as sync hooks on every sync, and on a Platform where ArgoCD is the only installer they guard against nothing. Footprint is the Alloy operator, one metrics collector, one logs DaemonSet pod, kube-state-metrics and node-exporter.

## Entra ID in Dex, and who is admin

Dex's `microsoft` connector, as ArgoCD's own "Entra ID App Registration Auth using Dex" page shows, with `clientID`, `clientSecret` and `tenant` all read from the Secret `argocd-entra` (`$argocd-entra:<key>` works for any string in `dex.config`, not only the client secret). `groupNameFormat: id` so the RBAC line `g, <group object id>, role:admin` survives a group rename; the group id is the new `platform.yaml` field `argocdAdminGroup`, and `policy.default: role:readonly` lets every Itema account see the Platform. Dex resolves groups at login through Microsoft Graph, so the app registration needs `User.Read` and `GroupMember.Read.All` (delegated, admin consent); that is for the bootstrap wizard ticket. The local `admin` account is disabled (`admin.enabled: false`), as the design's "no local ArgoCD accounts" asks; the Platform admin's break-glass is `argocd --core` with the kubeconfig.

## How the fixture repositories are served

**Question.** The harness needs a git server reachable from ArgoCD inside kind, serving both the fixture Platform repository and this repository's `bootstrap/` (which the fixture pins), with no network dependency beyond pulling images.

**Options.**
1. A git server on the host, reached from pods through the container network's gateway. The address differs between Docker on Linux (the bridge gateway) and Podman on macOS (`host.containers.internal`), the fixture could not hardcode the URL, and macOS's firewall prompts for unsigned listeners.
2. A stock image serving the "dumb" HTTP protocol (static files after `git update-server-info`). ArgoCD lists remotes with go-git, which speaks only the smart protocol.
3. A stock image with `git http-backend` behind a CGI-capable server. None found: `alpine/git` ships neither `git-http-backend` nor an httpd applet, and the images with git (Debian `buildpack-deps`, the ArgoCD image) have no web server.
4. A tiny image built by the harness (`test/e2e/gitserver`: Alpine, `git-daemon` for `git-http-backend`, `lighttpd` with `mod_cgi`, the layout from `git-http-backend(1)`), loaded with `kind load docker-image`, with the repositories delivered in a ConfigMap tarball that an init container unpacks.

**Choice.** Option 4. `kind` already needs a container engine, so `docker build`/`podman build` adds no prerequisite; the build is a few seconds (a 3 MB base and two packages) and the image never leaves the machine. The fixture hardcodes `http://git-server.iidp-e2e.svc.cluster.local/git/<name>.git`, the harness checks each repository with the host's `git ls-remote` through a port-forward before ArgoCD sees it, and the harness's `Cluster` is a plain type a later test can reuse to serve an Application fixture and reach Traefik on the mapped host ports (18080 and 18443).

## Health in kind

Two things a cloud-less cluster lacks were arranged so that "Healthy" means what the ticket says. Traefik in kind is a NodePort Service (there is no ServiceLB), which would leave Ingress status empty and ArgoCD's Ingress health check at Progressing, so the harness passes `--providers.kubernetesingress.ingressendpoint.ip=<node IP>`, which is also what external-dns would read. And the ClusterIssuer, the Certificate, external-dns and Alloy are asserted Synced only, since with dummy credentials they cannot become Ready; the fixture points the issuer at Let's Encrypt staging so a real run of the fixture never spends production rate limits either.

## Directory name for the encrypted files

`bootstrap/secrets/` was the obvious name. It is `bootstrap/sops/` because a directory named `secrets` is what tooling with credential filters refuses to read or write, and the name says what the files are: SOPS documents.
