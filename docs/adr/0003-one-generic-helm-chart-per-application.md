---
status: accepted
date: 2026-09-20
---

# One generic Helm chart defines every Application

Every Application, regardless of Kind, is an instance of a single Helm chart (`chart/application`) that lives in this repository and is published to GHCR as an OCI artifact on each release. The Platform repository holds, per Environment, one ArgoCD Application pointing at a pinned chart version and one values file. The values file is the Application's whole definition: Kind, Capabilities, size, image tag, domains, migration command.

## Considered options

- **Crossplane compositions with an `Application` custom resource.** The recommended IDP pattern, and the reason Crossplane was on the original list. Rejected for now: Crossplane costs about 1 GB of RAM idle, there are no cloud resources to compose on a single Hetzner node, and the chart expresses the same abstraction. Revisit if developers should define their own CRDs or if the Platform gains managed cloud resources.
- **Kustomize bases and overlays.** Rejected: more files per Application and no versioning of the Platform's conventions.
- **Fully rendered manifests written by the CLI.** Maximum transparency, rejected because every Platform convention change would mean the CLI rewriting every Application's files.

## Consequences

- Platform conventions (probes, resource sizes, labels, ingress shape, the migration Job that runs as a sync hook before the rollout, the login middleware) live in the chart, not in the CLI. Changing them is a chart release and a version bump in the Platform repository, never a CLI release.
- Upgrading every Application is bumping one chart version.
- The chart is the Platform's contract with Applications and must be tested as such: `helm template` plus kubeconform on every change, a kind-based end-to-end when `chart/` or `bootstrap/` change.
- Static sites are not special: CI builds them into an nginx image and the chart's Static site Kind sets nginx defaults. One pipeline, one chart.
