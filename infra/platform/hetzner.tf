# Everything that knows about Hetzner is in this file: the provider
# requirement, the provider configuration, its credential, and the machine.
# Moving the Platform to another host means replacing this file with one
# that creates an equivalent machine, feeds it local.user_data, and sets
# local.node_public_ipv4. Nothing else in this root references hcloud.

terraform {
  required_providers {
    hcloud = {
      source  = "hetznercloud/hcloud"
      version = "~> 1.69"
    }
  }
}

variable "hcloud_token" {
  description = "Hetzner Cloud API token with read and write permission for the project."
  type        = string
  sensitive   = true
}

provider "hcloud" {
  token = var.hcloud_token
}

resource "hcloud_ssh_key" "admin" {
  name       = "${var.server_name}-admin"
  public_key = var.ssh_public_key
}

resource "hcloud_firewall" "node" {
  name = "${var.server_name}-firewall"

  # No outbound rules are declared, which leaves all outbound traffic open.

  rule {
    description = "ICMP"
    direction   = "in"
    protocol    = "icmp"
    source_ips  = local.anywhere
  }

  rule {
    description = "SSH"
    direction   = "in"
    protocol    = "tcp"
    port        = "22"
    source_ips  = local.anywhere
  }

  rule {
    description = "HTTP"
    direction   = "in"
    protocol    = "tcp"
    port        = "80"
    source_ips  = local.anywhere
  }

  rule {
    description = "HTTPS"
    direction   = "in"
    protocol    = "tcp"
    port        = "443"
    source_ips  = local.anywhere
  }
}

resource "hcloud_server" "node" {
  name        = var.server_name
  server_type = var.server_type
  location    = var.location
  image       = var.image

  ssh_keys     = [hcloud_ssh_key.admin.id]
  firewall_ids = [hcloud_firewall.node.id]

  user_data = local.user_data

  public_net {
    ipv4_enabled = true
    ipv6_enabled = true
  }

  labels = {
    platform = "iidp"
  }

  # A changed user_data would replace the server, and replacing the server
  # destroys every local volume (each Application's Postgres data lives on
  # this disk) and the age key. Version bumps are therefore applied in place
  # by re-running iidp-bootstrap on the node (see infra/README.md); the
  # variables stay the record of what should be running. A deliberate
  # rebuild is `tofu apply -replace=hcloud_server.node`.
  lifecycle {
    ignore_changes = [user_data]
  }
}

locals {
  anywhere         = ["0.0.0.0/0", "::/0"]
  node_public_ipv4 = hcloud_server.node.ipv4_address
}
