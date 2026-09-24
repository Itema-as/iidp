# Provider-neutral inputs. The Hetzner credential lives in hetzner.tf next
# to the provider that consumes it.

# Node

variable "server_name" {
  description = "Name of the node at the hosting provider. Also used as the prefix for the SSH key and the firewall."
  type        = string
  default     = "iidp-node"
}

variable "server_type" {
  description = "Machine size. CPX22 (2 vCPU, 4 GB) is the agreed Phase 1 size; CPX32 is the first step up."
  type        = string
  default     = "cpx22"
}

variable "location" {
  description = "Datacenter location. Helsinki (hel1) keeps the node next to the Object Storage buckets."
  type        = string
  default     = "hel1"
}

variable "image" {
  description = "Operating system image for the node."
  type        = string
  default     = "ubuntu-24.04"
}

variable "ssh_public_key" {
  description = "Public key of the Platform admin. The only way onto the node; the kubeconfig is fetched over this connection."
  type        = string

  validation {
    condition     = can(regex("^(ssh-(ed25519|rsa)|ecdsa-sha2-nistp[0-9]+|sk-(ssh-ed25519|ecdsa-sha2-nistp256))@?[^ ]* ", var.ssh_public_key))
    error_message = "ssh_public_key must be an OpenSSH public key, for example the contents of ~/.ssh/id_ed25519.pub."
  }
}

# Software pinned by cloud-init. These are baked into the user data at first
# boot; a later change is applied in place by re-running iidp-bootstrap on
# the node with the new value (see infra/README.md), never by recreating
# the server.

variable "k3s_version" {
  description = "k3s release to install, in the form vX.Y.Z+k3sN. Default is the k3s stable channel as of 2026-09-21."
  type        = string
  default     = "v1.36.4+k3s1"

  validation {
    condition     = can(regex("^v1\\.[0-9]+\\.[0-9]+\\+k3s[0-9]+$", var.k3s_version))
    error_message = "k3s_version must look like v1.36.4+k3s1."
  }
}

variable "argocd_chart_version" {
  description = "argo-cd Helm chart version cloud-init renders with helm template and applies; the argocd bootstrap Application then manages ArgoCD with the same chart and version. Must match bootstrap/versions.yaml argocd.chart. Default is the newest stable release as of 2026-09-21."
  type        = string
  default     = "10.9.2"

  validation {
    condition     = can(regex("^[0-9]+\\.[0-9]+\\.[0-9]+$", var.argocd_chart_version))
    error_message = "argocd_chart_version must look like 10.9.2."
  }
}

variable "helm_version" {
  description = "Helm release cloud-init downloads (linux amd64) to render the argo-cd chart, in the form vX.Y.Z. Must match bootstrap/versions.yaml helm.version. Default is the newest stable v3 release as of 2026-09-21."
  type        = string
  default     = "v3.22.0"

  validation {
    condition     = can(regex("^v[0-9]+\\.[0-9]+\\.[0-9]+$", var.helm_version))
    error_message = "helm_version must look like v3.22.0."
  }
}

variable "helm_sha256_linux_amd64" {
  description = "sha256 checksum of the linux amd64 release tarball named by helm_version, published at https://get.helm.sh/helm-<version>-linux-amd64.tar.gz.sha256sum. Must match bootstrap/versions.yaml helm.sha256.linuxAmd64. Verified before the binary is installed."
  type        = string
  default     = "1e4ab49e429626cf6c6958d914248b78c9730803c2751b87627e171dc800e7bb"

  validation {
    condition     = can(regex("^[0-9a-f]{64}$", var.helm_sha256_linux_amd64))
    error_message = "helm_sha256_linux_amd64 must be a 64-character lowercase hex sha256 checksum."
  }
}

# Platform repository

variable "platform_repo_url" {
  description = "Git URL of the Platform repository the root ArgoCD Application reconciles from."
  type        = string
  default     = "https://github.com/Itema-as/iidp-platform.git"
}

variable "platform_repo_bootstrap_path" {
  description = "Directory in the Platform repository holding the ArgoCD Applications for the Platform components."
  type        = string
  default     = "bootstrap"
}

# ArgoCD's credential for the Platform repository. The same org GitHub App
# the deploy workflow uses for CI write-back (contents: write, which
# implies the read ArgoCD needs) doubles as this credential: cloud-init
# writes it into an ArgoCD repository Secret (docs/implementation-notes/
# 41-argocd-platform-repo-credential.md) so the root Application can
# reconcile the private Platform repository from first boot with nobody
# touching the cluster. No default: every Platform has its own App.

variable "platform_repo_github_app_id" {
  description = "Id of the org GitHub App (bootstrap wizard stage \"GitHub App for CI write-back\") that authenticates ArgoCD to the Platform repository. Matches platform.yaml's githubApp.id in the Platform repository."
  type        = number

  validation {
    condition     = var.platform_repo_github_app_id > 0
    error_message = "platform_repo_github_app_id must be a positive number."
  }
}

variable "platform_repo_github_app_installation_id" {
  description = "Installation id of the App on the org that owns the Platform repository. Matches platform.yaml's githubApp.installationId."
  type        = number

  validation {
    condition     = var.platform_repo_github_app_installation_id > 0
    error_message = "platform_repo_github_app_installation_id must be a positive number."
  }
}

variable "platform_repo_github_app_private_key" {
  description = "PEM private key of the App, downloaded once when the App is created. Sensitive and git-ignored like hcloud_token; rotate by pasting a new PEM here and re-running iidp-bootstrap on the node with the new value (tofu apply alone does nothing because user_data changes are ignored, see infra/README.md)."
  type        = string
  sensitive   = true

  validation {
    condition     = can(regex("BEGIN.*PRIVATE KEY", var.platform_repo_github_app_private_key))
    error_message = "platform_repo_github_app_private_key must be the PEM contents of the GitHub App's private key (starts with -----BEGIN ... PRIVATE KEY-----)."
  }
}

# The node's credential for pulling Applications' private images from GHCR
# (ADR-0005). ghcr.io accepts only a classic personal access token for
# pulls from outside Actions, never a GitHub App token, so this is one
# classic token with only the read:packages scope. cloud-init writes both
# values into k3s's /etc/rancher/k3s/registries.yaml (local.registries_yaml
# in bootstrap.tf) before k3s first starts. Like the App key above, a change
# here never reaches a running node through tofu apply alone: see "Adding
# or rotating the GHCR pull token" in infra/README.md. No default: the
# token belongs to one GitHub account (the Platform admin's at first, a
# machine user's after #56).

variable "ghcr_pull_username" {
  description = "GitHub login of the account that owns ghcr_pull_token, sent to ghcr.io as the basic-auth username. The bootstrap wizard reads it from the token itself (GET /user)."
  type        = string

  validation {
    condition     = can(regex("^[A-Za-z0-9][A-Za-z0-9-]{0,38}$", var.ghcr_pull_username))
    error_message = "ghcr_pull_username must be a GitHub login: letters, digits and hyphens, at most 39 characters."
  }
}

variable "ghcr_pull_token" {
  description = "Classic personal access token with only the read:packages scope, owned by ghcr_pull_username. Sensitive and git-ignored like hcloud_token. The bootstrap wizard checks its scopes before storing it."
  type        = string
  sensitive   = true

  validation {
    condition     = can(regex("^[A-Za-z0-9_]+$", var.ghcr_pull_token))
    error_message = "ghcr_pull_token must be a GitHub token: letters, digits and underscores (a classic token starts with ghp_)."
  }
}
