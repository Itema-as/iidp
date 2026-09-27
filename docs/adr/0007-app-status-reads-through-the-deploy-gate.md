---
status: accepted
date: 2026-09-26
---

# `iidp app status` reads cluster state through the Deploy gate's service

`iidp app status` shows, per Environment (Preview Environments included): ArgoCD sync and health, the running image tag and when it was deployed, pods ready and their restarts, the last migration and Scheduled task runs, and the addresses. It has `--json` for scripts. It shows no logs; those stay in Grafana Cloud, and the output links there.

The CLI gets this from a read endpoint on the Deploy gate's service. The CLI sends the developer's own `gh auth` token. The service asks GitHub whether that user can read the Application's bound repository (ADR-0005's binding by numeric id), and only then reads ArgoCD and Kubernetes inside the cluster and answers. This amends ADR-0002's expectation that status would come from "reading the ArgoCD API": the CLI still never talks to Kubernetes, and `gh auth` stays its only credential.

## Considered options

- **The ArgoCD API with SSO**, as ADR-0002 anticipated. The CLI would log in to ArgoCD through Entra (PKCE, as the `argocd` CLI does). Rejected: a second login for every developer, and ArgoCD's API exposed to developer machines, for a read-only view.
- **GitHub only.** The latest gate commit per Environment, workflow runs and the image tag, with nothing new on the Platform. Rejected: it can't show whether the Environment is actually healthy, which is the question `status` exists to answer.
- **Status plus logs** (`iidp app logs`). Rejected for now: a second read path with more access to secure, when Grafana Cloud already has the logs.

## Consequences

- The Deploy gate's service is no longer only a write path for CI. It also answers developers, with a different credential (a GitHub user token instead of an Actions OIDC token). Its in-cluster access grows to read ArgoCD Applications, Pods, Jobs and CronJobs in Application namespaces. It stays read-only, and the name **Deploy gate** still means only the deploy and promote path (`CONTEXT.md`).
- Anyone who can read an Application repository can see that Application's status. Nobody else can, and the check follows GitHub's permissions, so it never drifts from them.
- Every status call costs one GitHub API call made with the developer's own token, against the developer's own rate limit.

## Note (#114, 2026-09-27): the gate's service is no longer read-only in the cluster

The Consequences above say the service's in-cluster access "stays read-only". That no longer holds. After a Deploy or Promote commit, the Deploy gate sets `argocd.argoproj.io/refresh: normal` on the Environment's ArgoCD Application, so ArgoCD looks up the new commit within seconds instead of at its next 3-minute poll. That refresh request is its one write in the cluster. It uses the same service account, through a separate Role in `argocd` that grants only `patch` on `applications.argoproj.io`. Everything the status endpoint reads is still `list`, and the status endpoint itself still writes nothing. The worst a misused grant can do is change an ArgoCD Application in `argocd`, which covers the Platform's own components as well as the Environments. The gate only ever sends the refresh annotation, and it already holds the App key that can rewrite the whole Platform repository those Applications come from, so the grant adds little to what a compromised gate could already do. A failed patch never fails the Deploy, since ArgoCD's poll still picks the commit up. A GitHub webhook to ArgoCD would get the same refresh, but it needs a public ArgoCD endpoint and a webhook secret. See `docs/implementation-notes/114-argocd-refresh.md`.
