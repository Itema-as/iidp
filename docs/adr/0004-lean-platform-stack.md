---
status: accepted
date: 2026-09-20
---

# A lean Platform stack: observability off-cluster, most of the recommended components rejected

The original component list, recommended by colleagues with IDP experience, was Crossplane, Kyverno, ArgoCD, Grafana, Istio, Prometheus, Kafka, Keycloak, Harbor, Nexus, OpenSearch, OpenTelemetry and Vault. On a 4 GB node that list does not fit, and most of it solves problems Itema does not have yet. Phase 1 runs on the node: ArgoCD, Traefik (from k3s), cert-manager, external-dns, CloudNativePG, Grafana Alloy, SOPS via KSOPS, and oauth2-proxy. Off the node: Grafana Cloud's free tier for logs and metrics, GHCR for images, Cloudflare for DNS.

## Per component

| Component | Decision | Reason |
|---|---|---|
| ArgoCD | Kept | The reconciler. Everything else hangs off it. |
| Prometheus, Grafana | Replaced by Grafana Cloud free tier plus Alloy on the node | kube-prometheus-stack needs ~1.5 GB, more than a third of the node. Free tier covers 10k series and 50 GB logs with 14-day retention. Swap the Alloy endpoint for in-cluster Loki and VictoriaMetrics if observability must come in-house. |
| Kyverno | Deferred to Phase 2 | Guardrails matter once people other than the Platform admin provision Applications. |
| Crossplane | Deferred | See ADR-0003. |
| OpenTelemetry | Deferred to Phase 2 | Traces via Alloy and Grafana Cloud Tempo once an Application needs them. |
| Istio | Rejected | A service mesh for a handful of Applications on one node is overhead with no consumer. Traefik and cert-manager cover ingress and TLS. |
| Kafka | Rejected | No Platform need. Becomes an Application Capability if a concrete Application needs event streaming. |
| Keycloak | Rejected | Itema is on Entra ID. ArgoCD uses it via Dex; the "Itema login" Capability uses one oauth2-proxy with a cookie for `.app.itma.no`, so no per-Application registration. Keycloak only returns if Applications need a self-hosted identity provider for their own end users. |
| Harbor | Rejected | GHCR is free for the org's private images. Harbor is six or seven pods. |
| Nexus | Rejected | GitHub Packages if Itema ever publishes private libraries. |
| OpenSearch | Rejected | Logs go to Grafana Cloud Loki. OpenSearch wants 2 GB of JVM heap and an operator. |
| Vault | Rejected | SOPS with age: the private key lives only in the cluster, the CLI encrypts with the public key, secrets sit in the Platform repository. No unsealing, no backup, no runtime cost. External Secrets Operator with a cloud key vault is the upgrade path. |
| Supabase | Out of scope for Phase 1 | Self-hosted Supabase is a dozen containers and 4-6 GB per instance, single-project. The Lovable Application that motivated it stays where it is until a node that carries Supabase fits the budget. Plain Postgres via CloudNativePG is the default database Capability. |

## Consequences

- Observability data leaves Hetzner and depends on a free tier that Grafana Labs can change. Entra SSO for Grafana is not on the free tier; developers use Grafana accounts there.
- The Platform's on-node footprint is roughly 1.5-2 GB, leaving about half of a CPX22 for Applications.
- Every rejected component has a named trigger for reconsideration above, so the list is not reopened without a new fact.
