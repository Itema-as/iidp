# bootstrap

The ArgoCD app-of-apps that installs every Phase 1 Platform component. It is a Helm chart whose values are the Platform repository's `platform.yaml`, rendered by ArgoCD, and whose output is one ArgoCD `Application` per component. ("Application" below means an ArgoCD Application, not an Application in the sense of `CONTEXT.md`, except where it says so.) Nothing here is applied by hand: cloud-init applies one root Application (`infra/platform/cloud-init/user-data.yaml.tftpl`), and the Platform repository points it at this directory. The Platform repository is private (`docs/implementation-notes/12-deploy-workflow.md`, from `docs/design.md`'s access model), so cloud-init also applies ArgoCD's credential for it — an ArgoCD repository Secret for the org GitHub App `iidp-deploy`, which the Deploy gate also commits deploys as — before it applies the root Application, so ArgoCD reads the Platform repository from first boot with nobody touching the cluster (`docs/implementation-notes/41-argocd-platform-repo-credential.md`).

| ArgoCD Application | What it installs | Namespace |
|---|---|---|
| `argocd` | The `argo-cd` chart taking over the ArgoCD cloud-init installed: Dex with the Microsoft Entra ID connector, read-only RBAC with one admin group, the ingress, and KSOPS in the repo server | `argocd` |
| `cert-manager` | cert-manager with its CRDs | `cert-manager` |
| `platform-tls` | [`components/tls`](components/tls): the Let's Encrypt `ClusterIssuer` `letsencrypt` (Cloudflare DNS-01), the wildcard `Certificate` for `*.<baseDomain>` and `<baseDomain>` written to `kube-system/wildcard-tls`, Traefik's default `TLSStore` pointing at it, the k3s `HelmChartConfig` that redirects HTTP to HTTPS, and the `ClusterIssuer` `letsencrypt-http01` (HTTP-01 on Traefik) the application chart uses for custom domains the wildcard does not cover | `cert-manager`, `kube-system` |
| `external-dns` | external-dns with the Cloudflare provider, `sync` policy, TXT ownership (`--txt-owner-id iidp`) and a filter for the zone, reading Ingress hosts | `external-dns` |
| `cloudnative-pg` | The CloudNativePG operator | `cnpg-system` |
| `cnpg-barman-cloud` | The CloudNativePG Barman Cloud plugin, which the application chart's Postgres Capability uses for continuous backups to Object Storage | `cnpg-system` |
| `monitoring` | Grafana's `k8s-monitoring` chart: Alloy shipping pod logs, node and kube-state metrics to Grafana Cloud | `monitoring` |
| `oauth2-proxy` | The Itema login Capability's one shared oauth2-proxy (Entra ID, `entra-id` provider), served at `auth.<baseDomain>` through an Ingress covered by the wildcard, plus the Traefik ForwardAuth `Middleware` `itema-login-auth` the application chart's `login.enabled` Ingress annotation points at. An unauthenticated browser is redirected straight to Entra ID and back (`docs/implementation-notes/77-login-redirect.md`) | `oauth2-proxy` |
| `deploy-gate` | [`components/deploy-gate`](components/deploy-gate): the Deploy gate (`cmd/iidp-deploy-gate`), the one way an Application repository's CI deploys and promotes, served at `deploy.<baseDomain>` through an Ingress covered by the wildcard (below) | `argocd` |

Every version is pinned in [`versions.yaml`](versions.yaml). The Applications share one sync policy (`templates/_helpers.tpl`): automated with prune and self-heal, server-side apply, and unlimited retries, so a component that needs another one's CRDs or namespace converges on its own. Each retry syncs the newest commit (`retry.refresh: true`), so a fix pushed while a sync keeps failing applies on the next retry instead of waiting for someone to terminate the operation. No Application carries the resources finalizer: removing a component from the bootstrap leaves what it installed in the cluster, to be deleted by hand, rather than cascading into the deletion of CRDs and everything defined with them.

TLS works without any Secret being copied around: an Ingress with TLS on and no `secretName` (the Application chart's, and ArgoCD's own) is served with the wildcard from the default store.

## The Platform repository

The root Application `platform` syncs the directory `bootstrap/` of the Platform repository (`Itema-as/iidp-platform`), and this is what it expects to find:

```
platform.yaml                          Platform-wide settings (below)
.sops.yaml                             SOPS creation rules: the age public key from platform.yaml
bootstrap/
  platform-components.yaml             Application pinning this directory, values from platform.yaml
  platform-secrets.yaml                Application for bootstrap/sops, decrypted by KSOPS
  applications.yaml                    Application discovering every Environment's own Application, below
  templates/
    backups-credentials.enc.yaml       Secret backups-credentials (no namespace): ACCESS_KEY_ID,
                                       ACCESS_SECRET_KEY -- copied, byte for byte, into every Environment
                                       with Postgres by iidp app create/add-capability --postgres. Not a
                                       sibling of the files above: the root Application reads every file
                                       directly under bootstrap/ and applies it as a manifest, which this
                                       SOPS document is not (see docs/platform-repository.md)
  sops/
    kustomization.yaml                 generators: [ksops.yaml]
    ksops.yaml                         the KSOPS generator listing the files below
    argocd-entra.enc.yaml              Secret argocd/argocd-entra: clientID, clientSecret, tenant
    cloudflare-api-token-cert-manager.enc.yaml   Secret cert-manager/cloudflare-api-token: apiToken
    cloudflare-api-token-external-dns.enc.yaml   Secret external-dns/cloudflare-api-token: apiToken
    grafana-cloud.enc.yaml             Secret monitoring/grafana-cloud: prometheus-url, prometheus-username,
                                       loki-url, loki-username, access-token
    oauth2-proxy-entra.enc.yaml        Secret oauth2-proxy/oauth2-proxy-entra: clientID, clientSecret,
                                       tenant, cookieSecret
<application>/                         one directory per Application (CONTEXT.md sense), written by the CLI (later tickets)
```

[`test/e2e/fixtures/platform-repo`](../test/e2e/fixtures/platform-repo) is a complete example, with dummy values encrypted for a throwaway key. Only the files directly under `bootstrap/` are read by the root Application (it does not recurse), and none of them is touched by the CLI.

### `platform-components.yaml`

An Application with two sources: this repository at a pinned revision, path `bootstrap`, rendered with `$platform/platform.yaml` as its value file; and the Platform repository itself under the ref `platform`. Two Helm parameters, `bootstrap.repoURL` and `bootstrap.targetRevision`, are set from ArgoCD's build environment (`$ARGOCD_APP_SOURCE_REPO_URL`, `$ARGOCD_APP_SOURCE_TARGET_REVISION`), so the `platform-tls` Application, which points back at `components/tls` in this repository, always follows the same pin. **Upgrading the bootstrap is changing `targetRevision` in this file** to a newer `iidp` release tag.

### `platform-secrets.yaml`

An Application for `bootstrap/sops` in the Platform repository. ArgoCD detects the kustomization, and the repo server, which the `argocd` Application has equipped with `ksops` and `--enable-alpha-plugins --enable-exec`, decrypts each file with the age key in the Secret `argocd/sops-age` (key `keys.txt`, generated by cloud-init). The five Secrets above are what the components consume; the ArgoCD Entra Secret carries the label `app.kubernetes.io/part-of: argocd` so ArgoCD substitutes `$argocd-entra:<key>` in the Dex configuration; `oauth2-proxy`'s reads its own Entra Secret through `extraEnv` instead (see `oauth2-proxy.yaml`'s comment), since its issuer URL has to be built from the tenant id rather than substituted whole. Every Secret carries `kustomize.config.k8s.io/needs-hash: "false"` so its name is not suffixed.

To add or change one: write the Secret in the clear, run `sops --encrypt --in-place bootstrap/sops/<name>.enc.yaml` in the Platform repository (`.sops.yaml` there names the key and encrypts only `data` and `stringData`), commit. Until the CLI's `iidp secret set` exists, that is the procedure for the Platform secrets too.

Until the `argocd` Application has completed its first sync, `platform-secrets` cannot render (no ksops yet) and the ArgoCD UI shows a comparison error on it. It clears by itself on the next refresh.

### `applications.yaml`

An Application with a plain directory source on the Platform repository itself, path `applications`, `directory.recurse: true` and `directory.include: '*/*/application.yaml'`: it picks up exactly the files `docs/platform-repository.md` says the CLI writes, `applications/<name>/<environment>/application.yaml`, and applies each as a plain ArgoCD Application manifest. Every Environment's Application is a child of it, the same app-of-apps shape as `platform-components.yaml` one level up; nothing else made ArgoCD notice a new Application directory, because the root `platform` Application only reads `bootstrap/`, not `applications/`. The `default` AppProject (created by ArgoCD's own install manifest, not overridden by anything the bootstrap adds) is fully permissive out of the box, so it already allows every Environment's two sources (the chart repository and the Platform repository) and any destination namespace; there is nothing to widen. See [`docs/implementation-notes/09-e2e-fixture-application.md`](../docs/implementation-notes/09-e2e-fixture-application.md) for the alternatives considered (an ApplicationSet git directory generator, in particular) and why this was simpler.

### `templates/backups-credentials.enc.yaml`

The Platform's Object Storage access and secret key, encrypted once by the bootstrap wizard for the Platform's age key: a Kubernetes Secret named `backups-credentials` (`chart/application`'s `platform.backupsCredentialsSecret` default), `stringData` `ACCESS_KEY_ID` and `ACCESS_SECRET_KEY`, no `namespace`. It lives in its own `templates/` subdirectory rather than directly under `bootstrap/` or under `bootstrap/sops/`, deliberately: the root Application `platform` reads every file directly under `bootstrap/` and applies each one as a plain manifest (this is how `platform-components.yaml`/`platform-secrets.yaml`/`applications.yaml` themselves reach the cluster) -- a SOPS-encrypted document placed there is applied too, as ciphertext, and fails (`.sops: field not declared in schema`); `bootstrap/sops/`, meanwhile, is applied by `platform-secrets.yaml` into the Platform's own component namespaces, which is not what this file is for either. `bootstrap/templates/` is invisible to both. `iidp app create --postgres` and `iidp app add-capability --postgres` copy it byte for byte into `applications/<name>/<environment>/sops/backups-credentials.enc.yaml` for every Environment that gets Postgres, where that Environment's own ArgoCD Application applies it into its own namespace through the same KSOPS mechanism `platform-secrets.yaml` uses. The copy is never decrypted: SOPS's MAC covers the values, not the file's path, so it decrypts correctly wherever it is copied. See [`docs/platform-repository.md`](../docs/platform-repository.md#bootstraptemplatesbackups-credentialsencyaml-and-the-postgres-capabilitys-own-secret) for the full flow and [`docs/implementation-notes/42-backups-credentials.md`](../docs/implementation-notes/42-backups-credentials.md) for the decisions behind it, including how this location was found.

To add or change it by hand: write the Secret in the clear at `bootstrap/templates/backups-credentials.enc.yaml`, run `sops --encrypt --in-place bootstrap/templates/backups-credentials.enc.yaml` in the Platform repository (`.sops.yaml` there names the key and encrypts only `data`/`stringData`), commit. The bootstrap wizard does this for you (`scripts/bootstrap-wizard.sh`'s `stage_hetzner`/`stage_platform_repo`), keeping the existing file untouched on a re-run unless the Object Storage keys it collects actually changed.

### `platform.yaml`

| Field | Read by | Meaning |
|---|---|---|
| `baseDomain` | bootstrap, CLI | Applications are served under `<application>.<baseDomain>`; the wildcard certificate covers `*.<baseDomain>` and `<baseDomain>` |
| `cloudflareZone` | bootstrap, CLI | The Cloudflare zone containing `baseDomain`: the only zone external-dns manages and where custom domains are automated |
| `argocdURL` | bootstrap, CLI | Where ArgoCD is served; its host must be under `baseDomain` so the wildcard covers it |
| `grafanaURL` | CLI | The Grafana Cloud stack, for the closing summary |
| `chartVersion` | CLI | The application chart version written into new Environments |
| `githubApp.id`, `githubApp.installationId` | nobody (documentation) | The org GitHub App the Deploy gate commits as and ArgoCD reads the Platform repository with; both take it from the Secret cloud-init writes |
| `deployGate.githubOrgId` | bootstrap | Itema-as's numeric GitHub org id (default `1230559`): a deploy's OIDC token and the Application's binding must both carry it |
| `deployGate.image.*` | bootstrap | The gate's image; `tag` empty (the default) means the version of the bootstrap release `platform-components.yaml` pins |
| `deployGate.oidc.issuer`, `deployGate.oidc.jwksURL` | bootstrap | The OIDC issuer the gate trusts (default GitHub Actions') and its key set (default `<issuer>/.well-known/jwks`); only a test cluster changes them |
| `deployGate.githubAPI`, `deployGate.platformRepository`, `deployGate.appSecret` | bootstrap | The GitHub API, the Platform repository the gate writes, and the Secret with the App credential (default `platform-repo-github-app`); only a test cluster changes them |
| `agePublicKey` | CLI | What `iidp secret set` encrypts with; the private key exists only in the cluster |
| `backupsBucket` | CLI | The Object Storage bucket for CloudNativePG backups. Required for `--postgres` |
| `objectStorageEndpoint` | CLI | The S3 endpoint of `backupsBucket`'s location, for example `https://hel1.your-objectstorage.com`. Required for `--postgres` |
| `acme.email` | bootstrap | Optional; where Let's Encrypt sends expiry warnings |
| `acme.server` | bootstrap | The ACME directory; the staging one for a test Platform |
| `argocdAdminGroup` | bootstrap | Object id of the Entra ID group whose members are ArgoCD admins; everyone else who can log in is read-only |
| `clusterName` | bootstrap | The `cluster` label on everything shipped to Grafana Cloud |
| `dnsOwnerId` | bootstrap | The TXT owner id external-dns stamps on its records (default `iidp`) |
| `oauth2Proxy.skipOIDCDiscovery` | bootstrap | Cloud-less test clusters only (default `false`): skips oauth2-proxy's OIDC discovery call at startup, so it starts with a dummy tenant and client id where there is no route to `login.microsoftonline.com` |

[`values.yaml`](values.yaml) carries the defaults for the bootstrap fields.

## ArgoCD

cloud-init installs ArgoCD by rendering the `argo-cd` chart itself with `helm template` (`versions.yaml`'s `argocd.chart` and `helm.version`) and applying the output server-side, rather than from the upstream `install.yaml`; the `argocd` Application then keeps managing it with the same chart and version. Both renders use the same minimum values (`fullnameOverride: argocd`, `crds.install: true`), so the Deployments and StatefulSet this creates already carry the selectors the chart expects, and a sync only ever patches them. This replaced an earlier design where cloud-init installed from `install.yaml` and the first sync had to delete and recreate those workloads (`Replace=true,Force=true`, a selector being immutable), including the application controller's own StatefulSet — which raced the controller's replacement of itself against the repo server's often enough to make `kind bootstrap` flaky on a 2-vCPU runner (`docs/implementation-notes/04-bootstrap.md`, "The takeover race" and "Installing from the chart, not the upstream manifests"). If ArgoCD ever ends up in a broken state, re-run `iidp-bootstrap` on the node: it re-renders and re-applies the chart and the next sync converges.

Login is Entra ID through Dex only; the local `admin` account is disabled. The Entra app registration needs the delegated Microsoft Graph permissions `User.Read` and `GroupMember.Read.All` with admin consent, because Dex resolves group membership at login. The Platform admin does not need the UI to act: `argocd --core` uses the kubeconfig directly.

ArgoCD's own credential for the Platform repository — the Secret `argocd/platform-repo-github-app`, labelled `argocd.argoproj.io/secret-type: repository` — is also cloud-init's, applied right before the root Application; see `infra/README.md` ("Rotating the Platform repository credential") for the credential itself and `docs/implementation-notes/41-argocd-platform-repo-credential.md` for why it is shaped the way it is.

## The Deploy gate

The `deploy-gate` Application renders [`components/deploy-gate`](components/deploy-gate) at the same pin as this chart: a Deployment, a Service and an Ingress at `deploy.<baseDomain>` (TLS with no secret of its own, so Traefik serves the wildcard). It runs the image `ghcr.io/itema-as/iidp-deploy-gate:<version>`, which the release workflow publishes for every `v*` tag, where `<version>` is the pinned bootstrap revision without its `v`: bumping the bootstrap bumps the gate. A pin that is not a release tag names no image; the Application then fails to render with a message saying so, and nothing else in the bootstrap is affected. The node pulls the image with the same read-only GHCR credential it pulls Application images with.

The gate needs the `iidp-deploy` GitHub App's id, installation id and private key. Those are already in the cluster: the Secret `argocd/platform-repo-github-app` cloud-init writes as ArgoCD's credential for the Platform repository (`githubAppID`, `githubAppInstallationID`, `githubAppPrivateKey`; `docs/implementation-notes/41-argocd-platform-repo-credential.md`). The gate runs in the `argocd` namespace and mounts those three keys as a volume, since a pod can only mount a Secret of its own namespace. It needs no Kubernetes API access, so it runs without a service account token and no Role is involved. It reads the files on every call, so rotating the key (`infra/README.md`, "Rotating the Platform repository credential") reaches the gate once the kubelet refreshes the volume, with no restart.

Before committing a tag, the gate checks that the image exists, and for a private image on `ghcr.io` it needs the Platform's GHCR pull token for that. The node's copy is in k3s's `registries.yaml`, which a pod cannot read, so cloud-init also writes the token as the Secret `argocd/ghcr-pull-token` (keys `username` and `token`), from the same OpenTofu variables. The gate mounts those two keys the same way and reads them on every check. Without the Secret the gate's pod does not start; `infra/README.md`, "Adding or rotating the GHCR pull token", creates it on a running node and rotates it together with the node's copy. See `docs/implementation-notes/61-image-check.md`.

Its requests are 5m of CPU and 32Mi of memory, with a 128Mi memory limit: the node is near its CPU request limit, and a deploy is one shallow clone and one push. What it checks and what it commits is in [`docs/platform-repository.md`](../docs/platform-repository.md#how-a-deploy-reaches-the-platform-repository-the-deploy-gate); why it is built this way is in [`docs/implementation-notes/60-deploy-gate.md`](../docs/implementation-notes/60-deploy-gate.md).

## Verifying without a cluster

```sh
helm lint --strict bootstrap --values test/e2e/fixtures/platform-repo/platform.yaml
helm lint --strict bootstrap/components/tls
helm lint --strict bootstrap/components/deploy-gate --set bootstrapRevision=v0.0.0
go test ./bootstrap/...          # renders with helm template and checks the Applications
```

The kind end-to-end test in [`test/e2e`](../test/e2e) bootstraps all of this on a kind cluster from the fixture Platform repository; `.github/workflows/e2e.yaml` runs it when `bootstrap/`, `chart/` or `test/e2e/` change and on every release tag.
