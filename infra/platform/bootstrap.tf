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
  })
}
