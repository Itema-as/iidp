---
status: accepted
date: 2026-09-26
---

# Preview Environments come from an ArgoCD ApplicationSet, not the Deploy gate

A Preview Environment is an Environment for one open pull request labelled `preview` on an Application repository. An Application that has opted in (`--previews`, which requires a `staging` Environment) gets one ArgoCD ApplicationSet in the Platform repository. The ApplicationSet uses the Pull Request generator: ArgoCD polls GitHub every 3 minutes with the `iidp-deploy` App, creates an Environment for each open pull request carrying the label, and deletes it when the pull request closes or loses the label. CI only builds and pushes the pull request's image, tagged with its head SHA, and only for labelled pull requests. It calls nothing on the Platform.

A preview gets `<app>-pr-<n>.<baseDomain>`, the smallest size, staging's secrets and staging's current migration command, a fresh empty database with no backups, and always sits behind Itema login with the Application's sign-in groups. It runs no Scheduled tasks.

## Considered options

- **Through the Deploy gate.** CI would call the gate on pull request events. The gate would write and remove each preview's files in the Platform repository, enforce a cap of 3 per Application, and sweep previews whose pull request had closed. Everything would stay in git. Rejected: it widens the gate from "set my own Environment's image" to "create and delete Environments", which amends ADR-0005's central rule. It also needs a sweeper, because a missed close event would otherwise leave a preview running forever. ArgoCD's generator gives the same lifecycle with no new write path, and cleanup can't be missed.
- **A copy of staging's database.** Realistic data, but it is slower to start, reads more from Object Storage, and puts staging's data in every branch. Rejected in favour of an empty database, which the Application seeds itself if it wants data.
- **A GitHub webhook to ArgoCD** instead of polling. Near-instant, but it needs another org setting and another shared secret. Rejected for now; polling every 3 minutes costs a trivial share of the App's API rate limit.
- **The migration command read from the image.** It would always match the pull request, but it only works for images built to carry `iidp.yaml`, and Adopted Dockerfiles don't. Rejected for staging's command, with the limitation below.

## Consequences

- Previews are **not in the Platform repository's git log**. Their desired state is the ApplicationSet (which is in git) plus the set of open, labelled pull requests (which is in GitHub). This is the one exception to the Platform repository holding every Environment's desired state, and it is deliberate.
- There is **no hard cap** on the number of previews: the generator can't express one. The `preview` label is the limiter. Developers add it only to pull requests they want to see running. The node is CPX22, and the Platform admin watches memory by hand.
- The `iidp-deploy` App needs **Pull requests: read** on Application repositories. An org owner approves the new permission once.
- A pull request that changes the migration command in `iidp.yaml` previews with staging's command until it merges. The migrations themselves are the pull request's own, since they ship in its image.
- Previews run unmerged code with staging's secrets. That grants nothing new: the pull request's authors are org members who can already push to `main` and so deploy to staging.
