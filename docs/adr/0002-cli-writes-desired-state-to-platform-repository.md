---
status: accepted
date: 2026-09-20
---

# The CLI writes desired state to a Platform repository and never talks to the cluster

`iidp` provisions an Application by committing files to the Platform repository (`Itema-as/iidp-platform`). ArgoCD reconciles the cluster from that repository. The CLI holds no kubeconfig and makes no Kubernetes API call. The only direct calls it makes are to GitHub, for things that cannot be a file in git: creating an Application repository, opening a pull request on an adopted one, and installing the deploy workflow.

## Considered options

- **The CLI talks to the Kubernetes API directly.** Faster feedback, but every developer needs cluster credentials and nothing records what was done. Rejected.
- **Pure GitOps with no direct calls at all.** Rejected: the Application repository and its CI wiring have to be created by something, and that something is the GitHub API.

## Consequences

- Authentication to the Platform is `gh auth`. Write access to the Platform repository is the authorisation model. Commits go straight to `main` while the team is small; branch protection with required review is the upgrade path.
- The git log of the Platform repository is the audit trail for every Application change.
- A deploy is a commit: CI writes the new image tag into the Environment's values file. Promotion to `prod` is the same commit with a version tag instead of a SHA. No image is ever rebuilt for promotion.
- The CLI cannot show live status or logs in Phase 1. Developers use ArgoCD (Entra SSO) and Grafana Cloud for that. An `iidp app status` reading the ArgoCD API is a later addition.
- The Platform repository is machine-written. Hand edits are allowed but unsupported, and the CLI must tolerate them.
