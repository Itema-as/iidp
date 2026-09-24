# #59 The node pulls private images from GHCR

Decisions taken while implementing #59, the node's GHCR pull credential from ADR-0005. Sources were checked on 2026-09-24: the k3s private registry documentation (Context7 `/k3s-io/docs` and `/websites/k3s_io`, and `docs.k3s.io/installation/private-registry`), k3s's own source for how it turns `registries.yaml` into containerd configuration (`pkg/agent/containerd/config.go`, `getHostConfigs`), and GitHub's Packages documentation (Context7 `/github/docs`).

## The shape of `registries.yaml`

**Choice.** One entry under `configs`, no `mirrors`:

```yaml
"configs":
  "ghcr.io":
    "auth":
      "password": "ghp_..."
      "username": "<login>"
```

The k3s documentation gives `configs.<registry>.auth.username`/`password` as the basic-auth credentials for a registry, and says changes take effect only after k3s is restarted on each node. What it doesn't say outright is whether a registry that appears only under `configs`, with no `mirrors` entry, gets those credentials on its default endpoint. Its prose ("the TLS and credential configuration for each mirror") suggests a mirror is needed. k3s's source settles it: `getHostConfigs` first loops over `registry.Configs` and creates a host config with the default endpoint for every entry ("create config for default endpoints"), and only then over `Mirrors`. A mirror entry would add nothing: ghcr.io is its own endpoint.

The file is built with `yamlencode` in `bootstrap.tf` (`local.registries_yaml`), not typed out, so no username or token can break its quoting. The variables' validation keeps both to characters that need no quoting anyway (a GitHub login, and a token of letters, digits and `_`), because they also end up in a shell pipe in the rotation recipe.

## The username: the token owner's login

**Question.** ghcr.io's basic-auth username is either ignored, so any non-empty string works, or must be the login of the account that owns the token.

**Choice.** The owner's login, read by the wizard from the same `GET /user` response that carries the scopes, and stored as `ghcr_pull_username`. GitHub's documentation always shows `docker login ghcr.io -u USERNAME` with the account's own username. I could not test whether ghcr.io ignores it: that needs a valid token with `read:packages`, and I had none. The owner's login is right in either case, and it makes `registries.yaml` say whose token the node carries, which matters when #56 moves it to a machine user.

## cloud-init writes the file with `write_files`, not `iidp-bootstrap`

**Options.**
1. A step in `iidp-bootstrap`, before the k3s install, reading overridable environment variables like the #41 credential. Rotation would then be "re-run `iidp-bootstrap` with `GHCR_PULL_TOKEN=...`".
2. A `write_files` entry in the cloud-config.

**Choice.** Option 2. cloud-init runs `write_files` in its init stage and `runcmd` (which starts `iidp-bootstrap`, which installs and starts k3s) in its final stage, so the file exists before k3s first starts with no ordering logic at all. The entry is not `defer: true`, which would move it to the final stage. The deciding reason, though, is the running node: its `/usr/local/sbin/iidp-bootstrap` was written by an older cloud-init and has no such step, and cloud-init never runs again. Option 1 would need one recipe for old nodes (write the file by hand) and another for new ones (re-run the script), plus the script restarting k3s when the file changed but k3s did not. With option 2 there is one recipe for every node, as the issue asks: write the file over ssh, restart k3s. The cost is that re-running `iidp-bootstrap` never repairs the file. `infra/README.md` says so in "Re-running the bootstrap".

The content travels base64-encoded (`encoding: b64`), for the same reason as the #41 PEM: a YAML document inside the cloud-config's YAML then needs no re-indenting and no escaping. Mode `0600`, owner `root:root`, as the issue asks. k3s runs as root, and the file holds a token.

## The token reaches a running node through a sensitive output

**Question.** The rotation recipe has to get the same file onto the node. Typing it into a heredoc puts the token in shell history and repeats the format by hand. Parsing `terraform.tfvars` in the shell (as `tofu-env.sh` does for two keys) would repeat the format too.

**Choice.** `local.registries_yaml` is also the sensitive output `ghcr_registries_yaml`, and the recipe pipes `tofu output -raw ghcr_registries_yaml` into `ssh`, where `umask 077` creates the file with mode 600 and `mv` puts it in place whole. There is one definition of the file, used both by cloud-init and by the recipe. The catch is that an output only changes on `tofu apply`, so the recipe applies once after editing `terraform.tfvars`. Since the server ignores changes to its user data, that apply changes only outputs. The recipe says to stop if the plan shows anything else. The output is in the state, like every other secret this root already keeps there (the Hetzner token, the App key).

## Required variables, no default

`ghcr_pull_username` and `ghcr_pull_token` are required, like the App credential from #41. Every Application's image is private from now on (ADR-0005), so a node without the token can't run any Application. A default of `""` would render a `registries.yaml` that fails every pull without saying why. The consequence for the Platform that is already running: once this is merged, `tofu plan` and `tofu apply` in `infra/platform` refuse to run until both variables are in `terraform.tfvars`. That is step 2 of the README recipe, before the `tofu apply` in step 3, so following the recipe in #62 never hits it. `tofu validate` doesn't need values, so CI is unaffected.

## The wizard's check

`stage_ghcr` (stage 6 of 10, after the GitHub App and before Entra, so before OpenTofu) prints the classic-token page with the scope and description filled in (`/settings/tokens/new?scopes=read:packages&description=...`), the Settings path to it, "No expiration" and "read:packages only". Then `verify_ghcr_pull_token` runs before anything is written:

- `github_pat_...` is refused before any call: it is fine-grained, and ghcr.io refuses fine-grained tokens for pulls.
- A token with characters other than letters, digits and `_` is refused (a paste error; the OpenTofu validation would refuse it later anyway).
- `GET https://api.github.com/user` with the token, as `internal/github` does for the `workflow` scope (#54, `47-workflow-scope.md`). A 401 is "GitHub refused the token". `/user` rather than the API root because it also returns the owner's login.
- **No `X-OAuth-Scopes` header at all** means it is not a classic token (fine-grained and GitHub App tokens have permissions, not scopes), so it is refused. This is the reverse of the CLI's workflow-scope check, which lets an unknown token through: there the push might still work, but here ghcr.io is known to refuse any token that isn't classic.
- **A header without `read:packages`** is refused, and the message names the scopes it has. An empty header (a classic token with no scopes) is refused the same way. `write:packages` counts, since it includes read. The message says the scopes can be edited on the token's page without changing its value.
- **Scopes beyond `read:packages`** (including `write:packages`) get a warning, because the token sits in plaintext on the node, and a confirm, answered no by default. So an admin who knowingly reuses a broader token can still go on.

The token is then written to `terraform.tfvars` with its login. `_http` now saves the response headers (`curl -D`) and `http_header` reads one without regard to case, returning non-zero for a missing header so that it can be told apart from an empty one.

**Idempotency.** An existing `ghcr_pull_token` is offered with "Keep it?", like the App key. Unlike the Hetzner token, a kept token is checked again. The check is one read-only call, and a token revoked since the last run is exactly the failure that would otherwise show up much later as an image that won't pull. It also refreshes `ghcr_pull_username` from the token itself.

**Fake mode.** `_fake_http` answers `https://api.github.com/user` like the real API. `IIDP_WIZARD_FAKE_GHCR_TOKEN` (default `ghp_fakeghcrtoken`) gets 200 with `x-oauth-scopes: $IIDP_WIZARD_FAKE_GHCR_SCOPES` (default `read:packages`, may be set empty). A `github_pat_` token gets 200 with no header, and anything else gets 401 `Bad credentials`.

**Why not ask ghcr.io itself.** ghcr.io's token endpoint (`https://ghcr.io/token?service=ghcr.io&scope=repository:<org>/<image>:pull`) is the most direct proof. Probed with an invalid token and with no credentials, it answers `403 {"errors":[{"code":"DENIED",...}]}` both times, so a bad token can be recognised. But I had no valid token to see what success looks like, especially for an org with no private package yet, and the issue asks for the scope check. A probe whose success response I haven't seen could refuse a correct setup. The end-to-end proof is the `crictl pull` in the README's recipe instead.

## "No expiration"

As the issue says. Nothing on the Platform could warn before an expiry, and an expired token shows up only when a new image fails to pull, which is exactly when an Application is being deployed. The exposure is limited by the one read-only scope, and #56 moves the token to a machine user that no person uses to sign in.

## #56 and the Deploy gate (#60/#61)

- **Machine user (#56).** The README recipe doesn't assume the token is the admin's. Step 1 says "signed in as the account that will own it", and the login comes from the token. Moving the token is the same six steps with the machine user's token, followed by deleting the admin's. The machine user needs read access to the packages. Images pushed by an Application's workflow with `GITHUB_TOKEN` are linked to that repository and inherit its access, and org members have access to every repository (ADR-0005's Consequences), so org membership should be enough. The recipe's troubleshooting names that, and the organisation's policy for classic tokens (organisation Settings > Personal access tokens), as the two things to check when ghcr.io refuses a token that passed the scope check. I did not check whether `Itema-as` restricts classic tokens.
- **Deploy gate.** The gate will need its own copy of this token in the cluster, to check that an image tag exists. Not built here. Nothing here gets in its way. The token is an OpenTofu variable, not something that exists only as a file on the node. So a later change can hand it to the template again, for example as a Secret applied by `iidp-bootstrap` the way #41 applies the ArgoCD credential. The variable names say what the token is (`ghcr_pull_*`), not where it goes (`registries.yaml`). When that lands, the rotation recipe gains a step for the in-cluster copy.

## Tests

- `test/wizard/run.sh`: `http_header`'s case and empty-versus-missing handling. `stage_ghcr` covers: the printed page, path, scope and expiry; accepting a classic `read:packages` token (login and token in the tfvars); refusing a token without `read:packages`, one with no scopes, one with no scope header, a fine-grained one (with no API call made), and one GitHub doesn't know; the extra-scope warning, both agreed to and declined; `write:packages` taken as covering read; keeping an existing token (not asked again, but checked, and written once); refusing a kept token that is now revoked; and asking for a new one when the admin declines to keep it. The `--dry-run` smoke test now expects ten stages. Disabling the `read:packages` refusal fails the two scope tests.
- `infra/platform/registries_test.go` renders the template twice and puts both renders through the same assertions. The whole cloud-config must parse. `write_files` must have exactly one `/etc/rancher/k3s/registries.yaml` entry: mode `0600`, `root:root`, not deferred, base64 that decodes to YAML with `configs."ghcr.io".auth` holding the given login and token and nothing else. The token must not appear in the `iidp-bootstrap` script. `TestCloudInitWritesRegistriesYAMLBeforeK3sStarts` renders with a Go stand-in for the only template syntax the file uses (`${name}`, and `$${` for a literal `${`). The stand-in fails on anything else (a `%{` directive, an expression, a name the test doesn't supply), so it can't silently render a template it doesn't understand. It runs everywhere, including CI's Go job, which has no `tofu`. `TestOpenTofuRendersRegistriesYAML` copies `bootstrap.tf`, `variables.tf` and the template into a scratch root with no provider or backend, applies it against fixture variables with local state, and checks `local.user_data` as OpenTofu actually renders it, `yamlencode`, `base64encode` and variable validation included. It skips when `tofu` is not on `PATH`, and `IIDP_REQUIRE_TOFU=1` makes that a failure. This is the "rendered cloud-init" test the issue asks for. The #41 note judged a real render not worth a second template engine. A scratch OpenTofu root isn't one, it takes well under a second, and it's the only test that covers `bootstrap.tf`'s own expressions. It doesn't run in CI, because the workflow that installs `tofu` runs only `fmt` and `validate`, and changing workflows was out of scope for this issue. Setting the permissions to `0644` in the template fails both tests.

## What was not built

No wizard flag to run only this stage. Re-running the whole wizard is idempotent, and the README recipe does the same by hand. No automatic k3s restart anywhere: only the recipe restarts k3s, and it says what a restart touches. No change to the chart. The credential is node-level (ADR-0005), so Environments carry no pull Secret.
