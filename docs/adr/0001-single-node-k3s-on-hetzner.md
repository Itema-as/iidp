---
status: accepted
date: 2026-09-20
---

# Single-node k3s on Hetzner instead of managed Kubernetes

The Platform must cost under roughly €50 a month, because that is what the first Application to move onto it costs to run today. Managed Kubernetes on Azure puts a single 8 GB node alone above that ceiling, and Hetzner offers no managed control plane at all. We run k3s on one Hetzner CPX22 (2 vCPU, 4 GB, Helsinki) at about €29 a month, created by OpenTofu with cloud-init, with no Hetzner load balancer: Traefik binds the node's public IP directly.

## Considered options

- **AKS on Azure.** Itema has an Entra tenant, so it was the natural fit. Rejected on price: sub-€50 buys one small node and no load balancer.
- **Three small Hetzner nodes for high availability.** Rejected: at this budget the nodes would hold Platform components and little else. A single node with room for Applications beats three nodes with room for none.
- **A third-party managed control plane on Hetzner (Cloudfleet, ~€12/month).** Rejected for now: on a single node the node is the single point of failure, so a resilient control plane protects nothing.
- **No Kubernetes (Docker Compose, Coolify).** Rejected: the target stack (ArgoCD, CloudNativePG, cert-manager, external-dns) is Kubernetes-native and Itema wants a platform it can grow.

## Consequences

- A node reboot or failure takes every Application down. Accepted while Applications are internal; revisit when a customer-facing Application moves in.
- Capacity is roughly four small Applications with databases. Hetzner rescales CPU and RAM in place, so the first step up is CPX32 (8 GB, ~€53) via one OpenTofu change and a reboot.
- k3s upgrades are a manual version bump, not automated, because an unattended upgrade on one node is an unattended outage.
- Nothing on the node's disk is durable. Database backups go to Hetzner Object Storage continuously; that is the only recovery path.
- Moving host later means swapping the OpenTofu provider block. ArgoCD and the Platform repository do not care who runs the node or the control plane.
