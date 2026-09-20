# infra

OpenTofu for the Platform node and its buckets, and the cloud-init that turns a fresh Ubuntu machine into a k3s node running ArgoCD. After the first boot nothing here touches the cluster again: ArgoCD reconciles everything from the Platform repository (ADR-0001, ADR-0002).

Two roots, applied in order:

| Root | State | Creates |
|---|---|---|
| [`state-bucket/`](state-bucket/) | local file | The Object Storage bucket for OpenTofu state and the bucket for database backups |
| [`platform/`](platform/) | S3 backend in the state bucket | The server, its SSH key, its firewall, and the cloud-init that bootstraps k3s and ArgoCD |

The split exists because a bucket cannot hold the state of its own creation. `state-bucket/` is applied once and rarely touched; keep its `terraform.tfstate` somewhere safe (it is gitignored). Losing it is recoverable, since a bucket is one `tofu import` away.

## Prerequisites

- [OpenTofu](https://opentofu.org/docs/intro/install/) 1.10 or newer (`tofu`). CI runs 1.12.
- A Hetzner Cloud project with an API token that has read and write permission.
- Hetzner Object Storage credentials for the same project (Cloud Console: Security > S3 credentials). These are S3 access and secret keys, distinct from the API token.
- An SSH key pair. The public key goes into the node; the private key is the only way onto it.
- The Platform repository (`Itema-as/iidp-platform` by default) reachable over HTTPS with a `bootstrap/` directory. ArgoCD will report the root Application as missing until it exists; nothing else fails.

## First apply

Step 1, the buckets:

```sh
cd infra/state-bucket
cp terraform.tfvars.example terraform.tfvars   # fill in the two Object Storage keys
tofu init
tofu apply
```

Step 2, the node. The S3 backend reads the same Object Storage keys from the standard AWS environment variables:

```sh
cd infra/platform
cp terraform.tfvars.example terraform.tfvars   # fill in hcloud_token and ssh_public_key
export AWS_ACCESS_KEY_ID=<object storage access key>
export AWS_SECRET_ACCESS_KEY=<object storage secret key>
tofu init
tofu apply
```

`apply` returns as soon as Hetzner has handed out the server; cloud-init keeps working for a few minutes after that (k3s download, ArgoCD images). Follow it with:

```sh
ssh root@$(tofu output -raw node_public_ipv4) tail -f /var/log/iidp-bootstrap.log
```

If you changed `state_bucket_name` or `location` in step 1, the backend block in [`platform/backend.tf`](platform/backend.tf) has to agree. Either edit it or pass `-backend-config` at init time; the comment in that file shows both.

## Retrieving the age public key

The bootstrap generates an age key pair on the node the first time it runs and stores the private key only in the Secret `sops-age` (key `keys.txt`) in namespace `argocd`, which is the layout KSOPS mounts into the ArgoCD repo server. The public key is written to `/root/age.pub`. The output `age_public_key_command` prints the exact command:

```sh
tofu output -raw age_public_key_command
# ssh root@<ip> cat /root/age.pub
```

That public key goes into `platform.yaml` in the Platform repository; the CLI encrypts every Platform secret with it. The private key never leaves the cluster.

## Fetching the kubeconfig

Only the Platform admin holds one:

```sh
ssh root@<ip> cat /etc/rancher/k3s/k3s.yaml | sed "s/127.0.0.1/<ip>/" > ~/.kube/iidp.yaml
```

## Bumping k3s or ArgoCD

There are two ways. Both are deliberate, manual steps, as ADR-0001 requires.

**Rebuild (the OpenTofu way).** Change `k3s_version` or `argocd_version` and run `tofu apply`. Both versions are baked into the server's user data, and Hetzner does not allow user data to change on an existing server, so the plan shows the server being **destroyed and recreated**. ADR-0001 accepts that a single node is an outage, and everything on it is rebuilt from the Platform repository. Two things do not come back by themselves:

- **Databases.** Node disk is not durable. Every Application database is restored from its continuous backup in the backup bucket once CloudNativePG comes back; make sure backups are healthy before you recreate the node.
- **The age key.** A fresh node generates a fresh key, and every SOPS-encrypted secret in the Platform repository was encrypted for the old one. Before recreating, save the key: `kubectl -n argocd get secret sops-age -o yaml > sops-age.yaml` (treat that file as the crown jewels). After the new node is up, put it back and restart the repo server:

  ```sh
  kubectl -n argocd delete secret sops-age
  kubectl apply -f sops-age.yaml
  kubectl -n argocd rollout restart deployment argocd-repo-server
  ssh root@<ip> iidp-bootstrap   # refreshes /root/age.pub from the restored Secret
  ```

  If the key is gone, re-encrypt every Platform secret for the new public key with `iidp secret set`.

**In place (the documented command).** On the node, edit `K3S_VERSION` or `ARGOCD_VERSION` at the top of `/usr/local/sbin/iidp-bootstrap` and run it. The k3s installer upgrades the existing installation (a restart of the control plane, a minute or so), and the ArgoCD manifests are re-applied server-side. Databases and the age key stay where they are. Then change the matching variable in OpenTofu so the next rebuild agrees, and expect `tofu plan` to propose a recreate you can leave un-applied until you want one.

## Re-running the bootstrap

`/usr/local/sbin/iidp-bootstrap` on the node is the script cloud-init ran; its log is `/var/log/iidp-bootstrap.log`. It can be run again by hand after a transient failure (a download that timed out, say). Every step converges: the k3s installer is skipped when the requested version is already installed, the manifests are applied server-side, the age key is only generated when the Secret is absent, and the root Application is re-applied unchanged. If the node does not become Ready within ten minutes the script exits non-zero and points at `journalctl -u k3s`.

## Verification without a Hetzner account

```sh
tofu fmt -check -recursive infra
(cd infra/state-bucket && tofu init -backend=false && tofu validate)
(cd infra/platform && tofu init -backend=false && tofu validate)
```

This is what [`.github/workflows/infra.yaml`](../.github/workflows/infra.yaml) runs on every pull request that touches `infra/`. It needs no secrets. Whether the node actually boots into a healthy cluster can only be seen with a real `tofu apply`.

## Swapping the hosting provider

Only [`platform/hetzner.tf`](platform/hetzner.tf) knows about Hetzner: the provider requirement, the provider block, its token variable, and the server, SSH key and firewall resources. It consumes `local.user_data` and sets `local.node_public_ipv4`; the cloud-init template, the remaining variables and the outputs are provider-neutral. Replacing that one file with one that creates an equivalent machine elsewhere is the block swap ADR-0001 promises. The buckets are plain S3 and follow the same pattern: `state-bucket/` talks to an S3 endpoint, not to Hetzner.
