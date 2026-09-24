# #60 Deploy and promote through the Deploy gate

This note records the decisions taken while building the Deploy gate, [ADR-0005](../adr/0005-private-application-repositories-on-github-free.md)'s answer to private Application repositories on GitHub Free. For each question it gives the options considered and the answer chosen. The gate's contract (what a call carries, what is checked, what is committed) is in [`docs/platform-repository.md`](../platform-repository.md#how-a-deploy-reaches-the-platform-repository-the-deploy-gate). How the bootstrap installs it is in [`bootstrap/README.md`](../../bootstrap/README.md#the-deploy-gate). GitHub's OIDC facts (issuer, key set, claims, the audience parameter, and the ids arriving as strings) were checked against GitHub's documentation on 2026-09-24. GoReleaser's `dockers_v2` was checked with Context7 the same day.

## A binary of its own, not an `iidp` subcommand

The gate is `cmd/iidp-deploy-gate`, with its logic in `internal/deploygate` and token verification in `internal/oidc`. An `iidp deploy-gate serve` subcommand would have let the image reuse the CLI's own build. But it would have put an HTTP server holding a GitHub App key into the tool every developer installs, behind a hidden command. The separate binary costs about fifteen lines of GoReleaser configuration: one more build, linux/amd64 only, never archived. It still reuses the CLI's code: `internal/githubapp.SignJWT` for the App JWT, `internal/github` for the installation token, and `platformrepo.Writer.SetImageTag` for the clone, edit, commit and push with retry. `SetImageTag` now takes an `ImageTagChange`, whose `Environment` callback runs on each fresh clone. The gate's binding and ref checks therefore run on the same clone the write is made from, and run again when a moved `main` forces a retry.

## The API: one call

`POST /v1/deploy` takes `Authorization: Bearer <OIDC token>` and a JSON body `{"application", "environment", "tag"}`. It answers 200 with `{"application", "environment", "tag", "file", "commit"}`, plus `"unchanged": true` when the Environment already ran the tag. Every refusal is `{"error": "<what was refused and why>"}` with a status that says whose fault it is:

| Status | When |
|---|---|
| 400 | A malformed request: an invalid Application name, an unknown Environment, or a tag that is not a valid image tag |
| 401 | No token, or one the gate cannot trust: a bad signature, an unknown key, the wrong issuer or audience, expired |
| 403 | A trusted token that may not do this: another org, a repository that is not the Application's, an unbound Application, a ref that may not deploy, or an Environment that ref may not deploy |
| 404 | The Application or the Environment does not exist |
| 503 | The gate cannot fetch the issuer's keys right now |

`GET /healthz` serves the probes. The tag must match the OCI tag grammar, so nothing but a tag can reach `values.yaml` or the commit message.

The spec left open what `auto` means on a tag. Before the gate, `ci set-image auto` always meant "staging if the Application has one", and the workflow passed `prod` explicitly on a tag. Now the ref decides. `main` targets staging, or prod when there is no staging. A `v*` tag targets prod. `auto` means that target. An explicit Environment must equal the target: it is refused with 403 when it exists but the ref may not deploy it, and with 404 when the Application does not have it. The generated workflow still passes `auto` on `main` and `prod` on a tag, so both old and new workflows mean the same thing.

## Where the App key comes from: ArgoCD's Secret, mounted in the `argocd` namespace

The key is already in the cluster. cloud-init writes `argocd/platform-repo-github-app` (keys `githubAppID`, `githubAppInstallationID`, `githubAppPrivateKey`, `url`, `type`) as ArgoCD's credential for the Platform repository ([41](41-argocd-platform-repo-credential.md)). Three options were considered.

- **Its own copy, written by cloud-init into a gate namespace.** Rejected. It means a second Secret to keep in step on every rotation, a cloud-init change, and an `iidp-bootstrap` re-run on the node before the gate can start.
- **Read the Secret across namespaces through the Kubernetes API**, with a Role in `argocd` limited to that one Secret by `resourceNames`. Rejected. It needs a service account token in the pod, RBAC to get right, and either client-go or a hand-written API client. All of that exists only to reach a Secret a volume can deliver.
- **Chosen: run the gate in `argocd` and mount the Secret as a volume**, listing only the three keys it needs. A pod can only mount a Secret of its own namespace, so the gate lives next to it. The pod has `automountServiceAccountToken: false`, and no Role grants it anything, so sharing the namespace gives it nothing beyond the files. The gate reads the files on every call, so a rotation (`infra/README.md`) reaches it once the kubelet refreshes the volume, with no restart. `infra/README.md`, cloud-init and `variables.tf` now say that the Secret's name and keys are a contract with the gate.

The installation id comes from the Secret, so the gate never lists installations. An installation token is minted per call. That costs one request and keeps no token in memory between deploys. `github.Client.ListInstallations` and `githubapp.AppIDFromEnv`/`PrivateKeyFromEnv` had no caller left, so they are gone.

## The org id is pinned in the gate's configuration

`deployGate.githubOrgId` in `bootstrap/values.yaml` defaults to `1230559`, Itema-as's id, and reaches the gate as `IIDP_GATE_ORG_ID`. The gate refuses to start without it. Both the token's `repository_owner_id` and the binding's `repositoryOwnerId` must equal it. Without the second check, a hand-edited binding could point at a repository in another org. Checking the token alone would already refuse such a caller, but checking both gives a clearer message: the binding is broken, not the caller. The token check runs before anything is cloned, so a caller from another org costs no GitHub request.

## How CI learns the gate's URL: rendered into the workflow

`ci set-image` cannot read the private Platform repository. Three options were considered.

- **An org Actions variable.** Rejected. It brings back the org-level plumbing ADR-0005 removes, and on Free an org variable does not reach private repositories either.
- **A constant compiled into the CLI.** Rejected. `baseDomain` is Platform configuration in `platform.yaml` and is not fixed at release.
- **Chosen: rendered into the workflow at `app create`.** Create and Adopt render `https://deploy.<baseDomain>` (`platformrepo.Config.DeployGateURL`) into `env.IIDP_DEPLOY_GATE_URL`. The base domain comes from the clone `Writer.CheckAvailable` already makes, which now returns the Config. Adopt now calls `CheckAvailable` too, so a taken name is refused before the pull request is opened rather than after. `ci set-image` reads `--gate-url` or `IIDP_DEPLOY_GATE_URL` and requests the token with exactly that URL as audience, without a trailing slash. The gate's audience is `https://deploy.<baseDomain>`, built by the bootstrap chart from the same `baseDomain`, so the two agree by construction. The gate also ignores a trailing slash on either side. If `baseDomain` ever changes, existing workflows keep the old URL, like every Application address that changes with it.

## Reserved names: `deploy` and `auth`

An Application called `deploy` would be served at `deploy.<baseDomain>`, the gate's own address. Its Ingress would share the host with the gate's, and it could receive the OIDC tokens other Applications' workflows mint for the gate. A token is only good for its own repository, but a replayed token could still deploy that repository's Application with any tag within the token's lifetime. `iidp app create` and the wizard therefore refuse the names `deploy` and `auth` (the Itema login's `auth.<baseDomain>`, the same problem found in passing), and `--domain` refuses both hosts (`platformrepo.ReservedNames`). An existing Application with either name is not touched: `ValidateName` is unchanged for every other command.

## Commit identity: the actor authors, the App commits

`git.Auth` gained `Committer`. `Identity` still sets author and committer together, and `Committer`, when set, overrides the committer alone. The author is the token's `actor`, `<actor_id>+<actor>@users.noreply.github.com`, the same address `ci set-image` used on the runner since #48. The committer is `<slug>[bot]` with `<bot user id>+<slug>[bot]@users.noreply.github.com`, the address GitHub links to an App's bot account. The gate reads the slug from `GET /app`, authenticated with the App JWT. It reads the bot's id from `GET /users/<slug>[bot]` with the installation token, because `/users` does not accept a JWT. It looks both up on the first deploy and caches them for the life of the process. The commit body names the calling repository and its id, the commit, the ref and the workflow, so the Platform repository's log answers "which run deployed this" without GitHub.

## Concurrency and repeats

The gate holds a mutex around the write. Concurrent deploys queue instead of pushing over each other, and the existing retry-once on a moved `main` covers the CLI's own writes landing in between. When the Environment already runs the tag, nothing is committed and the answer is 200 with `unchanged`. That makes a repeated call harmless, so `ci set-image` retries a gate it cannot reach, or one that answers 502, 503 or 504 (Traefik while the gate restarts), twice, ten seconds apart. Every other refusal is final and printed as the gate worded it.

## Token verification with the standard library

`internal/oidc` verifies RS256 against the issuer's key set. Adding a JOSE library was rejected for the same reason `githubapp` signs by hand: one algorithm, a handful of claims, and a repository that keeps its dependencies minimal. The key set is fetched on first use and cached for an hour. A token signed with an unknown `kid` refetches it, at most every thirty seconds, which covers GitHub rotating its keys. If the issuer is unreachable but the key is already known, the cached key keeps working. `exp`, `nbf` and `iat` get a minute of leeway. `aud` may be a string or a list. The id claims are kept as the decimal strings GitHub sends and compared with `strconv.FormatInt`, as [58](58-repository-binding.md) specified; a JSON number is accepted too. The `sub` claim is never read: repositories created after 2026-07-15 get a different format. Neither is `environment`, which private repositories on Free never have.

## The image: GoReleaser `dockers_v2`, inert until the workflow allows it

The release workflow runs GoReleaser with `contents: write` only, and this change cannot edit workflows, because the token that pushes it lacks the `workflow` scope. Two options were considered.

- **`kos`** builds without Docker. But the gate shells out to `git`, so it would need a base image that carries git, and the kind harness would then have to build a different way from the release.
- **Chosen: `dockers_v2`.** It uses `cmd/iidp-deploy-gate/Dockerfile` (Alpine with `git` and `ca-certificates`, the binary copied from `$TARGETPLATFORM/`), the layout GoReleaser gives its build context. The harness cross-compiles into the same layout and builds the same file, so one Dockerfile serves both. It builds linux/amd64 only, since the node is a CPX22. One platform needs no QEMU for the `RUN apk add`, and SBOM and provenance are off, so one plain manifest is pushed.

The image is `ghcr.io/itema-as/iidp-deploy-gate:<version>`, the same version as the CLI archives. `disable` is `{{ ne (index .Env "IIDP_PUBLISH_DEPLOY_GATE") "true" }}`, so until the release workflow sets that variable, logs in to GHCR and has `packages: write`, GoReleaser skips the image instead of failing the release. `goreleaser check` and a snapshot release pass without it. The workflow change is a separate patch, listed in the pull request. It also has the e2e workflow lint the new component chart, and run the e2e when the gate's source changes.

The bootstrap chart does not pin the image tag separately. `components/deploy-gate` derives it from the bootstrap revision `platform-components.yaml` pins (`v1.2.3` gives `1.2.3`), so bumping the bootstrap bumps the gate. A revision that is not a `v*` tag names no published image. The component then fails to render with a message saying so. Only the `deploy-gate` Application shows the error, because the derivation happens in the component chart, not in the app-of-apps, and the rest of the bootstrap is unaffected. `deployGate.image.tag` pins a tag explicitly, which is what the kind fixture does. The node pulls the image with the same `registries.yaml` credential it uses for private Application images (#59), so the package can stay private.

## Resources

The gate requests 5m of CPU and 32Mi of memory, with a 128Mi memory limit. fakegithub requests 5m and 16Mi. A deploy is one shallow clone of the Platform repository and one push. In the local kind run before the pull request, the node's CPU requests peaked at 1940m with every Platform component and both shop Environments up, 10m of it the gate and fakegithub. brochure-prod's first image (250m) is deployed only after shop-staging's deletion frees 500m, and the run ended at 1690m. It took 534 seconds on Podman.

## The bootstrap wizard no longer puts the key in CI

`stage_github_app` used to write the App's key into the org Actions secret `IIDP_DEPLOY_APP_PRIVATE_KEY` and its id into the variable `IIDP_DEPLOY_APP_ID`. A fresh Platform would have recreated exactly what ADR-0005 deletes, so both calls and their helpers are gone. The stage is renamed "GitHub App for the Deploy gate and ArgoCD". The wizard test now asserts that no org secret or variable is set.

## Tests

- **The gate** (`internal/deploygate/gate_test.go`) is tested through its HTTP boundary against a fake issuer serving its own key set, a fake GitHub that verifies the App JWT and serves the installation token, slug and bot id, and a local bare Platform repository. Every acceptance criterion's refusal is covered: a bad signature, an unknown key, the wrong audience or issuer, an expired token, another org, another repository id (and a recreated repository), an Application without a recorded id (absent, id missing, id quoted, not YAML, another org's id), disallowed refs, an Environment the ref may not deploy, missing Environments and Applications, and malformed requests. Each refusal asserts that nothing was committed. The successful cases assert the author and the committer, the body, the untouched rest of `values.yaml`, staging versus prod on `main`, a promote, the unchanged repeat, the retry on a moved `main`, and that the bot lookup is cached. This package has its own test file, against the "the seam is the CLI" convention, because the gate is not a CLI command and its HTTP boundary is its seam.
- **`ci set-image`** (`internal/cli/ci_set_image_test.go`) runs through `cli.RunWith` against a fake Actions token endpoint and a fake gate. It covers the audience it asks for, the bearer token and body it sends, the flag overriding the variable, printing a refusal, retrying 503 and 502 and giving up, an unchanged answer, clear errors without `id-token: write` and without the URL, argument refusals before any request, and that it runs with no `IIDP_DEPLOY_APP_*` at all.
- **The workflow template** test asserts `id-token: write` in both jobs, the gate's URL, no `vars.`, no `secrets.IIDP` and no `IIDP_DEPLOY_APP`. The Create and Adopt tests assert the URL rendered from `platform.yaml`.
- **The bootstrap** tests render the `deploy-gate` Application (the pin, the namespace, the org id, the values) and the component chart (the image tag from the pin, the environment, the Secret's three keys, no service account token, 5m of CPU, the Ingress host and its wildcard TLS). They also check that an explicit tag works and that a branch pin with no tag is refused.
- **The kind e2e** (`testDeployGate`) runs the gate from the bootstrap, built from the working tree, against fakegithub (`test/e2e/testdata/fakegithub`), which serves the key set the test signs with and the three GitHub calls. It calls the gate through Traefik at `deploy.app.example.test`, like CI. A call from another repository of the org and a call from `refs/heads/feature` are refused with 403. A `main` deploy of `brochure`, bound by the fixture's new `applications/brochure/repository.yaml`, lands as one commit authored by the actor and committed by the App's bot. `brochure-prod`, which rendered nothing before, then answers 200 through Traefik and is Healthy. `brochure`'s image repository in the fixture is now nginx, which shop has already pulled. The test runs last, after shop-staging's CPU is freed. fakegithub sits under `testdata` so that `./test/e2e/...` still matches a single package, and `go test -v` streams its output instead of buffering it until the end.

## What was not built

- **Checking that the image tag exists in GHCR** is #61.
- **Binding the tag to the token** (the tag equal to `sha` on `main`, or to the version on a `v*` tag) would stop a workflow deploying any tag. It is not in the ticket and would constrain #61's design, so it is left for then.
- **Restricting `event_name` to `push`.** Anyone who can trigger a `workflow_dispatch` or a schedule on `main` can already push to it. As ADR-0005 says, the gate limits what a deploy can change, not who can push.
- **More than one replica, rate limiting and a NetworkPolicy.** The node runs one of everything, and ArgoCD's own endpoints have none of these either.

## Moving existing Applications over (#62)

A deploy workflow pins the `iidp` release that generated it, so an existing Application keeps calling the old `ci set-image`, which writes with the org secret, until its workflow changes. Nothing breaks while both paths exist, and the order below keeps it that way. For the live Platform and `hello`:

1. **Release.** Land the workflow patch from this pull request, then tag a release. Check that `ghcr.io/itema-as/iidp-deploy-gate:<version>` exists.
2. **Add the GHCR pull token, before the bootstrap bump.** Follow `infra/README.md`, "Adding or rotating the GHCR pull token", all of it. Since #61 it also writes the gate's copy of the token, the Secret `argocd/ghcr-pull-token`, which the gate mounts to check that an image tag exists ([61](61-image-check.md)). The gate's pod does not start without that Secret, so doing this first means step 3 installs a gate that starts at once. The other order works too: the pod waits in `ContainerCreating` until the Secret exists, then starts on its own.
3. **Bump the bootstrap.** Set `targetRevision` in the Platform repository's `bootstrap/platform-components.yaml` to that tag. ArgoCD installs `deploy-gate` in `argocd`, external-dns creates `deploy.<baseDomain>`, and the wildcard covers it. Check that `curl https://deploy.<baseDomain>/healthz` answers `ok`, and read the pod's first log line for the audience, issuer and org id.
4. **Bind `hello`.** `hello` must be in `Itema-as`; transfer it first if it is not. Then run `iidp app bind hello --repo Itema-as/hello`, with the CLI from step 1 or any release since #58.
5. **Change `hello`'s workflow.** Regenerating it from the step 1 release is simplest. By hand, in `.github/workflows/deploy.yaml`:
   - Add `IIDP_DEPLOY_GATE_URL: https://deploy.<baseDomain>` under the top-level `env:`.
   - Add `id-token: write` to the `permissions:` of both the `build` and `promote` jobs.
   - Delete the `env:` blocks carrying `IIDP_DEPLOY_APP_PRIVATE_KEY` and `IIDP_DEPLOY_APP_ID` from both `iidp ci set-image` steps.
   - Change `version="<old>"` in both "Install iidp" steps to the step 1 release. The old CLI ignores the gate.

   Push to `main`. The run should print `Deploy hello <environment> <sha>` and "The Deploy gate committed". The Platform repository's commit should be authored by the pusher and committed by `iidp-deploy[bot]`. A call with a tag that was never pushed (for example a run from `main` whose `iidp ci set-image` step is given a made-up tag) is refused with HTTP 422, naming `ghcr.io/itema-as/hello:<tag>`, and commits nothing.
6. **Take the key out of CI.** Delete the org secret `IIDP_DEPLOY_APP_PRIVATE_KEY` and the org variable `IIDP_DEPLOY_APP_ID` (`gh secret delete IIDP_DEPLOY_APP_PRIVATE_KEY --org Itema-as`, `gh variable delete IIDP_DEPLOY_APP_ID --org Itema-as`). Any workflow still on an old `iidp` then fails loudly, naming the missing variable, instead of writing with the key.
7. **Rotate the App key**, as ADR-0005 asks, because the old key was readable by CI. Generate a new key on the App's settings page and delete the old one. Then follow `infra/README.md`, "Rotating the Platform repository credential", which updates the Secret that ArgoCD and the gate both read. The gate picks up the new key on its next call, with no restart.

Any other Application follows steps 4 to 5 with its own name and repository.
