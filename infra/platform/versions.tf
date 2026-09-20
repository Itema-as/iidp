# Provider requirements live next to the resources that need them
# (hetzner.tf), so that swapping the hosting provider is a one-file change.
terraform {
  required_version = ">= 1.10"
}
