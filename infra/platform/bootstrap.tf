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
    # k3s's registry configuration, written by cloud-init's write_files
    # before k3s is installed. base64 for the same reason as the key above:
    # a YAML document nested in the cloud-config YAML then needs no
    # re-indenting and no escaping.
    registries_yaml_b64 = base64encode(local.registries_yaml)
    # The Deploy gate's copy of the same token, as a Secret manifest that
    # cloud-init writes to a file and iidp-bootstrap applies. base64 for
    # the same reason.
    ghcr_pull_secret_yaml_b64 = base64encode(local.ghcr_pull_secret_yaml)
    # The API server's audit policy, which cloud-init writes before k3s
    # starts (the guardrails record what they report in the audit log).
    # A file of its own because the kind harness (test/e2e) configures its
    # API server with the same one; base64 for the same reason as above.
    audit_policy_yaml_b64 = filebase64("${path.module}/cloud-init/audit-policy.yaml")
  })

  # /etc/rancher/k3s/registries.yaml: the credential for ghcr.io, so the
  # node can pull Applications' private images (ADR-0005). One entry under
  # configs and no mirrors: k3s generates a containerd host config for
  # every registry listed in configs, with the registry's default endpoint
  # and these credentials. Also the ghcr_registries_yaml output, which the
  # rotation recipe in infra/README.md copies onto a running node.
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

  # Secret argocd/ghcr-pull-token: the same token for the Deploy gate, a
  # pod that cannot read the node's registries.yaml, which checks with it
  # that an image tag exists before committing it
  # (docs/implementation-notes/61-image-check.md). Its name and keys are a
  # contract with bootstrap/components/deploy-gate. Also the
  # ghcr_pull_secret_yaml output, which the rotation recipe in
  # infra/README.md copies onto a running node, so rotating the token stays
  # one recipe for both copies.
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
