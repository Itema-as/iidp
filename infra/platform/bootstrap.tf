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
    # The private key travels base64-encoded because templatefile(),
    # cloud-config YAML and bash each have their own escaping rules, and a
    # raw PEM's newlines and dashes are not safe through all three.
    platform_repo_github_app_id              = var.platform_repo_github_app_id
    platform_repo_github_app_installation_id = var.platform_repo_github_app_installation_id
    platform_repo_github_app_private_key_b64 = base64encode(var.platform_repo_github_app_private_key)
    # base64 for the same reason: a YAML document nested in the
    # cloud-config YAML then needs no re-indenting and no escaping.
    registries_yaml_b64 = base64encode(local.registries_yaml)
    # The Deploy gate's copy of the GHCR token, as a Secret manifest.
    ghcr_pull_secret_yaml_b64 = base64encode(local.ghcr_pull_secret_yaml)
    # A file of its own because the kind harness configures its API server
    # with the same one.
    audit_policy_yaml_b64 = filebase64("${path.module}/cloud-init/audit-policy.yaml")
  })

  # /etc/rancher/k3s/registries.yaml: the credential for ghcr.io, so the
  # node can pull private images. No mirrors are needed: k3s generates a
  # containerd host config for every registry listed in configs.
  registries_yaml = yamlencode({
    configs = {
      "ghcr.io" = {
        auth = {
          username = var.ghcr_pull_username
          password = var.ghcr_pull_token
        }
      }
    }
  })

  # The same token for the Deploy gate, which cannot read the node's
  # registries.yaml. Its name and keys are a contract with
  # bootstrap/components/deploy-gate.
  ghcr_pull_secret_yaml = yamlencode({
    apiVersion = "v1"
    kind       = "Secret"
    metadata = {
      name      = "ghcr-pull-token"
      namespace = "argocd"
      labels = {
        "app.kubernetes.io/part-of" = "iidp"
      }
    }
    type = "Opaque"
    stringData = {
      username = var.ghcr_pull_username
      token    = var.ghcr_pull_token
    }
  })
}
