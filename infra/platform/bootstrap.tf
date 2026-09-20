# The node's first boot: provider-neutral cloud-init that installs k3s and
# ArgoCD and hands the cluster to the Platform repository. Any provider that
# accepts cloud-init user data can consume local.user_data unchanged.

locals {
  user_data = templatefile("${path.module}/cloud-init/user-data.yaml.tftpl", {
    k3s_version                  = var.k3s_version
    argocd_version               = var.argocd_version
    platform_repo_url            = var.platform_repo_url
    platform_repo_bootstrap_path = var.platform_repo_bootstrap_path
  })
}
