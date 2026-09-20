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

# Software pinned by cloud-init. Changing either of these changes the user
# data, and a change to user data recreates the server (see infra/README.md).

variable "k3s_version" {
  description = "k3s release to install, in the form vX.Y.Z+k3sN. Default is the k3s stable channel as of 2026-09-21."
  type        = string
  default     = "v1.36.4+k3s1"

  validation {
    condition     = can(regex("^v1\\.[0-9]+\\.[0-9]+\\+k3s[0-9]+$", var.k3s_version))
    error_message = "k3s_version must look like v1.36.4+k3s1."
  }
}

variable "argocd_version" {
  description = "ArgoCD release whose manifests/install.yaml is applied, in the form vX.Y.Z. Default is the latest stable release as of 2026-09-21."
  type        = string
  default     = "v3.5.3"

  validation {
    condition     = can(regex("^v[0-9]+\\.[0-9]+\\.[0-9]+$", var.argocd_version))
    error_message = "argocd_version must look like v3.5.3."
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
