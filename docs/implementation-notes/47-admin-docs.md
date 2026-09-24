# #47 Admin documentation: the SSH tunnel and the state backend credentials

Items 9 and 10 of #47, both found while running the acceptance in #19 against the real Platform: `infra/README.md` had two recipes that could not work as written. This file records each change and why. The wizard, `.gitignore`, bootstrap, chart and CLI items of #47 are recorded separately.

## Item 9: the Kubernetes API through an SSH tunnel

**Problem.** "Fetching the kubeconfig" rewrote the kubeconfig's server from `127.0.0.1` to the node's public IP. The firewall in `infra/platform/hetzner.tf` opens only 22, 80 and 443, so port 6443 is unreachable from outside and `kubectl` timed out.

**Why not open 6443.** The closed port is intended. The Kubernetes API is admin-only (`docs/design.md`'s access model: "kubeconfig only for the Platform admin"), and SSH is already the admin's only way onto the node. Opening 6443 to the internet would expose the API server for no gain, and restricting it to the admin's address would break whenever that address changes.

**Choice.** The README section is now "Reaching the Kubernetes API", with three steps:

1. Fetch `/etc/rancher/k3s/k3s.yaml` unchanged into `~/.kube/iidp.yaml`. It already points at `https://127.0.0.1:6443`, and the k3s API certificate includes `127.0.0.1`, so TLS verifies through the tunnel with no edits. The file is created and `chmod 600`ed *before* the kubeconfig is written into it, because it holds the cluster-admin client key and should never be readable by others, not even briefly.
2. Keep `ssh -N -o ExitOnForwardFailure=yes -L 6443:127.0.0.1:6443 root@<ip>` open in its own terminal. `ExitOnForwardFailure=yes` was added to the recipe in the issue: without it, `ssh -N` whose local port is already taken prints a warning and then stays connected with no forward, which looks like a working tunnel.
3. `KUBECONFIG=~/.kube/iidp.yaml kubectl ...`.

This recipe was used successfully against the real Platform.

**If local port 6443 is taken** (another local cluster, such as Docker Desktop, k3d or Rancher Desktop), forward another local port and change the kubeconfig's server port with `kubectl config set-cluster default --server=https://127.0.0.1:16443`. That is used rather than `sed -i` because `sed -i` differs between macOS and Linux, and the portable `-i.bak` form would leave a second copy of the admin credentials next to the first. k3s names the cluster in its kubeconfig `default`. The certificate check compares only the host, not the port, so TLS still verifies.

**Rebuilding the node** also used `kubectl`, so that recipe changed too:

- Saving the age key now needs the tunnel open to the old node.
- The key is saved to `~/iidp-sops-age.yaml`, created with mode 600 like the kubeconfig. The old recipe wrote `sops-age.yaml` to the current directory, which could be inside this public repository's checkout, and nothing gitignores that name.
- After the rebuild, the kubeconfig has to be fetched again and the tunnel reopened, because the new node is a new cluster with new certificates.
- If Hetzner hands the new server the old address, `ssh` refuses the changed host key, so the recipe explains `ssh-keygen -R <ip>`.

**Not changed here.** `scripts/bootstrap-wizard.sh`'s closing summary still prints the old `sed "s/127.0.0.1/<ip>/"` recipe (`closing_summary`, the "Kubeconfig:" line). The wizard belongs to another part of #47 and is being edited in parallel, so it is left for that change. It should print the fetch-unchanged-plus-tunnel recipe, or point at "Reaching the Kubernetes API" in `infra/README.md`.

## Item 10: loading the state backend credentials in a fresh shell

**Problem.** `infra/platform`'s S3 backend reads `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY`, because a backend block cannot read OpenTofu variables (#3's notes). The wizard exports them only inside its own process. The README mentioned the exports only in "First apply", with placeholders to type in. So every later recipe, starting with `tofu output -raw node_public_ipv4`, failed in a new terminal with "No valid credential sources found".

**Options considered.**

- *Tell the admin to `export` the two keys by hand.* This is what the README said before. It puts the secret key into shell history, or makes the admin copy it from the tfvars file each time. The issue rules it out.
- *Have the wizard write an env file* (`.envrc`, `infra/platform/.env`). That is a second copy of the same secret to keep in sync and to keep out of git, and it would be a wizard change, which is outside this part of #47. The keys already sit in `infra/state-bucket/terraform.tfvars`, which is gitignored and mode 600 (the wizard's `tfvar_set`).
- *A wrapper that runs one command with the keys set* (`infra/tofu.sh output ...`). This works for plain `tofu` calls. But most recipes nest `tofu output` inside an `ssh` command line (`ssh root@$(tofu output -raw node_public_ipv4) ...`), so every one of those would need rewriting around the wrapper, and a copied recipe that forgot the wrapper would still fail.
- *A sourceable script that exports both keys from the state-bucket tfvars.* **Chosen.** The admin runs one line per terminal, `source ../tofu-env.sh`, and every existing recipe then works unchanged.

**`infra/tofu-env.sh`.**

- It reads `object_storage_access_key` and `object_storage_secret_key` from `state-bucket/terraform.tfvars`, found relative to the script's own location, so it doesn't matter where the shell is. If a key appears more than once, the last line wins, the same way `tfvar_get` in the wizard reads it. It then exports the two variables.
- It prints the file it read, never a key.
- It fails with a clear message, returning 1 and exporting nothing, when the file is missing, when a key is missing, or when a key still holds the example's `"..."` placeholder. The message names the file and the missing key or keys. In any of those cases, keys already in the shell are left alone.
- `IIDP_STATE_TFVARS` overrides the file. That covers an admin who keeps the tfvars outside the checkout. It is also how the tests point the script at fixtures, since fixtures can't be named `terraform.tfvars` on the development machine (its sandbox denies reading that name).
- It works when sourced from zsh as well as from bash 3.2 or newer. zsh is the default login shell on macOS, where the admin works. In bash the script's path comes from `BASH_SOURCE`, and in zsh from `$0`, which zsh sets to the sourced file's path. That value is read at the top level of the script, because inside a zsh function `$0` is the function's name.
- It is written as a sourced file should be: no `set -e`, no `exit` on the sourced path (it would close the admin's terminal), and helpers that are unset again before it returns, so only the two exports are left behind.
- Running it instead of sourcing it (`./tofu-env.sh`) exits 2 with "source this file instead of running it". Otherwise it would appear to succeed while the exports died with its process.

**README.** A new section, "Loading the state backend credentials", explains why the step is needed, how to run it and how it fails. Every recipe that runs `tofu` in `infra/platform` now starts with `cd infra/platform` and `source ../tofu-env.sh`, so each can be pasted into a new terminal from the root of the clone: first apply step 2, the age public key, the kubeconfig fetch and the tunnel, bumping k3s/ArgoCD/helm, rotating the Platform repository credential, and the two steps of rebuilding the node that use `tofu` (the rebuild itself, and re-running `iidp-bootstrap` on the new node). The "Bootstrap wizard" section points at the new section too, because that is where an admin coming from the wizard lands. `infra/state-bucket` needs none of this: it keeps its state in a local file and reads the keys as ordinary variables. The comments in `infra/platform/backend.tf` and `infra/platform/terraform.tfvars.example`, which name the two environment variables, now also name the script.

## Tests

`test/tofu-env/run.sh` follows `test/wizard/run.sh`'s style (`t_start`/`assert_*`, a summary line, a non-zero exit on any failure). Each case sources the script in a new `bash` or `zsh` process, the way a new terminal would, with no AWS keys inherited. It checks:

- A fixture in the wizard's exact format exports both keys. A repeated key takes its last value, and a key containing `/`, `+` and `=` survives.
- Neither key is printed.
- A missing file, a missing key and the example's placeholders each return 1, export nothing and name the right file or key.
- A failure leaves keys already in the shell untouched.
- Nothing but the two exports survives the source, whether it succeeds or fails.
- Without `IIDP_STATE_TFVARS`, the script resolves `state-bucket/terraform.tfvars` next to itself. This is checked both when sourced by absolute path and when sourced as `../tofu-env.sh` from `infra/platform`, the README's own form. Both are checked through the missing-file message, so no file named `terraform.tfvars` is ever needed.
- Running the script instead of sourcing it exits 2.

The zsh cases run where zsh is installed and are skipped with a note elsewhere. The suite passes under macOS's bash 3.2 and under bash 5. Two deliberate breaks were each caught: dropping the zsh `$0` fallback, and leaving a helper function behind. Both the script and the test pass `shellcheck`.

**How CI runs it.** This machine's GitHub token lacks the `workflow` scope, so `.github/workflows/` can't be changed from here. `infra/tofu_env_test.go` therefore runs `test/tofu-env/run.sh` from `go test`, which the Go CI job already runs as `go test ./...`. It skips when bash is missing. The natural home for the suite is the `wizard` job in `.github/workflows/ci.yaml`: add `infra/tofu-env.sh test/tofu-env/run.sh` to its `shellcheck` step, and add a step that runs `bash test/tofu-env/run.sh`. Once that lands, the Go wrapper can go. Until then, the script is not shellchecked in CI.
