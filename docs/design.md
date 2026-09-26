# iidp design

The shared understanding reached in the design session on 2026-09-20. Vocabulary is defined in [`CONTEXT.md`](../CONTEXT.md); the hard-to-reverse decisions and their rejected alternatives are in [`docs/adr/`](./adr/). This file records everything else that was agreed, so it is not re-decided by accident.

## Summary

| Area | Decision |
|---|---|
| Hosting | One Hetzner CPX22 in Helsinki, k3s on Ubuntu 24.04 via OpenTofu and cloud-init, state in Hetzner Object Storage, manual k3s version bumps |
| Cost | ~€29 node + ~€7 Object Storage, no load balancer |
| On-node Platform | ArgoCD, Traefik, cert-manager, external-dns, CloudNativePG, Grafana Alloy, SOPS via KSOPS, oauth2-proxy |
| Off-node | Grafana Cloud free tier for logs and metrics, GHCR for images, Cloudflare DNS for `itma.no` |
| Rejected | Istio, Kafka, Keycloak, Harbor, Nexus, OpenSearch, Vault, Supabase in Phase 1; Crossplane deferred, Kyverno replaced by built-in admission in Phase 2 (see ADR-0004) |
| Repositories | `iidp` holds CLI, chart, infra, bootstrap, docs. `iidp-platform` holds one ArgoCD Application and values file per Environment, plus `platform.yaml`. Org `Itema-as` |
| Application model | Kind: Static site or Web service. Capabilities: Postgres, staging, custom domain, Itema login. Sizes small/medium/large |
| Addresses | `<app>.app.itma.no`, `<app>-staging.app.itma.no`, one wildcard cert, custom domains in the Platform's Cloudflare zone (`cloudflareZone`, `itma.no`) automatic, anything else by CNAME, other Cloudflare zones included |
| Delivery | CI builds SHA-tagged images to GHCR. `main` deploys to staging or prod, a `v*` tag retags and promotes to prod. Write-back via the Deploy gate, authenticated by GitHub Actions OIDC; images in private GHCR (ADR-0005, which replaced the org GitHub App secret) |
| Database | One single-instance CNPG cluster per Environment, `DATABASE_URL` injected, optional migration Job run as an ArgoCD sync hook before the rollout (see notes for #7 on why not PreSync), continuous backups to Object Storage, final backup kept 30 days on delete |
| Secrets | CLI generates and SOPS-encrypts, private age key only in the cluster, `iidp secret set` for developer-supplied values |
| Access | `gh auth` for the CLI, direct commits to `main` of the Platform repository, Entra SSO for ArgoCD, Grafana account for Grafana Cloud, kubeconfig only for the Platform admin |
| CLI | Go, `iidp app create` wizard with flags for non-interactive use, Create and Adopt paths, Adopt opens a PR, framework Dockerfiles for Next.js and Vite only |
| Testing | Unit tests, `helm template` and kubeconform on every PR, kind end-to-end on chart or bootstrap changes and on tags |
| Done means | A fresh Next.js app with Postgres created end-to-end by the wizard, then an Adopt of a real Itema repository |
| Build order | Infra and bootstrap, chart, CLI Create path, Adopt path, then Itema login |
| Phase 2 | Designed 2026-09-26 (spec issue, ADR-0006, ADR-0007): Scheduled tasks (a Capability declared in `iidp.yaml`, replacing the Cron job Kind), guardrails with Pod Security and ValidatingAdmissionPolicy (replacing Kyverno), `iidp app status` through the Deploy gate's service, sign-in groups for Itema login (replacing per-app Entra registrations), Preview Environments from an ArgoCD ApplicationSet. Deferred: OpenTelemetry traces (`phase-3`), the CPX32 resize (manual, when memory runs out) |

## The wizard

`iidp app create` asks, in order, with defaults in brackets:

1. Application name. Lowercase, DNS-safe, unique on the Platform.
2. Create or Adopt? Adopt asks for the repository URL.
3. Kind: Static site or Web service. On Create, framework: Next.js, Vite React, or Other. Skipped when the repository already has a Dockerfile; an existing Dockerfile is never touched and is what gets deployed. Other scaffolds a commented Dockerfile stub that must be completed before the first deploy succeeds.
4. Postgres database? [no]. If yes: migration command [detected or none]. The CLI looks for Prisma (`prisma/schema.prisma`), Drizzle (`drizzle.config.*`) or a `migrate` script in `package.json` and proposes the matching command. Help text:
   ```
   Migration command (optional)
   Runs once before every rollout, in a one-off container built from your
   image, with DATABASE_URL set. Leave empty if your app has no migrations.
   Detected: prisma/schema.prisma → suggested "npx prisma migrate deploy"
   ```
5. Staging Environment? [no]
6. Custom domain? [none]. If the domain is in the Platform's Cloudflare zone (`platform.yaml`'s `cloudflareZone`), everything is automatic. Otherwise, including a domain on another Cloudflare zone, the closing summary prints the CNAME to create.
7. Itema login? [no]. Only available on Platform addresses, not custom domains.
8. Size: small / medium / large [small]
9. Summary screen, confirm.

Every question has a flag, so `iidp app create --name x --kind web-service --postgres` runs non-interactively.

It then creates the Application repository (Create) or opens a PR adding the Dockerfile and deploy workflow (Adopt), writes the Environment files to the Platform repository, and prints the addresses, any CNAME to add, and links to ArgoCD and Grafana Cloud.

Other commands planned: `iidp app add-capability`, `iidp secret set`, `iidp app delete`. Delete takes a final Postgres backup, keeps it 30 days, requires typing the Application name, and never touches the Application repository.

## Conventions the chart encodes

- A Web service container listens on `PORT` (default 3000). The readiness probe hits `/` unless the values file overrides it.
- Sizes: small 250m CPU / 256 MiB, medium 500m / 512 MiB, large 1 CPU / 1 GiB. Postgres is a fixed 256 MiB single instance. The numbers live in the chart, not the CLI.
- Static sites are built into an nginx image in CI and deployed like a Web service.
- Each Environment has its own database and its own address.

## Where the CLI learns about the Platform

The org and Platform repository name are baked into the binary. Everything else (base domain, chart version, Object Storage bucket) is read from `platform.yaml` at the root of the Platform repository, so changing it is a commit rather than a release.

## Repository layout

```
cmd/iidp/            CLI entrypoint
internal/            wizard, github, platformrepo, render
chart/application/   the generic Helm chart
infra/               OpenTofu for the node, cloud-init for k3s
bootstrap/           ArgoCD app-of-apps for Platform components
scripts/bootstrap-wizard.sh
docs/adr/
docs/design.md
CONTEXT.md
```

Released with GoReleaser: binaries on GitHub Releases, a Homebrew tap under `Itema-as`, the chart pushed to GHCR as OCI on the same tag.

## One-time bootstrap

`scripts/bootstrap-wizard.sh` walks the Platform admin through the steps only a human can do: Hetzner project and token, Cloudflare token, the GitHub App, the Grafana Cloud stack. The Entra app registration for ArgoCD and oauth2-proxy is created with the Azure CLI when the admin is logged in with rights to register applications, and asked for otherwise. OpenTofu takes over from there, and ArgoCD installs the rest of the Platform from `iidp-platform`.

## Hetzner prices observed

Console hourly rates on 2026-09-20, Helsinki, monthly cap is roughly the hourly rate times 730:

| Plan | vCPU / RAM | Hourly | ≈ Monthly |
|---|---|---|---|
| CPX12 | 1 / 2 GB | €0.0240 | €17.5 |
| CPX22 | 2 / 4 GB | €0.0400 | €29 |
| CPX32 | 4 / 8 GB | €0.0721 | €53 |
| CPX42 | 8 / 16 GB | €0.1402 | €102 |
