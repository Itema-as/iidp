# infra

OpenTofu for the Platform node and its buckets, and the cloud-init that turns a fresh Ubuntu machine into a k3s node running ArgoCD. After the first boot nothing here touches the cluster again: ArgoCD reconciles everything from the Platform repository (ADR-0001, ADR-0002).

Two roots, applied in order:

| Root | State | Creates |
|---|---|---|
| [`state-bucket/`](state-bucket/) | local file | The Object Storage bucket for OpenTofu state and the bucket for database backups |
| [`platform/`](platform/) | S3 backend in the state bucket | The server, its SSH key, its firewall, and the cloud-init that bootstraps k3s and ArgoCD |

The split exists because a bucket cannot hold the state of its own creation. `state-bucket/` is applied once and rarely touched; keep its `terraform.tfstate` somewhere safe (it is gitignored). Losing it is recoverable, since a bucket is one `tofu import` away.

## Bootstrap wizard

`../scripts/bootstrap-wizard.sh` walks the Platform admin through every step below, plus Cloudflare, Grafana Cloud, the GitHub App, the node's GHCR pull token and the two Entra app registrations, and writes the Platform repository's `platform.yaml` and `bootstrap/` files at the end. It is idempotent (a value that already exists is detected, shown masked and offered to keep) and safe to explore with no setup at all:

```sh
./scripts/bootstrap-wizard.sh --dry-run --platform-repo ../iidp-platform
```

Run it for real from a clone of this repository, with the Platform repository cloned at `--platform-repo` (default `../iidp-platform`, on `main`):

```sh
./scripts/bootstrap-wizard.sh --platform-repo ../iidp-platform
```

Requires `tofu`, `gh`, `sops`, `ssh`, `jq`, `curl` and bash >= 4.3 (macOS ships bash 3.2; `brew install bash` and invoke it explicitly if `bash --version` shows 3.x). `az` is optional: without it, the Entra stage prints the exact app registration to create by hand and asks for the resulting tenant id, client id and secret instead of creating it automatically. `--no-push` writes and commits the Platform repository without pushing; see `scripts/bootstrap-wizard.sh --help` for every flag, and `docs/implementation-notes/05-bootstrap-wizard.md` for why it is built the way it is.

The wizard exports the state backend credentials only inside its own process, so a `tofu` command in `infra/platform` run afterwards, from any other terminal, needs them loaded first: see [Loading the state backend credentials](#loading-the-state-backend-credentials).

The sections below are the same steps done by hand, for when the wizard cannot run (no Hetzner/Cloudflare/Grafana/GitHub access from the current machine, or a step it got wrong needs redoing on its own) or when you want to see what each command actually does before trusting the wizard with it.

## Prerequisites

- [OpenTofu](https://opentofu.org/docs/intro/install/) 1.10 or newer (`tofu`). CI runs 1.12.
- A Hetzner Cloud project with an API token that has read and write permission.
- Hetzner Object Storage credentials for the same project (Cloud Console: Security > S3 credentials). These are S3 access and secret keys, distinct from the API token.
- An SSH key pair. The public key goes into the node; the private key is the only way onto it.
- The Platform repository (`Itema-as/iidp-platform` by default) with a `bootstrap/` directory. It is private (established in `docs/implementation-notes/12-deploy-workflow.md`, from `docs/design.md`'s access model: `gh auth` and direct commits to `main` as the authorisation presuppose the repository is not otherwise open), so cloud-init needs a credential for it: the org GitHub App the bootstrap wizard's "GitHub App for the Deploy gate and ArgoCD" stage creates (`contents: write`, which already implies the read ArgoCD needs). The Deploy gate mounts the same Secret to commit deploys as that App (`bootstrap/README.md`, "The Deploy gate"), so its name, `argocd/platform-repo-github-app`, and its keys are a contract. Its id, installation id and private key PEM are `platform_repo_github_app_id`, `platform_repo_github_app_installation_id` and `platform_repo_github_app_private_key` in `infra/platform/terraform.tfvars` (`terraform.tfvars.example` documents them); cloud-init writes them into an ArgoCD repository Secret before it applies the root Application (`docs/implementation-notes/41-argocd-platform-repo-credential.md`). ArgoCD will report the root Application as missing until `bootstrap/` exists in the Platform repository; nothing else fails.
- A GitHub classic personal access token with only the `read:packages` scope, for the node to pull Applications' private images from GHCR (ADR-0005). ghcr.io refuses GitHub App and fine-grained tokens for pulls from outside Actions, so it has to be a classic one. It starts out as the Platform admin's own; #56 moves it to a machine user. Create it at [github.com/settings/tokens/new?scopes=read:packages](https://github.com/settings/tokens/new?scopes=read:packages) (Settings > Developer settings > Personal access tokens > Tokens (classic)), with Expiration "No expiration". It goes in `ghcr_pull_token` in `infra/platform/terraform.tfvars`, and the GitHub login that owns it in `ghcr_pull_username`. cloud-init writes both into k3s's `/etc/rancher/k3s/registries.yaml` (mode 600) before k3s first starts (`docs/implementation-notes/59-ghcr-pull-token.md`), and into the Secret `argocd/ghcr-pull-token`, the Deploy gate's copy, which it checks that an image tag exists with (`docs/implementation-notes/61-image-check.md`).

## First apply

Step 1, the buckets:

```sh
cd infra/state-bucket
cp terraform.tfvars.example terraform.tfvars   # fill in the two Object Storage keys
tofu init
tofu apply
```

Step 2, the node. The S3 backend reads the same Object Storage keys from the standard AWS environment variables, which `tofu-env.sh` loads from the file step 1 just filled in (see [the next section](#loading-the-state-backend-credentials)):

```sh
cd infra/platform
cp terraform.tfvars.example terraform.tfvars   # fill in hcloud_token, ssh_public_key, the platform_repo_github_app_* and the ghcr_pull_* values
source ../tofu-env.sh
tofu init
tofu apply
```

`apply` returns as soon as Hetzner has handed out the server; cloud-init keeps working for a few minutes after that (k3s download, ArgoCD images). Follow it with:

```sh
ssh root@$(tofu output -raw node_public_ipv4) tail -f /var/log/iidp-bootstrap.log
```

If you changed `state_bucket_name` or `location` in step 1, the backend block in [`platform/backend.tf`](platform/backend.tf) has to agree. Either edit it or pass `-backend-config` at init time; the comment in that file shows both.

## Loading the state backend credentials

`infra/platform` keeps its state in the state bucket, and its S3 backend reads the Object Storage keys from `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY` (a backend block cannot read OpenTofu variables; see [`platform/backend.tf`](platform/backend.tf)). Nothing sets them in a new terminal: the bootstrap wizard exports them only inside its own process. Without them every `tofu` command in `infra/platform` fails, even `tofu output`, with `No valid credential sources found`.

[`tofu-env.sh`](tofu-env.sh) loads them from `state-bucket/terraform.tfvars`, where the wizard (or step 1 above) already wrote them as `object_storage_access_key` and `object_storage_secret_key`, so the keys are never typed or pasted into shell history. Source it at the start of every terminal session that runs `tofu` against the Platform:

```sh
cd infra/platform
source ../tofu-env.sh
```

- It exports the two variables into the current shell and prints neither key. It works in bash and zsh.
- It fails with a message naming the file, or the missing key, when `state-bucket/terraform.tfvars` does not exist or still holds the example's `"..."` placeholders. It exports nothing in that case.
- Source it rather than running it: a script that runs in its own process cannot change your shell's environment, so running it only prints a reminder.
- `IIDP_STATE_TFVARS=<path> source ../tofu-env.sh` reads a different file, for example when the tfvars are kept outside the checkout.
- On a machine where `infra/platform` has never been initialised (a fresh clone), run `tofu init` once after sourcing it.

`infra/state-bucket` does not need it: that root keeps its state in a local file and reads the same keys as ordinary variables from its own `terraform.tfvars`.

Every recipe below starts with those two lines, run from the root of your clone of this repository, so each works from a new terminal.

## Retrieving the age public key

The bootstrap generates an age key pair on the node the first time it runs and stores the private key only in the Secret `sops-age` (key `keys.txt`) in namespace `argocd`, which is the layout KSOPS mounts into the ArgoCD repo server. The public key is written to `/root/age.pub`. The output `age_public_key_command` prints the exact command:

```sh
cd infra/platform
source ../tofu-env.sh
tofu output -raw age_public_key_command
# ssh root@<ip> cat /root/age.pub
```

Run the command it prints, or run it in one go with `ssh root@$(tofu output -raw node_public_ipv4) cat /root/age.pub`.

That public key goes into `platform.yaml` in the Platform repository; the CLI encrypts every Platform secret with it. The private key never leaves the cluster.

## Reaching the Kubernetes API

Only the Platform admin holds a kubeconfig. The node's firewall ([`platform/hetzner.tf`](platform/hetzner.tf)) opens only ports 22, 80 and 443, so the Kubernetes API on port 6443 cannot be reached from outside the node, by design. `kubectl` reaches it through an SSH tunnel instead. The kubeconfig k3s writes already points at `https://127.0.0.1:6443`, and the API server's certificate is valid for `127.0.0.1`, so the file is used as it is and TLS still verifies through the tunnel.

**1. Fetch the kubeconfig**, once (and again after [a rebuild](#rebuilding-the-node), which creates a new cluster with new certificates). It holds the cluster-admin client key, so it is made readable only by you before anything is written into it:

```sh
cd infra/platform
source ../tofu-env.sh
mkdir -p ~/.kube
touch ~/.kube/iidp.yaml && chmod 600 ~/.kube/iidp.yaml
ssh root@$(tofu output -raw node_public_ipv4) cat /etc/rancher/k3s/k3s.yaml > ~/.kube/iidp.yaml
```

**2. Open the tunnel** in a terminal of its own and leave it running while you use `kubectl`. `Ctrl-C` closes it:

```sh
cd infra/platform
source ../tofu-env.sh
ssh -N -o ExitOnForwardFailure=yes -L 6443:127.0.0.1:6443 root@$(tofu output -raw node_public_ipv4)
```

`-N` runs no remote command, so the connection only forwards local port 6443 to the API server on the node. `ExitOnForwardFailure=yes` makes `ssh` exit with an error if the local port can't be used, instead of staying connected with no forward.

**3. Use `kubectl`** in any other terminal:

```sh
KUBECONFIG=~/.kube/iidp.yaml kubectl get nodes
```

Or `export KUBECONFIG=~/.kube/iidp.yaml` once per terminal. `argocd --core` reads the same kubeconfig.

**If local port 6443 is taken** (the tunnel exits with `bind [127.0.0.1]:6443: Address already in use`, typically because another local cluster such as Docker Desktop, k3d or Rancher Desktop listens there), forward a different local port and point the kubeconfig's server at it. k3s names the cluster in its kubeconfig `default`:

```sh
ssh -N -o ExitOnForwardFailure=yes -L 16443:127.0.0.1:6443 root@$(tofu output -raw node_public_ipv4)
KUBECONFIG=~/.kube/iidp.yaml kubectl config set-cluster default --server=https://127.0.0.1:16443
```

The `ssh` line goes in the tunnel terminal, after the same `cd` and `source` lines as in step 2. The certificate check only looks at the address, not the port, so TLS still verifies.

## Bumping k3s or ArgoCD

Upgrades are deliberate, manual steps (ADR-0001) and they happen **in place**. The node is never recreated for a version bump: its disk holds every Application's Postgres volume (CloudNativePG uses the node's local storage) and the age key, so replacing the server would be a data-loss event. The server resource therefore ignores changes to its user data.

1. Change `k3s_version`, `argocd_chart_version` or `helm_version` in OpenTofu (in `terraform.tfvars`, or the defaults in [`platform/variables.tf`](platform/variables.tf); a `helm_version` bump also needs its `helm_sha256_linux_amd64` checksum, from `https://get.helm.sh/helm-<version>-linux-amd64.tar.gz.sha256sum`) and commit. The variables are the record of what is meant to be running; `tofu plan` shows no change, by design.
2. Apply the same versions on the node by re-running the bootstrap with them in the environment:

   ```sh
   cd infra/platform
   source ../tofu-env.sh
   ssh root@$(tofu output -raw node_public_ipv4) \
     K3S_VERSION=$(tofu output -raw k3s_version) \
     ARGOCD_CHART_VERSION=$(tofu output -raw argocd_chart_version) \
     HELM_VERSION=$(tofu output -raw helm_version) \
     iidp-bootstrap
   ```

   Any variable can be left out to keep that component as it is (`HELM_SHA256_LINUX_AMD64` only matters together with a `HELM_VERSION` bump). The k3s installer upgrades the existing installation when the requested version differs from the installed one (the control plane restarts, about a minute; workloads keep running) and is skipped when it does not; helm is replaced in place the same way when its version differs; ArgoCD is installed by rendering the argo-cd chart at the requested version with `helm template` and applying the output server-side (`bootstrap/README.md`, "ArgoCD"), so the argocd bootstrap Application's next sync only ever patches; the age key is left alone because its Secret exists; the root Application is re-applied unchanged. Databases and volumes are untouched.

A first boot uses the versions rendered into the user data at that time. A node that was rebuilt later therefore comes up on whatever the variables said when `tofu apply -replace` ran, which is why step 1 comes first.

## Rotating the Platform repository credential

The same in-place pattern as version bumps, for the same reason: the credential is baked into the user data cloud-init ran once, and `tofu apply` never re-runs it (`ignore_changes = [user_data]`), so a new PEM in `terraform.tfvars` alone changes nothing on the node.

1. Get a new private key for the App (GitHub App settings > "Generate a private key"; the old key keeps working until it is explicitly deleted, so there is no window with no working key) and put its PEM in `platform_repo_github_app_private_key` in `terraform.tfvars` (`platform_repo_github_app_id`/`platform_repo_github_app_installation_id` only change if the App itself was recreated). Commit if `terraform.tfvars` is tracked anywhere outside this machine; `tofu plan` shows no change, by design.
2. Apply it on the node by re-running the bootstrap with the new key, base64-encoded, in the environment:

   ```sh
   cd infra/platform
   source ../tofu-env.sh
   ssh root@$(tofu output -raw node_public_ipv4) \
     PLATFORM_REPO_GITHUB_APP_PRIVATE_KEY_B64=$(base64 < /path/to/new-key.pem | tr -d '\n') \
     iidp-bootstrap
   ```

   This re-applies the ArgoCD repository Secret (`argocd/platform-repo-github-app`) with the new key; everything else the script does is a no-op re-check. Delete `new-key.pem` from the admin's machine afterward; it is never written to the node's disk (see `docs/implementation-notes/41-argocd-platform-repo-credential.md` for why cloud-init decodes it only in memory). Once ArgoCD has synced with the new key, delete the old key from the App's settings page.

## Adding or rotating the GHCR pull token

The node pulls Applications' private images from ghcr.io with the classic token in k3s's `/etc/rancher/k3s/registries.yaml`. The Deploy gate, a pod that cannot read that file, checks with its own copy of the same token that an image tag exists before committing it: the Secret `argocd/ghcr-pull-token` (keys `username` and `token`), whose manifest sits next to it in `/etc/iidp/ghcr-pull-token.yaml` (`docs/implementation-notes/61-image-check.md`). cloud-init writes both files once, at first boot, and `iidp-bootstrap` applies the Secret from the second. `tofu apply` never re-runs cloud-init, so a node created before the token existed, or before the gate needed it, or a new token, needs both files written over ssh, k3s restarted (k3s reads its file only when it starts) and the Secret applied. The steps below do all of it without relying on the node's own `iidp-bootstrap`, so they work on every node, however old its bootstrap script is. Both files belong to iidp alone; they are replaced whole.

Use the same steps to add the token to a running node for the first time, to replace it, or to move it to another account (the machine user of #56).

1. **Create the token**, signed in to GitHub as the account that will own it: [github.com/settings/tokens/new?scopes=read:packages](https://github.com/settings/tokens/new?scopes=read:packages&description=iidp%20node%20GHCR%20pull) (Settings > Developer settings > Personal access tokens > Tokens (classic) > Generate new token (classic)), Expiration "No expiration", scope `read:packages` and nothing else. The account needs read access to the Applications' packages; an organisation owner has it, and a machine user gets it through the Application repositories, whose access their images inherit.
2. **Record it in `infra/platform/terraform.tfvars`**: `ghcr_pull_token` is the token and `ghcr_pull_username` the owner's GitHub login (see `terraform.tfvars.example`). Re-running `../scripts/bootstrap-wizard.sh` writes both, after checking the token's scopes, but it also walks through every other stage. By hand, check the scopes first. `X-OAuth-Scopes` must say `read:packages`, and must be there at all (a fine-grained token has no such header). `read -s` keeps the token out of your shell history:

   ```sh
   read -rs GHCR_TOKEN   # paste the token, then Enter
   curl -sS -o /dev/null -D - -H "Authorization: Bearer $GHCR_TOKEN" https://api.github.com/user | grep -i '^x-oauth-scopes'
   unset GHCR_TOKEN
   ```

3. **Render the files.** `local.registries_yaml` and `local.ghcr_pull_secret_yaml` build them from the two variables, and the sensitive outputs `ghcr_registries_yaml` and `ghcr_pull_secret_yaml` expose them, so `tofu apply` has to run once to update the outputs:

   ```sh
   cd infra/platform
   source ../tofu-env.sh
   tofu apply
   ```

   The plan must change no resource, only the `ghcr_registries_yaml` and `ghcr_pull_secret_yaml` outputs (the server ignores changes to its user data). If it shows anything else, answer no and find out why first. On a node that already has the token, only `ghcr_pull_secret_yaml` is new the first time after #61; that is expected.
4. **Write it on the node and restart k3s.** The file is created with mode 600 (`umask 077`) and moved into place whole:

   ```sh
   tofu output -raw ghcr_registries_yaml | ssh root@$(tofu output -raw node_public_ipv4) \
     'umask 077 && cat > /etc/rancher/k3s/registries.yaml.new && mv /etc/rancher/k3s/registries.yaml.new /etc/rancher/k3s/registries.yaml && systemctl restart k3s'
   ```

   Restarting k3s restarts the control plane only: running Pods keep running and nothing is re-pulled. The API is back within a minute; this waits for the node to be Ready again:

   ```sh
   ssh root@$(tofu output -raw node_public_ipv4) \
     'until kubectl wait --for=condition=Ready node --all --timeout=10s >/dev/null 2>&1; do sleep 5; done; kubectl get nodes'
   ```

5. **Give the Deploy gate its copy.** The manifest goes to `/etc/iidp/ghcr-pull-token.yaml` the same way, where a later `iidp-bootstrap` run finds the current token rather than the first boot's, and is applied from there. Server-side apply keeps the token out of a `last-applied-configuration` annotation:

   ```sh
   tofu output -raw ghcr_pull_secret_yaml | ssh root@$(tofu output -raw node_public_ipv4) \
     'umask 077 && mkdir -p /etc/iidp && cat > /etc/iidp/ghcr-pull-token.yaml.new && mv /etc/iidp/ghcr-pull-token.yaml.new /etc/iidp/ghcr-pull-token.yaml && kubectl apply --server-side -f /etc/iidp/ghcr-pull-token.yaml'
   ```

   It prints `secret/ghcr-pull-token serverside-applied`. The gate reads the token on every check, so a running gate uses the new one once the kubelet refreshes its volume (within about a minute), with no restart. A gate that was waiting for the Secret (its pod stuck in `ContainerCreating`, with a `FailedMount` event naming `ghcr-pull-token`) starts on its own.
6. **Check it** by pulling a private image on the node, for example an Application's image (`ghcr.io/itema-as/<application>:<tag>`):

   ```sh
   ssh root@$(tofu output -raw node_public_ipv4) crictl pull ghcr.io/itema-as/<application>:<tag>
   ```

   It prints `Image is up to date for sha256:...`. Without a working credential the same command fails with `403 Forbidden` or `401 Unauthorized`; running it once before step 4 shows the difference. If it still fails after step 4: `ssh root@<ip> cat /etc/rancher/k3s/registries.yaml` shows the username and token the node uses, `journalctl -u k3s` shows whether k3s read the file, and a token that passed the scope check but is refused by ghcr.io points at the organisation's setting for classic tokens (organisation Settings > Personal access tokens), or at an account with no read access to that package.

   The gate's copy is the same token, so the pull above proves it too once `ssh root@<ip> kubectl -n argocd get secret ghcr-pull-token` lists the Secret. The gate itself shows it on the next deploy of a private image: a tag that exists is committed, and a refusal saying "couldn't check ... the Platform's GHCR pull token cannot read it" means the Secret holds a token ghcr.io refuses. Its `username` is readable with `kubectl -n argocd get secret ghcr-pull-token -o jsonpath='{.data.username}' | base64 -d` on the node.
7. **When replacing a token,** delete the old one on the owning account's Tokens (classic) page once step 6 passes. Until then both work, so there is no moment with no working credential.

A node rebuilt with `tofu apply -replace` gets both files from whatever `terraform.tfvars` holds at that time, which is why step 2 comes first.

## Rebuilding the node

Only for disaster recovery, after confirming the database backups in the backup bucket are current. A rebuild **destroys every local volume, so every Application database on the node, and the age key**.

**Before the rebuild, save the age key.** With [the tunnel](#reaching-the-kubernetes-api) open to the current node, write it to a file outside this (public) repository's checkout, made readable only by you before the key goes into it. Treat that file as the crown jewels:

```sh
touch ~/iidp-sops-age.yaml && chmod 600 ~/iidp-sops-age.yaml
KUBECONFIG=~/.kube/iidp.yaml kubectl -n argocd get secret sops-age -o yaml > ~/iidp-sops-age.yaml
```

**Rebuild.** Close the tunnel first; it points at the node that is about to go:

```sh
cd infra/platform
source ../tofu-env.sh
tofu apply -replace=hcloud_server.node
```

**After the new node is up, put the key back and restart the repo server.** The new node is a new cluster with its own certificates, so fetch its kubeconfig and open a tunnel to it again ([steps 1 and 2](#reaching-the-kubernetes-api)). If `ssh` refuses with `REMOTE HOST IDENTIFICATION HAS CHANGED`, the new server was given the old address; the host key changed with the rebuild, so drop the old one with `ssh-keygen -R <ip>` and connect again. Then:

```sh
export KUBECONFIG=~/.kube/iidp.yaml
kubectl -n argocd delete secret sops-age
kubectl apply -f ~/iidp-sops-age.yaml
kubectl -n argocd rollout restart deployment argocd-repo-server
```

and refresh `/root/age.pub` on the node from the restored Secret:

```sh
cd infra/platform
source ../tofu-env.sh
ssh root@$(tofu output -raw node_public_ipv4) iidp-bootstrap
```

If the key is gone, every SOPS-encrypted secret in the Platform repository has to be re-encrypted for the new public key with `iidp secret set`. Application databases are restored from their continuous backups once CloudNativePG is back.

## Re-running the bootstrap

`/usr/local/sbin/iidp-bootstrap` on the node is the script cloud-init ran; its log is `/var/log/iidp-bootstrap.log`. It can be run again by hand after a transient failure (a download that timed out, say), with or without `K3S_VERSION`, `ARGOCD_CHART_VERSION`, `HELM_VERSION` and `HELM_SHA256_LINUX_AMD64` in the environment. Every step converges: the k3s and helm installers are skipped when the requested version is already installed, the argo-cd chart is re-rendered and applied server-side, the age key is only generated when the Secret is absent, the Deploy gate's `ghcr-pull-token` Secret is re-applied from `/etc/iidp/ghcr-pull-token.yaml`, and the root Application is re-applied unchanged. It never touches `/etc/rancher/k3s/registries.yaml` or `/etc/iidp/ghcr-pull-token.yaml` (see [Adding or rotating the GHCR pull token](#adding-or-rotating-the-ghcr-pull-token), which keeps both current). A node whose script predates #61 does not apply the Secret at all; the recipe does. If the node does not become Ready within ten minutes the script exits non-zero and points at `journalctl -u k3s`.

## Verification without a Hetzner account

```sh
tofu fmt -check -recursive infra
(cd infra/state-bucket && tofu init -backend=false && tofu validate)
(cd infra/platform && tofu init -backend=false && tofu validate)
go test ./infra/...
```

This is what [`.github/workflows/infra.yaml`](../.github/workflows/infra.yaml) and the main Go CI job run on every pull request that touches `infra/`. The `go test` includes `test/tofu-env/run.sh`, which sources [`tofu-env.sh`](tofu-env.sh) against fixture files under bash and, when it is installed, zsh (`bash test/tofu-env/run.sh` runs it on its own). None of it needs secrets: `tofu validate` (unlike `plan`/`apply`) does not require variables to have values, even required ones with no default, so `platform_repo_github_app_private_key`, `ghcr_pull_token` and their siblings need nothing here. Its cloud-init tests (`infra/platform`) mostly read the template as text instead of rendering it through OpenTofu, since reproducing `templatefile()`'s own template syntax in Go was judged not worth it for what they check (see `docs/implementation-notes/41-argocd-platform-repo-credential.md`). The `registries.yaml` tests (which also cover the Deploy gate's `ghcr-pull-token` file) render it twice: once with a Go stand-in for the plain `${name}` substitution the template uses, which always runs, and once through `tofu` itself against fixture variables, which runs only where `tofu` is installed (`IIDP_REQUIRE_TOFU=1` makes a missing `tofu` fail instead; see `docs/implementation-notes/59-ghcr-pull-token.md`). Whether the node actually boots into a healthy cluster, and whether ArgoCD actually reads the Platform repository, can only be seen with a real `tofu apply`.

## Swapping the hosting provider

Only [`platform/hetzner.tf`](platform/hetzner.tf) knows about Hetzner: the provider requirement, the provider block, its token variable, and the server, SSH key and firewall resources. It consumes `local.user_data` and sets `local.node_public_ipv4`; the cloud-init template, the remaining variables and the outputs are provider-neutral. Replacing that one file with one that creates an equivalent machine elsewhere is the block swap ADR-0001 promises. The buckets are plain S3 and follow the same pattern: `state-bucket/` talks to an S3 endpoint, not to Hetzner.
