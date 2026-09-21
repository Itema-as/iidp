# bootstrap

The ArgoCD app-of-apps that installs every Phase 1 Platform component. It is a Helm chart whose values are the Platform repository's `platform.yaml`, rendered by ArgoCD, and whose output is one ArgoCD `Application` per component. ("Application" below means an ArgoCD Application, not an Application in the sense of `CONTEXT.md`, except where it says so.) Nothing here is applied by hand: cloud-init applies one root Application (`infra/platform/cloud-init/user-data.yaml.tftpl`), and the Platform repository points it at this directory.

| ArgoCD Application | What it installs | Namespace |
|---|---|---|
| `argocd` | The `argo-cd` chart taking over the ArgoCD cloud-init installed: Dex with the Microsoft Entra ID connector, read-only RBAC with one admin group, the ingress, and KSOPS in the repo server | `argocd` |
| `cert-manager` | cert-manager with its CRDs | `cert-manager` |
| `platform-tls` | [`components/tls`](components/tls): the Let's Encrypt `ClusterIssuer` `letsencrypt` (Cloudflare DNS-01), the wildcard `Certificate` for `*.<baseDomain>` and `<baseDomain>` written to `kube-system/wildcard-tls`, Traefik's default `TLSStore` pointing at it, the k3s `HelmChartConfig` that redirects HTTP to HTTPS, and the `ClusterIssuer` `letsencrypt-http01` (HTTP-01 on Traefik) the application chart uses for custom domains the wildcard does not cover | `cert-manager`, `kube-system` |
| `external-dns` | external-dns with the Cloudflare provider, `sync` policy, TXT ownership (`--txt-owner-id iidp`) and a filter for the zone, reading Ingress hosts | `external-dns` |
| `cloudnative-pg` | The CloudNativePG operator | `cnpg-system` |
| `cnpg-barman-cloud` | The CloudNativePG Barman Cloud plugin, which the application chart's Postgres Capability uses for continuous backups to Object Storage | `cnpg-system` |
| `monitoring` | Grafana's `k8s-monitoring` chart: Alloy shipping pod logs, node and kube-state metrics to Grafana Cloud | `monitoring` |

Every version is pinned in [`versions.yaml`](versions.yaml). The Applications share one sync policy (`templates/_helpers.tpl`): automated with prune and self-heal, server-side apply, and unlimited retries, so a component that needs another one's CRDs or namespace converges on its own. No Application carries the resources finalizer: removing a component from the bootstrap leaves what it installed in the cluster, to be deleted by hand, rather than cascading into the deletion of CRDs and everything defined with them.

TLS works without any Secret being copied around: an Ingress with TLS on and no `secretName` (the Application chart's, and ArgoCD's own) is served with the wildcard from the default store.

## The Platform repository

The root Application `platform` syncs the directory `bootstrap/` of the Platform repository (`Itema-as/iidp-platform`), and this is what it expects to find:

```
platform.yaml                          Platform-wide settings (below)
.sops.yaml                             SOPS creation rules: the age public key from platform.yaml
bootstrap/
  platform-components.yaml             Application pinning this directory, values from platform.yaml
  platform-secrets.yaml                Application for bootstrap/sops, decrypted by KSOPS
  sops/
    kustomization.yaml                 generators: [ksops.yaml]
    ksops.yaml                         the KSOPS generator listing the files below
    argocd-entra.enc.yaml              Secret argocd/argocd-entra: clientID, clientSecret, tenant
    cloudflare-api-token-cert-manager.enc.yaml   Secret cert-manager/cloudflare-api-token: apiToken
    cloudflare-api-token-external-dns.enc.yaml   Secret external-dns/cloudflare-api-token: apiToken
    grafana-cloud.enc.yaml             Secret monitoring/grafana-cloud: prometheus-url, prometheus-username,
                                       loki-url, loki-username, access-token
<application>/                         one directory per Application (CONTEXT.md sense), written by the CLI (later tickets)
```

[`test/e2e/fixtures/platform-repo`](../test/e2e/fixtures/platform-repo) is a complete example, with dummy values encrypted for a throwaway key. Only the two files directly under `bootstrap/` are read by the root Application (it does not recurse), and neither of them is touched by the CLI.

### `platform-components.yaml`

An Application with two sources: this repository at a pinned revision, path `bootstrap`, rendered with `$platform/platform.yaml` as its value file; and the Platform repository itself under the ref `platform`. Two Helm parameters, `bootstrap.repoURL` and `bootstrap.targetRevision`, are set from ArgoCD's build environment (`$ARGOCD_APP_SOURCE_REPO_URL`, `$ARGOCD_APP_SOURCE_TARGET_REVISION`), so the `platform-tls` Application, which points back at `components/tls` in this repository, always follows the same pin. **Upgrading the bootstrap is changing `targetRevision` in this file** to a newer `iidp` release tag.

### `platform-secrets.yaml`

An Application for `bootstrap/sops` in the Platform repository. ArgoCD detects the kustomization, and the repo server, which the `argocd` Application has equipped with `ksops` and `--enable-alpha-plugins --enable-exec`, decrypts each file with the age key in the Secret `argocd/sops-age` (key `keys.txt`, generated by cloud-init). The four Secrets above are what the components consume; the Entra Secret carries the label `app.kubernetes.io/part-of: argocd` so ArgoCD substitutes `$argocd-entra:<key>` in the Dex configuration. Every Secret carries `kustomize.config.k8s.io/needs-hash: "false"` so its name is not suffixed.

To add or change one: write the Secret in the clear, run `sops --encrypt --in-place bootstrap/sops/<name>.enc.yaml` in the Platform repository (`.sops.yaml` there names the key and encrypts only `data` and `stringData`), commit. Until the CLI's `iidp secret set` exists, that is the procedure for the Platform secrets too.

Until the `argocd` Application has completed its first sync, `platform-secrets` cannot render (no ksops yet) and the ArgoCD UI shows a comparison error on it. It clears by itself on the next refresh.

### `platform.yaml`

| Field | Read by | Meaning |
|---|---|---|
| `baseDomain` | bootstrap, CLI | Applications are served under `<application>.<baseDomain>`; the wildcard certificate covers `*.<baseDomain>` and `<baseDomain>` |
| `cloudflareZone` | bootstrap, CLI | The Cloudflare zone containing `baseDomain`: the only zone external-dns manages and where custom domains are automated |
| `argocdURL` | bootstrap, CLI | Where ArgoCD is served; its host must be under `baseDomain` so the wildcard covers it |
| `grafanaURL` | CLI | The Grafana Cloud stack, for the closing summary |
| `chartVersion` | CLI | The application chart version written into new Environments |
| `githubApp.id`, `githubApp.installationId` | CLI | The org GitHub App the deploy workflow writes back with |
| `agePublicKey` | CLI | What `iidp secret set` encrypts with; the private key exists only in the cluster |
| `backupsBucket` | CLI | The Object Storage bucket for CloudNativePG backups |
| `acme.email` | bootstrap | Optional; where Let's Encrypt sends expiry warnings |
| `acme.server` | bootstrap | The ACME directory; the staging one for a test Platform |
| `argocdAdminGroup` | bootstrap | Object id of the Entra ID group whose members are ArgoCD admins; everyone else who can log in is read-only |
| `clusterName` | bootstrap | The `cluster` label on everything shipped to Grafana Cloud |
| `dnsOwnerId` | bootstrap | The TXT owner id external-dns stamps on its records (default `iidp`) |

[`values.yaml`](values.yaml) carries the defaults for the bootstrap fields.

## ArgoCD

cloud-init installs ArgoCD from the upstream `install.yaml` (`versions.yaml`, `argocd.manifest`) and the `argocd` Application then manages it with the `argo-cd` chart of the same release. The chart's Deployments and StatefulSet have different pod selectors from the upstream manifests, and a selector cannot be changed in place, so those workloads carry `Replace=true,Force=true` and `ApplyOutOfSyncOnly=true` keeps that to workloads whose manifest actually changed: the first sync recreates them (a minute of ArgoCD downtime, once), later syncs only touch what changed, and a chart bump restarts ArgoCD rather than rolling it. If the controller ever ends up deleted without its replacement (the sync was interrupted between the two), re-run `iidp-bootstrap` on the node: it re-applies the upstream manifests and the next sync finishes the takeover.

Login is Entra ID through Dex only; the local `admin` account is disabled. The Entra app registration needs the delegated Microsoft Graph permissions `User.Read` and `GroupMember.Read.All` with admin consent, because Dex resolves group membership at login. The Platform admin does not need the UI to act: `argocd --core` uses the kubeconfig directly.

## Verifying without a cluster

```sh
helm lint --strict bootstrap --values test/e2e/fixtures/platform-repo/platform.yaml
helm lint --strict bootstrap/components/tls
go test ./bootstrap/...          # renders with helm template and checks the Applications
```

The kind end-to-end test in [`test/e2e`](../test/e2e) bootstraps all of this on a kind cluster from the fixture Platform repository; `.github/workflows/e2e.yaml` runs it when `bootstrap/`, `chart/` or `test/e2e/` change and on every release tag.
