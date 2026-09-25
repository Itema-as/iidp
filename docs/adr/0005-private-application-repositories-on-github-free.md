---
status: accepted
date: 2026-09-24
---

# Private Application repositories on GitHub Free: a Deploy gate on the Platform, private GHCR, org-only repositories

Itema's GitHub organisation stays on the Free plan, and Application repositories will be private. Phase 1 assumed each Application repository's deploy workflow could read the `iidp-deploy` GitHub App's private key from an organisation Actions secret, and on Free those don't reach private repositories. The acceptance run (#19) only got through because its Application was public. That assumption also hid a worse problem: every Application repository held a key that could write the whole Platform repository, and ArgoCD applies whatever is there, including `bootstrap/`. Anyone able to change any Application's workflow effectively had cluster admin.

We decided that an Application repository's CI can do exactly one thing to the Platform: deploy or promote **its own** Environments. It does that through a **Deploy gate**, a small service in this repository that runs on the Platform and is the only holder of the App key. Images live in **private GHCR**. The node pulls them with one read-only credential, and only repositories in `Itema-as` can be Applications.

## Decision

- **Deploy gate.** A Go service built from this repository, installed by the bootstrap chart and served behind Traefik at `deploy.<baseDomain>` under the existing wildcard certificate. CI calls it with the GitHub Actions OIDC token (`id-token: write`), so no secret is stored in any Application repository. The gate checks, in order:
  1. The token is signed by `https://token.actions.githubusercontent.com` for the gate's own audience.
  2. `repository_owner_id` is `Itema-as`, and `repository_id` is the id recorded for that Application.
  3. The ref is allowed for the Environment: `refs/heads/main` deploys to `staging`, or to `prod` when there is no `staging`; `refs/tags/v*` promotes to `prod`. Everything else is refused.
  4. The image tag exists in GHCR.

  Only then does it commit the one-line change to the Platform repository. The author is the OIDC `actor`, with GitHub's noreply address for that account; the committer is the App. `iidp ci set-image` becomes an HTTP client of the gate.
- **Repositories are bound by numeric id, not by name.** `iidp app create` and Adopt record the Application repository's `repository_id` (and the org's id) in the Platform repository. A rename keeps working. A repository deleted and recreated under the same name cannot deploy.
- **Only `Itema-as` repositories.** `--owner user` is removed, and Adopt refuses a repository outside the org ("transfer it to Itema-as first").
- **Private GHCR for images.** CI pushes and promotes with the workflow's built-in `GITHUB_TOKEN`, which works in private repositories on Free. GitHub states that container storage and bandwidth are currently free, private images included, and that it will give at least a month's notice before any change. ghcr.io accepts only a classic personal access token for pulls from outside Actions, not GitHub App tokens. So the node pulls with one `read:packages` token in k3s's `registries.yaml`, written by cloud-init from an OpenTofu variable the bootstrap wizard asks for. Rotating it is a documented ssh step and a k3s restart. It starts as the Platform admin's own token and moves to a machine user (#56).
- **The App key leaves CI.** When the gate ships, the organisation secret `IIDP_DEPLOY_APP_PRIVATE_KEY` and variable `IIDP_DEPLOY_APP_ID` are deleted, and the App's private key is rotated. Until then both are restricted to the one repository that uses them.

## Considered options

- **Upgrading to GitHub Team,** which makes organisation secrets reach private repositories. Rejected by the organisation. It would also have kept the cluster-admin-equivalent key in every Application repository.
- **The admin setting a repository-level secret on each new Application.** Repository secrets do work on Free. Rejected: a human step per Application breaks the self-service promise, and the key would still sit in every repository.
- **An off-the-shelf OIDC-to-GitHub-token exchange** (Chainguard's octo-sts, self-hostable). Rejected: the tokens it mints cover the whole Platform repository, so a compromised Application could still rewrite `bootstrap/`. The gate makes the one allowed change itself instead of handing out a token.
- **Pull-based deploys** (ArgoCD Image Updater watching the registry and committing tags). CI would need no write access at all. Rejected: another component on the node (upstream requests 512Mi), a polling delay, upstream's own "not for critical production" caveat, and multi-source write-back with an OCI chart plus `$values` that upstream doesn't document.
- **A self-hosted registry** (zot backed by Hetzner Object Storage). zot verifies GitHub Actions OIDC tokens natively, so CI could push with no secret. Not chosen yet: GHCR costs nothing today, and zot would be one more component on a 4 GB node. **Triggers to revisit:** GitHub announcing charges for private container storage or bandwidth, or the organisation restricting classic personal access tokens again (see the #75 note below). zot on Object Storage is the planned move then.
- **Public images from private repositories.** Rejected: an image carries the compiled server code, often source maps, and whatever was baked in at build time, so it is as confidential as the source.

## Consequences

- Private repositories share the organisation's 2,000 Actions minutes a month, and builds stop at the limit without a payment method. That's accepted and monitored. A self-hosted runner on the node was ruled out, because it would run every Application's CI next to the App key.
- Every organisation member has write access to every repository (the org's default permission), so any member can deploy any Application by pushing to its `main`. The gate limits *what* a deploy can change, not *who* can push. That lever is the organisation's permissions or branch protection, not the Platform.
- GitHub Environments don't exist in private repositories on Free, so the gate never relies on the OIDC `environment` claim.
- ADR-0002 still holds. The CLI never talks to Kubernetes, and developers still change the Platform repository only through the CLI. `ci set-image` now asks the gate to write rather than writing git itself.
- `docs/design.md`'s Delivery and Access rows, and `docs/implementation-notes/12-deploy-workflow.md`'s credential sections, describe the Phase 1 mechanism this replaces.

## Note (#66): the gate also sets the caller's own migration command

A deploy may now carry the migration command from the Application repository's `iidp.yaml`, and the gate writes it into `postgres.migrationCommand` in the same commit as the tag, so the command always matches the image it runs in. It is set only for the Environment the same checks let the caller deploy, only where Postgres is enabled, and only as one line of at most 1024 bytes. This does not widen what CI can do. CI already controls the whole image, and the image already runs with `DATABASE_URL` and the Application's secrets, so any command it could put in `migrationCommand` it could already run from the image's own entrypoint. The migration Job runs from that same image, in the Environment's own namespace, with the same environment the Application's containers get, and nothing more. See `docs/implementation-notes/66-migration-command-in-repo.md`.

## Note (#75): private GHCR depends on the organisation allowing classic tokens

ghcr.io takes only a classic personal access token for pulls from outside Actions. Both the node's `registries.yaml` token and the Deploy gate's copy (`argocd/ghcr-pull-token`) are classic tokens. `Itema-as` had "Restrict access via personal access tokens (classic)" turned on, so during the cutover (#62) both got 403 on the first private image. The admin allowed classic tokens in the organisation's settings (Settings > Personal access tokens). Every private image pull on the Platform now depends on that setting. Tightening it again breaks every pull at once: no new Pod can start, and the gate refuses every deploy with "couldn't check". If the organisation wants classic tokens restricted, move to zot on Object Storage first. zot takes the Actions OIDC token for pushes, and the node can pull from it without any personal token, which also makes the machine user of #56 unnecessary.
