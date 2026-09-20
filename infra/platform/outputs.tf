output "node_public_ipv4" {
  description = "Public IPv4 address of the node. Traefik listens here; external-dns points A records at it."
  value       = local.node_public_ipv4
}

output "age_public_key_command" {
  description = "Prints the age public key the CLI encrypts Platform secrets with. Wait for cloud-init to finish first (a few minutes after apply)."
  value       = "ssh root@${local.node_public_ipv4} cat /root/age.pub"
}
