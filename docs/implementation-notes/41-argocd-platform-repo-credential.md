# #41 ArgoCD reads the private Platform repository from first boot

Decisions taken while implementing #41 that the ticket left open. Sources were checked on 2026-09-22.

## Confirming the repository Secret shape

**Question.** The ticket names the fields (`githubAppID`, `githubAppInstallationID`, `githubAppPrivateKey`) but not the exact Secret shape ArgoCD expects.

**Choice.** Confirmed via Context7 against the current ArgoCD documentation ("Configure GitHub App authentication for repositories in YAML" and "Repository Credentials", `argo-cd.readthedocs.io`): a plain Kubernetes `Secret` in namespace `argocd`, labelled `argocd.argoproj.io/secret-type: repository`, with `stringData.type: git`, `stringData.url` (the Platform repository's HTTPS URL), `stringData.githubAppID`, `stringData.githubAppInstallationID` and `stringData.githubAppPrivateKey` (the PEM, as a block scalar). No other fields are required for a GitHub App credential; `githubAppEnterpriseBaseUrl` is GitHub Enterprise-only and does not apply here. cloud-init applies exactly this shape, named `platform-repo-github-app`.

One deliberate deviation from the documentation's own example: it shows `githubAppID: 1` and `githubAppInstallationID: 2` unquoted. `Secret.stringData` is `map[string]string`; an unquoted digit string is a plain YAML integer, and `kubectl apply`'s YAML-to-JSON conversion would hand the API server a JSON number for a field the API's decoder expects to unmarshal as a Go string -- the same "quote your ports and versions" footgun `ConfigMap`/`Secret` data has always had. cloud-init quotes both (`githubAppID: "$PLATFORM_REPO_GITHUB_APP_ID"`), which the documentation's own example does not need to, since it is illustrating the shape rather than being copy-pasted with shell-substituted digits the way this template is.

## Read permission: already covered by `contents: write`

**Question.** The ticket asks the docs to describe "the read permission the App needs on the Platform repository."

**Choice.** Nothing to add on the App side: the manifest `stage_github_app` already submits (`scripts/bootstrap-wizard.sh`) requests `default_permissions: {contents: "write"}` for ticket #12's CI write-back, and ArgoCD's own documentation states the App "must have at least Read-only permissions to the Contents" of the repository — write already implies read. The same App, the same installation, is reused as ArgoCD's credential rather than a second App being created, so this is purely a documentation task: `infra/README.md`'s prerequisites and `bootstrap/README.md` now say so explicitly.

## Carrying the PEM through templatefile()/YAML/bash: base64, not raw text

**Question.** The ticket allows either base64-encoding the key in the OpenTofu template and decoding it on the node, or writing it with a quoted heredoc, and asks that both the multi-line shape and templatefile()'s `${}` escaping be tested.

**Options.**
1. Interpolate the raw PEM as a `templatefile()` value directly into the cloud-config YAML, inside a bash block-scalar assignment (`GITHUB_APP_PRIVATE_KEY="${platform_repo_github_app_private_key}"`). Three problems at once: the PEM's own newlines break a plain quoted bash assignment; cloud-init's own YAML dedent of the `content: |` block would need every PEM line individually re-indented by OpenTofu, which `templatefile()` cannot do to an interpolated value; and a key that happened to contain `${` (vanishingly unlikely for base64-free PEM text, but not something to rely on) would collide with `templatefile()`'s own interpolation syntax.
2. base64-encode the PEM once, in `bootstrap.tf` (`base64encode(var.platform_repo_github_app_private_key)`), pass the base64 string as the template variable, and decode it in a single line on the node (`base64 -d`). A base64 string has none of the above problems: no newlines, no `${`, no quoting hazard.

**Choice.** Option 2, as the ticket's own wording favours. Verified two ways, both recorded as trade-offs rather than left to a real `tofu apply` to discover:

- **The doubled-dollar escaping.** A small Python script substituted fake values for every `templatefile()` variable in `user-data.yaml.tftpl` (mimicking what OpenTofu's renderer does: `$${` becomes `${`, `${var}` becomes the value) and `shellcheck` was run against the result — 0 findings, confirming the rendered script is valid bash with no template markers left unresolved. This is not run as an automated test (see "cloud-init template testing", below, for why); it was a one-off check while writing the change.
- **The multi-line nesting.** `infra/platform/cloud_init_test.go`'s `TestCloudInitRepositorySecretIsWellFormedYAML` reconstructs what the script's own `kubectl apply -f - <<EOF` heredoc actually feeds to `kubectl`: the static YAML lines from the template, dedented by cloud-init's own block-scalar rule (strip the first line's indentation from every line), plus a stand-in private key indented the way the script's `sed 's/^/    /'` indents the decoded key, parsed with `gopkg.in/yaml.v3`. This is the one property a plain text search cannot see — that `githubAppPrivateKey: |`'s nested block scalar is actually indented deeper than its sibling keys once cloud-init's own dedent is accounted for — and it did catch a real bug during development: the first draft left the `$(printf ... | sed ...)` line at column 0 in the template instead of matching the surrounding 6-space indentation, which would have made cloud-init's YAML parser end the `content: |` block early and fail to boot the node at all.

## Never logging the key

**Choice.** The decoded key lives only in the shell variable `GITHUB_APP_PRIVATE_KEY`, fed directly to `kubectl apply`'s stdin via a heredoc, and `unset` immediately after. It is never passed to the script's own `log()` helper or to a bare `echo`, which matters because this whole script's stdout and stderr are teed to `/var/log/iidp-bootstrap.log` (`exec > >(tee -a ...) 2>&1`, set up before this script does anything else) — anything printed anywhere in the script ends up in a log file a future `cat` or `less` could expose. No file on disk ever holds the plaintext key either (the ticket's other option, a mode-0600 file removed afterward, was not needed: a heredoc-to-stdin never touches disk at all).

## cloud-init template testing: text and reconstructed-YAML, not a real render

**Question.** The ticket suggests either shelling out to `tofu console`/a small render harness, or a Go test that reads the template as text; it calls the latter "simplest" and asks for the trade-off to be logged.

**Choice.** The text/reconstruction approach (`infra/platform/cloud_init_test.go`), for three tests:

1. `TestCloudInitReferencesThePlatformRepoCredentialVariables` -- the three new `bootstrap.tf` locals are genuinely read somewhere in the template, guarding against the OpenTofu side and the cloud-init side drifting apart.
2. `TestCloudInitAppliesThePlatformRepoCredentialBeforeTheRootApplication` -- the acceptance criterion itself, checked as a byte-offset comparison: `kind: Secret` (and its `argocd.argoproj.io/secret-type: repository` label) appear before `kind: Application`.
3. `TestCloudInitRepositorySecretIsWellFormedYAML` -- see above.

**Trade-off.** None of these three renders the template the way `tofu apply` actually would. A real render needs OpenTofu's own `templatefile()` (HCL template syntax, including the doubled-dollar escaping this file documents) and, beyond syntax, a real Hetzner apply to know whether cloud-init genuinely accepts the rendered YAML and the script genuinely runs to completion on a real node. Reimplementing HCL template syntax in Go to get a byte-identical render would be a second template engine to keep in sync with OpenTofu's own, for coverage `tofu validate` (in CI on every PR that touches `infra/`, per `.github/workflows/infra.yaml`) and `tofu fmt -check` already give from the OpenTofu side, and a real `tofu apply` is the only thing that can ever confirm the node actually boots. What the three tests above add on top of `tofu validate` is specifically the two properties that live entirely in the template's own text and are invisible to HCL syntax checking: the credential-before-root-Application ordering, and the nested block scalar's indentation once cloud-init's own YAML dedent is accounted for.

## Wizard: where the tfvars file lives, and why it needed to become a variable

**Question.** `scripts/bootstrap-wizard.sh` had no existing concept of "the `infra/platform` directory" as anything other than a hardcoded path inside `stage_opentofu` (which is not exercised under `IIDP_WIZARD_FAKE=1` at all -- it shells out to `tofu` for real, gated only by `--dry-run`). Writing the three new tfvars from `stage_github_app` needed a path that a fake-mode test could redirect to a scratch directory, the same way `PLATFORM_REPO` already is, without ever risking a test writing into this clone's own `infra/platform/terraform.tfvars`.

**Choice.** `INFRA_PLATFORM_DIR`, a plain global initialised to `$IIDP_REPO_ROOT/infra/platform` right next to `IIDP_REPO_ROOT` itself, following the same pattern `PLATFORM_REPO` already uses: a hard default, reassignable as a plain variable by a caller (or, in `test/wizard/run.sh`, by `in_wizard`'s `eval`'d test code) after sourcing. Every `test/wizard/run.sh` case that exercises `stage_github_app` now sets it to a `scratch_dir`, so no test run ever touches a real `terraform.tfvars`.

## Idempotency: platform.yaml's `githubApp.id` and the tfvars PEM are two independent facts

**Question.** `stage_github_app` already had one idempotency check (`platform.yaml`'s `githubApp.id`, kept or not, gating whether a new App is created at all). The PEM lives somewhere `platform.yaml` cannot help with -- it is a secret, never written there -- so a second, independent check was needed for whether ArgoCD's tfvars credential already exists.

**Choice.** Two nested checks, not one. Keeping `githubApp.id` (no new App created) says nothing about whether `infra/platform/terraform.tfvars` already has a private key for it -- the field predates this ticket, and an admin who bootstrapped before #41 landed will have a `githubApp.id` in `platform.yaml` but nothing in `terraform.tfvars` yet. So the "keep" branch reads `tfvar_get_multiline` on the tfvars file, and:

- if a key is already there, offers to keep it (the common re-run case, once #41 is live);
- if not, asks for the PEM path directly (the upgrade case: an existing Platform admin picking up #41), without needing to touch the GitHub App or its manifest flow at all, since the App and its id/installation already exist and only the local credential file is missing.

Whole-file rewrite (`docs/implementation-notes/05-bootstrap-wizard.md`, "Idempotency") extends naturally to the heredoc case: `tfvar_set_multiline` always removes any previous value for the key -- whether it was a single `tfvar_set` line or an earlier heredoc block of its own -- and writes a fresh block, so re-running the stage is a clean upsert regardless of which helper wrote the previous value.

## Heredoc form in `terraform.tfvars`, not a second file

**Question.** The ticket allows the PEM to be written as a heredoc-style multi-line string directly in `terraform.tfvars`, or as a path with OpenTofu's `file()` function reading a second file the tfvars side points at.

**Choice.** The heredoc, inline in `terraform.tfvars` (`tfvar_set_multiline`, an HCL `<<IIDP_..._EOT ... IIDP_..._EOT` block -- valid syntax in a `.tfvars` file, which OpenTofu parses with the same HCL grammar as a `.tf` file). `terraform.tfvars` is already git-ignored (`infra/platform/.gitignore` matches `*.tfvars`, keeping only `terraform.tfvars.example` tracked) and already `chmod 600` by every `tfvar_set`/`tfvar_set_multiline` call, so nesting the PEM inside it keeps the key out of git exactly as well as a second file would, with one fewer path for a future change to forget to `.gitignore`. A separate file (`platform_repo_github_app_private_key = file("./app-key.pem")`) would need its own gitignore entry, its own permissions, and would leave two files to keep in sync (delete one, forget the other) on every rotation; the heredoc form has none of that, at the cost of a marginally less familiar syntax for an admin editing `terraform.tfvars` by hand instead of through the wizard.

## Rotation: re-running `iidp-bootstrap` on the node, not a new `tofu apply`

**Question.** `hcloud_server.node`'s `lifecycle { ignore_changes = [user_data] }` (`docs/implementation-notes/03-infra.md`) means a changed `platform_repo_github_app_private_key` in `terraform.tfvars` never reaches the node through `tofu apply` alone, the same way a `k3s_version` bump does not.

**Choice.** The exact same pattern the k3s/ArgoCD/helm version bumps already use (`infra/README.md`, "Bumping k3s or ArgoCD"): change the variable (record of intent; `tofu plan` shows no change, by design), then re-run `iidp-bootstrap` on the node with the new value passed through the environment -- `PLATFORM_REPO_GITHUB_APP_PRIVATE_KEY_B64`, base64-encoded on the admin's machine the same way OpenTofu encodes it for the first boot. A new `infra/README.md` section, "Rotating the Platform repository credential", documents the exact command. The alternative the ticket floated -- the script reading the key from a file the admin copies onto the node -- was not chosen: it would need the admin to `scp` a plaintext PEM onto the node's disk (even briefly) and clean it up afterward, where passing it through the environment of one SSH command never touches the node's disk at all, matching how the decoded key already never touches disk during a first boot (see "Never logging the key", above).

## What was not built

Pagination or multi-installation handling for the App is out of scope here as it already was for ticket #12 (`docs/implementation-notes/12-deploy-workflow.md`, "What was not built") -- this ticket reuses the same App and installation, not a second one. No change to `.github/workflows/infra.yaml`: the existing `tofu fmt`/`tofu validate` matrix already covers the new variables and the `go test ./...` job in `.github/workflows/ci.yaml` already picks up `infra/platform/cloud_init_test.go` with no path filter to adjust. `test/e2e/harness.go` (the kind harness's anonymous in-cluster git server) is unchanged and untouched by this ticket, exactly as the ticket asks: it is a separate credential-free path for CI, not a stand-in for the real node's.
