# #5 Bootstrap wizard script

Questions that came up while building `scripts/bootstrap-wizard.sh`, the options considered, and the answer chosen for each. Sources were checked on 2026-09-21.

## Bash 3.2 or bash >= 4?

**Question.** macOS ships bash 3.2. The ticket allows either writing the wizard to run on it, or requiring a newer bash and checking for it.

**Choice.** Require bash >= 4.3, checked and logged at the top of the script (and again at the top of `test/wizard/run.sh`, which sources it). 4.3 rather than a bare 4.0 because the Entra helper (`az_create_entra_app`) hands three values (tenant, client id, client secret) back to its caller through namerefs (`local -n`), added in 4.3; bash 3.2 has neither namerefs nor associative arrays, and writing the whole wizard (idempotency lookups, wrapper functions, the Entra flow) against 3.2 would have meant working around both everywhere. The failure message tells the admin to `brew install bash` and invoke the script with it explicitly, since Homebrew's bash never replaces `/bin/bash`.

## GitHub App: who calls the manifest-conversion endpoint?

**Superseded by #47.** GitHub rejected this manifest on the real Platform, and the wizard no longer opens pages. The App is now created from printed manual steps, see [`47-wizard-prompts.md`](47-wizard-prompts.md). The section below records the original choice.

**Question.** The REST API cannot create a GitHub App directly; the manifest flow can, by POSTing a manifest to a GitHub page, which redirects back with a temporary `code`, which is then exchanged at `POST /app-manifests/{code}/conversions` for the app id, PEM and webhook secret (checked against the current GitHub REST API docs via context7). Does the wizard perform that exchange itself?

**Options.**
1. The wizard performs the exchange: capture the `code` from the redirect (needs a local HTTP listener or the admin pasting a URL), POST it, parse the JSON, save the PEM.
2. GitHub performs the exchange itself: when the manifest flow's redirect lands back on GitHub's own conversion URL, GitHub completes the handshake server-side and shows the new App's settings page directly, with the App ID visible and a "Generate a private key" button. The admin installs the App from the same page and reads the installation id from the resulting URL.

**Choice.** Option 2. It needs no listener, no pasted callback URL, and no JSON parsing in bash for a response that contains a private key. The wizard's job is only to open a self-submitting HTML form (a local temp file, since the manifest must be POSTed and is too large for a query string) targeting `https://github.com/organizations/<org>/settings/apps/new?state=<random>` with the manifest as a hidden field, then ask for the three values the resulting page shows: app id, the downloaded `.pem` path, and the installation id after the admin clicks "Install App". This matches the ticket's own wording ("ask for the resulting app id, installation id and the private key PEM path") and keeps the private key on the admin's disk, never in a variable the wizard has to serialise anywhere.

The manifest requests `contents: write`, no events, and `hook_attributes.active: false` (no webhook), name `iidp-deploy`. Secrets land as the org Actions secret `IIDP_DEPLOY_APP_PRIVATE_KEY` and the org Actions variable `IIDP_DEPLOY_APP_ID` (`gh secret set --org` / `gh variable set --org`, visibility `all`), which is what ticket #12's deploy workflow reads.

**Extended by #41.** The same App and the same downloaded PEM are also ArgoCD's credential for the Platform repository (`contents: write` already implies the read ArgoCD needs). `stage_github_app` now also writes `platform_repo_github_app_id`, `platform_repo_github_app_installation_id` and `platform_repo_github_app_private_key` into `infra/platform/terraform.tfvars`, idempotently and independently of whether `platform.yaml`'s `githubApp.id` was kept or a new App was created -- see [`41-argocd-platform-repo-credential.md`](41-argocd-platform-repo-credential.md) for why that second idempotency check was needed and how it works.

## Entra: which Graph permission ids?

Delegated Microsoft Graph permissions are added by object id, not name, when using `az ad app permission add`. From the Microsoft Graph permissions reference: `User.Read` is `e1fe6dd8-ba31-4d61-89e7-88639da4683d` and `GroupMember.Read.All` is `bc024368-1153-4739-b217-4326f2e966d0`, both delegated (`=Scope`). Both app registrations (ArgoCD/Dex and oauth2-proxy) get both permissions and admin consent, matching the ticket's "create them with ... the delegated Graph permissions named above" for "them" (both apps), even though oauth2-proxy strictly only needs `User.Read` today; `GroupMember.Read.All` is harmless to have in reserve if oauth2-proxy's group check needs it later, and keeping one rule ("both apps, same permissions") is simpler to state and to verify by hand when `az` is unavailable.

If `az ad app permission admin-consent` fails (the caller lacks Global/Privileged Role Administrator), the wizard does not fail the whole run: it records a manual step in the closing summary and moves on, since the app registration and its secret still work once consent is granted later.

## Where do oauth2-proxy's Entra credentials go?

**Question.** `bootstrap/README.md` lists exactly four SOPS-encrypted secrets under `bootstrap/sops/` (`argocd-entra`, two Cloudflare tokens, `grafana-cloud`); there is no fifth secret for oauth2-proxy yet because ticket #18 (Itema login) has not landed and no Platform component consumes it. But the wizard is asked to create the oauth2-proxy Entra app registration now, so it exists when #18 lands.

**Choice.** Write the tenant id, client id and client secret to a local, git-ignored file, `infra/platform/oauth2-proxy-entra.env` (`chmod 600`, `umask 077`), next to the other tfvars files the wizard already keeps out of git. It is not read by anything yet; #18's component reads it directly, or the admin re-runs this wizard once #18 defines where it belongs (most likely a fifth `bootstrap/sops/*.enc.yaml` file). The closing summary and this note both point at the file path, never at its contents. The oauth2-proxy redirect host, `auth.<baseDomain>`, is logged the same way (in the wizard's own output and here) so #18 does not have to re-derive it: `https://auth.<baseDomain>/oauth2/callback`.

**Superseded by #18.** The fifth `bootstrap/sops/*.enc.yaml` file guessed above is now real: `stage_entra` writes `bootstrap/sops/oauth2-proxy-entra.enc.yaml` directly (plus a generated `cookieSecret`) instead of the local env file; see [`docs/implementation-notes/18-itema-login.md`](18-itema-login.md).

## `chartVersion` and the bootstrap pin: same tag, different fallback

**Question.** `platform.yaml`'s `chartVersion` (the application chart version) and `bootstrap/platform-components.yaml`'s `targetRevision` (the pin on this repository's `bootstrap/` directory) are both meant to track "the current release", but one is a semver used to resolve an OCI chart and the other is a git ref.

**Choice.** Both default to the latest tag from `gh release list --repo <this repo>` when one exists (`chartVersion` with the leading `v` stripped, the bootstrap pin with it kept, since it is a git tag). When no release exists yet, `chartVersion` falls back to `0.1.0` (an ordinary placeholder version, matching the CLI's own default, per `docs/platform-repository.md`), but the bootstrap pin falls back to `main`: a fabricated tag would not exist as a git ref, and `bootstrap/README.md` already documents "upgrading the bootstrap is changing `targetRevision` to a newer `iidp` release tag", so `main` is the honest answer for "no tag exists yet". Both choices are logged as they are made.

## "Show the plan and confirm once", with two roots

**Question.** The ticket asks OpenTofu's stage to "show the plan and confirm once" for `infra/state-bucket` then `infra/platform`. Read one way, that is one confirmation covering both roots; read the other, it is one confirmation per root (as opposed to confirming twice per root, once for the plan and once again for the apply).

**Choice.** One confirmation per root: `stage_opentofu` shows the state-bucket plan and asks once, applies it, then shows the platform plan and asks once, applies it. A single confirmation covering both roots is not achievable in practice: `infra/platform` uses the S3 backend that `infra/state-bucket` creates, so its plan cannot even be computed (`tofu init`/`plan` would have nothing to talk to) until the state bucket actually exists. Showing both plans before asking anything would mean either applying the first root speculatively before the admin has agreed to anything, or fabricating the second plan's output. Asking exactly once per root, immediately after that root's own plan, is the reading that keeps "show the plan" and "confirm" adjacent for both.

## Idempotency: whole-file rewrite, not patching

**Question.** "Each step idempotent and skippable when its result already exists" could mean patching individual fields in `platform.yaml`/`bootstrap/*.yaml` in place, or always regenerating the whole file from values gathered this run (some fresh, some kept from before) and letting `git diff` decide whether anything actually changed.

**Choice.** Whole-file rewrite, the same approach `internal/render` already uses for the Platform repository's per-Environment files (#10's note: "written whole"). Every `platform.yaml`/`bootstrap/*.yaml` write is unconditional; `git_commit_if_changed` stages the result and skips the commit (and, transitively, the push) when nothing actually changed. This means a value that was "kept" still needs to be known in memory for the rewrite: each stage function populates its variables from the existing file (via `platform_yaml_get`/`tfvar_get`) whether the admin re-enters a value or presses Enter to keep it, so downstream stages and the final write always have a complete picture. `platform_yaml_get` is a small awk-based reader that understands exactly the two-space-indented, one-level-of-nesting shape this script itself writes; it is not a general YAML parser, and does not need to be one, because nothing else ever hand-edits the fields it reads back (`baseDomain`, `githubApp.id`, `acme.email`, and so on).

The four SOPS-encrypted secrets are the exception: there is no way to "regenerate and diff" a document whose plaintext the wizard cannot read back (the private key lives only in the cluster). For those, idempotency means asking the admin whether to keep the existing encrypted file, and if so, never touching it and never asking for the underlying token/credential again that run.

## Test seam

**Question.** The ticket asks for wrapper functions a test mode can replace, and a `test/wizard/` shell test exercising idempotency, run from a new CI job.

**Choice.** Three switches, all environment variables so no CLI surface is added purely for testing:

- `IIDP_WIZARD_SOURCE_ONLY=1` stops the script right after it defines its functions, before `main "$@"` runs, so `test/wizard/run.sh` can `source` it and call `tfvar_get`, `platform_yaml_get`, `validate_hetzner_token`, `stage_cloudflare`, and so on, directly.
- `IIDP_WIZARD_FAKE=1` makes every network/`gh`/`az`/`sops` wrapper function return a fixed, deterministic result instead of calling out, so the test suite needs no credentials and no network.
- `IIDP_WIZARD_ANSWERS=<file>` gives `ask`/`ask_secret` a `KEY=value` file to read from instead of `/dev/tty`, for the handful of tests that need to drive a stage through its "nothing exists yet, collect a fresh value" path rather than its "existing value found, keep it" path.

`--dry-run` is a fourth, orthogonal switch (a real CLI flag, since it is part of the delivered script, not just its tests): every wrapper function checks it first and, if set, prints what it would do and returns a synthetic success without touching the network, disk, or `/dev/tty` at all. This is what makes `--dry-run` usable as a smoke test with no setup, and it is also what `test/wizard/run.sh`'s full-script test runs.

One bash subtlety the test file works around: `VAR=1 source file` only sets `VAR` for the duration of that one `source` command (bash's normal "temporary environment for a simple command" behaviour applies to builtins like `source` too), so it reverts the moment sourcing finishes, before any of the sourced functions are called afterwards. `test/wizard/run.sh`'s `in_wizard` helper assigns `IIDP_WIZARD_SOURCE_ONLY`/`IIDP_WIZARD_FAKE` as plain statements before sourcing instead, so they persist for the rest of that subshell.

## Prompts never read stdin

Every `ask`/`ask_secret`/`confirm`/`pause` call reads from `/dev/tty` explicitly, never from the script's own stdin, so the wizard still works if something is piped into it (for example, an admin scripting most of the answers via a here-doc while still wanting to eyeball a couple of prompts). If there is no controlling terminal at all, prompts fail with a clear error rather than hanging or silently reading garbage; `--dry-run` and `IIDP_WIZARD_FAKE=1` are the two ways to run the script with no terminal at all.

## Secrets on disk

Hetzner and Object Storage credentials go into the two `terraform.tfvars` files (already git-ignored, `chmod 600`). The Cloudflare token, Grafana Cloud credentials and Entra client secrets exist on disk only as long as `write_secret_plaintext` writes them to `bootstrap/sops/<name>.enc.yaml` in the clear, immediately followed by `sops --encrypt --in-place` on the same path — the same two-step procedure `bootstrap/README.md` already documents as the manual fallback. Nothing is ever written to `docs/implementation-notes/`, the wizard's own log, or any other file in the clear; every echoed prompt default for a secret field is masked (`ab****yz`).

## What a review changed, and what it did not

A two-axis review (repository standards, then the ticket's acceptance criteria) was run before the pull request. Fixed: the ArgoCD (Dex) and oauth2-proxy app registration blocks in `stage_entra` were near-identical, differing only in the app name, redirect URI, default tenant and whether to open the Azure portal URL again; they are now one `entra_register_app` helper called twice. `html_escape` was missing `"` from what it escaped, which happened to be safe only because its one call site embeds the result in a single-quoted HTML attribute; a function named as a general escaper now escapes `"` too, and a test locks the full set in. The "show the plan and confirm once" reading (one confirmation per OpenTofu root, not one for both) is recorded above rather than changed, since a single confirmation covering both roots cannot work: `infra/platform`'s plan cannot be computed before `infra/state-bucket` has actually been applied.

Left as they were: the recurring `DRY_RUN`/`IIDP_WIZARD_FAKE`/real three-way branch in most wrapper functions, and the five `write_*` functions each following the same dry-run-check/`mkdir -p`/heredoc/`ok` shape. Both are the price of the test seam (every wrapper function needs all three branches to be independently traceable and fakeable) and of writing each Platform repository file as a plain, readable heredoc rather than through a shared templating layer this script has no other use for.

## `bootstrap/applications.yaml` (found on the first real Platform)

The Platform repository's `bootstrap/` needs three Applications: `platform-components.yaml`, `platform-secrets.yaml` and `applications.yaml`, the last of which discovers every Environment's `applications/<name>/<environment>/application.yaml`. The wizard wrote only the first two. The kind e2e fixture had all three, hand-written in #9 with a comment saying every Platform repository carries the third "hand-written", but nothing ever told an admin to write it. So on the first real Platform, `iidp app create hello` committed both Environments and ArgoCD never created them, while the e2e run passed on the fixture's copy. `write_bootstrap_applications` now writes it alongside the other two, in the same commit. A wizard test asserts that the wizard writes every top-level file the fixture's `bootstrap/` has, so the fixture can't drift ahead of the wizard again without failing CI.
