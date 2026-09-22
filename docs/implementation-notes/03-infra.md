# #3 OpenTofu node and cloud-init k3s

Decisions taken while implementing #3 that the ticket and the spec (#1) left open. Sources were checked on 2026-09-21.

## How the Object Storage buckets are created

**Question.** The ticket asks OpenTofu to create two Object Storage buckets. Can the `hetznercloud/hcloud` provider do that?

**Options.**
1. `hcloud` provider. Its current documentation (guide "Use Hetzner Object Storage (S3)", provider v1.69.0) states that Object Storage has no Hetzner Cloud API and can only be managed through third-party providers.
2. `aminueza/minio` provider against the S3 endpoint. This is the workflow Hetzner's own documentation describes ("Creating a Bucket via MinIO Terraform Provider").
3. `hashicorp/aws` provider with a custom endpoint. Works, but drags in a large provider and needs a long list of `skip_*` flags to stop it looking for AWS.

**Choice.** Option 2, `aminueza/minio` `~> 3.43` (3.43.0 is the newest release in the OpenTofu registry). It is the documented path and the provider is small. Credentials are the Object Storage access and secret key, passed as sensitive variables.

## Two roots, both buckets in the first

**Question.** The state bucket cannot store the state of its own creation. Where does that state live, and where does the backup bucket go?

**Options.** One root with a bootstrap dance (apply with local state, then migrate); two roots with only the state bucket in the local-state root and the backup bucket in the S3-backed root; two roots with both buckets in the local-state root.

**Choice.** Two roots, both buckets in `infra/state-bucket/` (local state); `infra/platform/` uses the S3 backend and creates everything else. Keeping both buckets together means the Object Storage credentials and the MinIO provider are configured in one place, and the backup bucket is as static as the state bucket: created once, never changed, never destroyed while an Application has data. The cost is that the backup bucket's state lives in a local file; the state-bucket root is applied once, its state is gitignored, and either bucket is trivially re-adopted with `tofu import` if that file is lost. Versioning is enabled on the state bucket so a bad state write can be rolled back; it is not enabled on the backup bucket, whose retention CloudNativePG manages itself.

## Backend credentials and locking

**Question.** The ticket says Object Storage credentials come from variables, but a backend block cannot read variables.

**Choice.** The `platform` backend reads `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY` from the environment, the standard mechanism for the S3 backend; the bucket name and endpoint are spelled out in `backend.tf` with the same defaults as the state-bucket variables, overridable with `-backend-config`. The `skip_credentials_validation`, `skip_region_validation`, `skip_requesting_account_id`, `skip_metadata_api_check` and `skip_s3_checksum` flags plus `use_path_style` are what the OpenTofu S3 backend documentation lists for S3-compatible stores.

State locking is not configured. OpenTofu's S3-native locking (`use_lockfile`) is documented to rely on conditional writes with the `If-None-Match` header; Hetzner's Object Storage documentation does not list that as supported, and a store that silently ignores the header would give the appearance of a lock without one. There is no DynamoDB. Phase 1 has one Platform admin applying from one machine; revisit when a second person or CI starts applying, by testing `use_lockfile = true` against the real bucket.

## Versions pinned

**k3s: `v1.36.4+k3s1`.** The k3s documentation (docs.k3s.io, "Manual Upgrades") recommends the `stable` channel for production and `INSTALL_K3S_VERSION` for pinning. On 2026-09-21 `https://update.k3s.io/v1-release/channels/stable` resolved to `v1.36.4+k3s1`; `latest` was `v1.37.0+k3s1`, released a week earlier and not yet promoted to stable. Stable wins.

**ArgoCD: `v3.5.3`.** The latest non-prerelease on `github.com/argoproj/argo-cd/releases` on 2026-09-21 (v3.6.0-rc1 exists and was ignored). Installed at the time with `kubectl apply -n argocd --server-side --force-conflicts -f https://raw.githubusercontent.com/argoproj/argo-cd/<version>/manifests/install.yaml`, the form the ArgoCD getting-started guide gives, including the note that server-side apply is required because the CRDs exceed the client-side annotation limit. The bootstrap ticket later replaced this with rendering the argo-cd Helm chart via `helm template` instead (`argocd_chart_version`, `helm_version`, `helm_sha256_linux_amd64` in `platform/variables.tf`): see `docs/implementation-notes/04-bootstrap.md`, "Installing from the chart, not the upstream manifests".

**Providers.** `hetznercloud/hcloud ~> 1.69` and `aminueza/minio ~> 3.43`, the newest releases in the OpenTofu registry. Lock files are committed with hashes for linux and darwin on amd64 and arm64.

**Actions.** `actions/checkout@v7` and `opentofu/setup-opentofu@v2`, the current major tags, with OpenTofu 1.12.6 pinned in the workflow.

## How the age key is generated on the node

**Question.** The spec wants the age private key generated once at bootstrap and stored only in the cluster, in the form KSOPS expects.

**Options.** Generate on the admin's machine and pass it in via a variable (ends up in state and in user data, both readable outside the cluster); generate in a Kubernetes Job (needs an image with age and RBAC before ArgoCD exists); generate on the node in cloud-init.

**Choice.** On the node. The bootstrap script installs the `age` package from Ubuntu 24.04 (universe, 1.1.1), and if the Secret `argocd/sops-age` does not exist, runs `age-keygen` into a `mktemp -d` directory, creates the Secret with `--from-file=keys.txt=...` (the layout the KSOPS README mounts into `argocd-repo-server` as `SOPS_AGE_KEY_FILE=/.config/sops/age/keys.txt`), and deletes the temporary file. The public key in `/root/age.pub` is derived from the Secret with `age-keygen -y` on every run, so it always matches what the cluster holds, including after the admin restores a saved key. The private key is never written to the OpenTofu state or to user data.

The consequence, spelled out in `infra/README.md`: a rebuilt node generates a new key unless the admin saves the Secret first and restores it afterwards. That is one of the reasons version bumps never rebuild the node (next section).

## Version bumps are applied in place, never by recreating the server

**Question.** The k3s and ArgoCD versions are in the user data, and Hetzner does not allow user data to change on an existing server, so a plain `tofu apply` after a bump would destroy and recreate the node. Should `lifecycle { ignore_changes = [user_data] }` prevent that?

**Options.** Let the change recreate the node (the literal reading of "change the variable and re-apply"); or ignore user data changes and apply bumps in place on the node.

**Choice.** Ignore user data changes. Recreating the node destroys not only the age key but every local volume, and CloudNativePG on a single node stores every Application's Postgres data on that disk, so a routine k3s bump would be a data-loss event. The upgrade procedure is: change the variable in OpenTofu (it stays the record of what is meant to be running; `tofu plan` shows no change), then re-run `iidp-bootstrap` on the node with `K3S_VERSION`, `ARGOCD_CHART_VERSION`, `HELM_VERSION` and `HELM_SHA256_LINUX_AMD64` in the environment (see `docs/implementation-notes/04-bootstrap.md` for why ArgoCD is now a chart version, not a manifest one). The script reads those with the rendered values as fallback, skips the k3s and helm installers when the installed version already matches and otherwise lets k3s upgrade in place (the k3s manual-upgrade procedure) and replaces the helm binary, re-renders and re-applies the argo-cd chart server-side with `--force-conflicts`, and leaves the age key alone because its Secret exists. A deliberate rebuild is `tofu apply -replace=hcloud_server.node`, documented as disaster recovery only, after backups are confirmed, with the age key saved and restored around it. The first version of this note chose the opposite; it was reversed once the local-volume consequence was spelled out.

## Root Application shape

The root ArgoCD Application `platform` targets `repoURL = platform_repo_url`, `path = platform_repo_bootstrap_path`, `targetRevision = HEAD`, destination `https://kubernetes.default.svc` namespace `argocd`, with `automated.prune`, `automated.selfHeal`, `CreateNamespace=true` and `ServerSideApply=true`. No `resources-finalizer` is set on the root, so deleting it by mistake leaves the child Applications running. Whether the bootstrap directory is a kustomization or a plain directory is left to ArgoCD's autodetection; the bootstrap ticket decides that.

## Firewall

The ticket says "22, 80 and 443 only". ICMP inbound is allowed as well, following the Hetzner provider's own firewall example: it exposes nothing and keeps `ping` working for the admin. No outbound rules are declared, which in a Hetzner firewall leaves outbound open; the node needs that to pull images and talk to GitHub.

## Directory name

`CONTEXT.md` lists "infra" among the words to avoid for the Platform. The directory is still `infra/` because `docs/design.md` fixes the repository layout (`infra/  OpenTofu for the node, cloud-init for k3s`) and the ticket refers to it; the directory names the tooling, not the Platform.

## Bootstrap re-runs

The cloud-init user data writes the bootstrap to `/usr/local/sbin/iidp-bootstrap` and runs it once from `runcmd`, rather than inlining the commands in `runcmd`. That gives the admin one idempotent command to re-run after a transient failure and one log file (`/var/log/iidp-bootstrap.log`) to read. `apt-get` runs with `DPkg::Lock::Timeout` because `apt-daily` often holds the lock in the first minutes of an Ubuntu cloud image's life, and the wait for the node to become Ready has a ten-minute deadline so a broken k3s start fails loudly instead of hanging cloud-init.
