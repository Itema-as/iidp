# #75 Friction from the Deploy gate cutover

Decisions taken while fixing the eight things the cutover (#62) needed a workaround or luck for.

## 1. Rotating the App key patches only the Secret

`infra/README.md`'s rotation recipe used to re-run `iidp-bootstrap` with the new key. That script also re-rendered the stock argo-cd chart and applied it with `--server-side --force-conflicts`. Once the bootstrap's `argocd` Application manages ArgoCD, that apply resets the Platform's customisations (the Dex/Entra login, the KSOPS repo server) until ArgoCD self-heals. The recipe now does what #62 did: it pipes a merge patch of `data.githubAppPrivateKey` into `kubectl -n argocd patch secret platform-repo-github-app --type merge --patch-file /dev/stdin` over ssh, built on the admin's machine, so the key never touches the node's disk or a command line there. The proof steps are the ones #62 used:

- a deploy through the gate, before and after the old key is deleted;
- after the deletion, a repo-server restart and a hard refresh of `platform` and `platform-secrets`, which must come back Synced and Healthy.

The restart is there because the repo server can keep working from a token it fetched earlier. Without it, ArgoCD could look fine on the deleted key for a while.

**`iidp-bootstrap` no longer re-installs ArgoCD once ArgoCD manages itself.** The script checks for the `argocd` Application (`kubectl -n argocd get applications.argoproj.io argocd`) and skips the chart install when it exists. After the takeover, ArgoCD's version comes from `bootstrap/versions.yaml` through that Application, so the infra variable `argocd_chart_version` only matters for a first boot. The version-bump recipe says so and asks to keep the two equal, so a rebuilt node starts on the same version. The check only reaches new nodes: `tofu apply` never re-runs cloud-init (`ignore_changes = [user_data]`), so the running node keeps the old script until it's rebuilt. That's why the rotation recipe no longer uses the script at all, and the docs warn that an old script still re-applies the chart. New tests: `bash -n` on the whole script as cloud-init writes it (nothing checked its syntax before), and the chart install sitting in the `else` branch of the check.

## 2. Pods drain before they stop

A terminating Pod leaves its endpoints at the same moment its container gets SIGTERM, and Traefik takes a moment to notice, so every rollout answered 502 for a few seconds. The chart's Deployment now has a 5-second `preStop` sleep. It uses Kubernetes' native sleep action (`lifecycle.preStop.sleep`, on by default since Kubernetes 1.30; the Platform and kind run 1.36, and kubeconform validates it against the 1.36 schema), not `sleep` in the image, so images without a shell work too. `terminationGracePeriodSeconds` is 35, because the preStop hook counts against it and the container should still get the default 30 seconds after the sleep. `chart/application` tests both. The kind e2e restarts `shop-prod`'s Deployment while polling it through Traefik every 50 ms, and fails on anything but 200, until 45 seconds after the rollout finishes (`Cluster.CheckRolloutServes`).

## 3. Environment Applications retry against the newest commit

The Environment Applications the CLI writes had no `retry` block, so ArgoCD's default applied, which on `hello-prod` meant five retries of the commit the failed sync started on. A failing migration kept retrying the old commit there while the fix was already pushed. They now get the same policy as the Platform's own Applications (`bootstrap/templates/_helpers.tpl`): `limit: -1`, `refresh: true`, backoff from 10 s doubling to 3 minutes.

- **Unlimited, not five:** the first sync of a new Environment can fail for reasons that pass on their own (the database isn't ready, the registry hiccups), and those should converge without a human.
- **The price:** a migration that keeps failing re-runs every 3 minutes until it is fixed. Each run fails fast, and ArgoCD shows it.
- **`refresh`:** each retry uses the newest commit, so the fix is what finally syncs.

Everything that writes `application.yaml` goes through `render.ArgoCDApplication`: `app create` on both paths, and `add-capability --staging`. `internal/cli`'s create test checks the fields through the CLI seam. Existing Environments keep their files: `hello-prod`, `hello-staging` and `iprofil-prod` need the `retry` block added by hand in the Platform repository.

## 4. The Next.js runtime user has a writable home

`adduser --system` gives a user the home `/nonexistent`, so npm couldn't write its cache or logs, and `npx`, which Prisma and similar tools use for migrations, would fail. The template now creates `nextjs` with `--home /home/nextjs` (Debian's `adduser` creates it, owned by the user) and sets `HOME` to it. The Vite template's runtime is nginx and has no npm, and the "Other" template is a stub, so neither needs the same.

The check that `npx --version` and `npm cache verify` work in the built image is a step in the CI "Templates" job. That is a workflow file, so it's delivered as a patch (below) and runs once pushed.

## 5. ADR-0005: private GHCR depends on the organisation allowing classic tokens

Recorded in ADR-0005 (a note, plus the zot trigger) and in `infra/README.md`'s prerequisites: ghcr.io takes only classic tokens for pulls from outside Actions, so every private pull depends on the organisation setting that allows them. Restricting classic tokens again is now a trigger to move to zot, which also makes #56 unnecessary.

## 6. The pull-token check must use a private image

A public image pulls with no credential, so `crictl pull` on one proves nothing. It passed during #62 while the token was unusable. Step 6 of "Adding or rotating the GHCR pull token" now requires a private image, and first tests the token itself from the admin's machine: ghcr.io's token endpoint with the classic token as basic auth, then a manifest request with the bearer token. That's the same flow the gate uses (`internal/registry`), and it answers 200 for a token that can read the image. The recipe was checked anonymously against the public `charts/application` package (200), and against a private image with no credentials (401).

## 7. `brew update` before installing or upgrading

Homebrew refreshes a tap at most once a day on its own, so the tap had 0.2.0 while `brew upgrade` still installed 0.1.1. The README's install and upgrade commands now run `brew update` first.

## 8. The release's chart push retries once

The `v0.2.0` release's chart job failed with `failed to perform "Tag" on destination: sha256:…: not found` right after a successful upload, and a re-run passed. The push is now tried a second time after 15 seconds. `helm push` of the same version is idempotent, so retrying after a partial success is safe.

## Delivered as a patch

Items 4 (the CI check) and 8 change files under `.github/workflows/`, which the agent's token can't push. The patch is in the pull request's description, to be pushed over SSH as for #68. Until it is pushed, item 4's check hasn't run in CI. The template image still builds and serves `/` in the existing Templates job, which proves the `adduser` line works, but not `npx`.
