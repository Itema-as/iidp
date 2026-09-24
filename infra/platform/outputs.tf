output "node_public_ipv4" {
  description = "Public IPv4 address of the node. Traefik listens here; external-dns points A records at it."
  value       = local.node_public_ipv4
}

output "k3s_version" {
  description = "k3s version the node is meant to run. Pass it to iidp-bootstrap on the node to apply a bump in place."
  value       = var.k3s_version
}

output "argocd_chart_version" {
  description = "argo-cd Helm chart version the node is meant to run. Pass it to iidp-bootstrap on the node to apply a bump in place."
  value       = var.argocd_chart_version
}

output "helm_version" {
  description = "Helm release the node is meant to run. Pass it to iidp-bootstrap on the node to apply a bump in place."
  value       = var.helm_version
}

output "ghcr_registries_yaml" {
  description = "Contents of /etc/rancher/k3s/registries.yaml, the node's GHCR pull credential. Sensitive: tofu output -raw prints it. Copied onto a running node to add or rotate the token (infra/README.md)."
  value       = local.registries_yaml
  sensitive   = true
}

output "ghcr_pull_secret_yaml" {
  description = "Manifest of Secret argocd/ghcr-pull-token, the Deploy gate's copy of the GHCR pull token, which cloud-init writes to /etc/iidp/ghcr-pull-token.yaml. Sensitive: tofu output -raw prints it. Copied onto a running node and applied to add or rotate the token (infra/README.md)."
  value       = local.ghcr_pull_secret_yaml
  sensitive   = true
}

output "age_public_key_command" {
  description = "Prints the age public key the CLI encrypts Platform secrets with. Wait for cloud-init to finish first (a few minutes after apply)."
  value       = "ssh root@${local.node_public_ipv4} cat /root/age.pub"
}
