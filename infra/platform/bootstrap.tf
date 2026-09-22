# The node's first boot: provider-neutral cloud-init that installs k3s and
# ArgoCD and hands the cluster to the Platform repository. Any provider that
# accepts cloud-init user data can consume local.user_data unchanged.

locals {
  user_data = templatefile("${path.module}/cloud-init/user-data.yaml.tftpl", {
    k3s_version                  = var.k3s_version
    argocd_chart_version         = var.argocd_chart_version
    helm_version                 = var.helm_version
    helm_sha256_linux_amd64      = var.helm_sha256_linux_amd64
    platform_repo_url            = var.platform_repo_url
    platform_repo_bootstrap_path = var.platform_repo_bootstrap_path
    # ArgoCD's credential for the Platform repository (issue #41): id and
    # installation id pass through unchanged, but the private key travels
    # base64-encoded, since templatefile()/cloud-config YAML/bash all have
    # their own escaping rules and a raw PEM's newlines and dashes are not
    # safe to carry through all three at once. Decoded on the node, in
    # memory only, right before it is written into the repository Secret.
    platform_repo_github_app_id              = var.platform_repo_github_app_id
    platform_repo_github_app_installation_id = var.platform_repo_github_app_installation_id
    platform_repo_github_app_private_key_b64 = base64encode(var.platform_repo_github_app_private_key)
  })
}
