# #47 Wizard prompts: the GitHub App by hand, `~` in paths, links printed not opened

Items 3, 4 and 5 of #47, found while bootstrapping the real Platform in #19.

## The GitHub App is created from printed steps, not a manifest

**Problem.** GitHub rejected the manifest the wizard submitted ("`url` wasn't supplied", "`redirect_url` wasn't supplied"): it had no `redirect_url`, and its `hook_attributes` had `active: false` but no `url`.

**Options.**
1. Repair the manifest: add `redirect_url` and a placeholder hook `url`.
2. Replace the flow with printed manual steps.

**Choice.** Option 2, as the issue asks. The manifest flow only works as a browser form POST: the manifest is too large for a query string, so the wizard had to write a self-submitting HTML page and open it. That is exactly what item 5 forbids. The manual route is five fields on one form, so printing them costs the admin almost nothing. It also removes the question [`05-bootstrap-wizard.md`](05-bootstrap-wizard.md) answered about who calls the manifest-conversion endpoint.

`stage_github_app` now prints `https://github.com/organizations/<org>/settings/apps/new` and the values to enter:

- GitHub App name `iidp-deploy`
- Homepage URL: the Platform repository's URL, from its `origin` remote (normalised to https, no `.git`)
- Webhook: untick Active
- Repository permissions → Contents: Read and write, everything else left at No access
- Where can this GitHub App be installed: Only on this account

It then asks for the App ID, has the admin generate a private key and asks for its path, and prints the install page (`.../settings/apps/iidp-deploy/installations`) with the step to install the App on the Platform repository only (Only select repositories). That repository is the only one the App writes to (the deploy write-back) and the only one ArgoCD reads with it (#41). Last, it asks for the installation id from the URL GitHub lands on. What happens after those three answers (org secret, org variable, tfvars) is unchanged, as is the keep path for an existing `githubApp.id`.

`html_escape` existed only to put the manifest into that page's hidden field, so it is gone, with its test. `jq` stays a required tool, because the Cloudflare checks use it.

The install URL assumes the App got the slug `iidp-deploy`. GitHub App names are unique across GitHub, so if that name is ever taken, the admin picks another and the printed install URL is wrong. The step names the sidebar link it points to (Install App) for that case.

## `~` in path answers

**Problem.** `~/Downloads/iidp-deploy.pem` failed with "no such file": a prompt's answer is a literal string, and tilde expansion only happens on words the shell parses.

**Choice.** A new `ask_path` wraps `ask` and passes the answer through `expand_home`, which replaces a leading `~` or `~/` with `$HOME`. Every prompt that asks for a path uses it: both private-key prompts in `stage_github_app` (a new App, and the keep path's #41 key) and the SSH public key prompt in `stage_hetzner`. There are no other path prompts.

`~user/...` is left as typed. Expanding it needs either `eval` on the admin's answer or a passwd lookup, and nobody types another user's home directory into this wizard.

**Tests.** `expand_home` has unit tests for `~`, `~/…`, a `~` further into the path, and `~user`. A stage test answers the private-key prompt with `~/Downloads/iidp-deploy.pem`, with `HOME` pointed at a scratch directory holding that file. It asserts `PEM_PATH` is the expanded path, then turns fake mode off and runs `require_pem_file` on it, so the file is found and read. A second stage test covers the SSH public key prompt.

## Links are printed, never opened

**Problem.** `open_url` ran `wslview`, `explorer.exe`, `xdg-open` or `open`. The admin needs to choose the browser and profile, because personal and Itema logins sit side by side.

**Choice.** `open_url` is replaced by `print_url`, which prints the URL on its own line (`↗ <url>`, the URL in bold) and does nothing else, in every mode, so a dry run shows exactly what a real run prints. It was renamed rather than kept, because a function called `open_url` that doesn't open anything misleads the next reader.

Each stage prints its URL next to the step it belongs to:

- Hetzner, Cloudflare and Grafana: the same place as before.
- GitHub App: the new-App form, then the install page.
- Entra: the URL used to be opened only for the first of the two manual registrations (a seventh `entra_register_app` argument said which). It is now printed right under "In Entra ID > App registrations > New registration" for both, so the oauth2-proxy registration has its link too, including when the ArgoCD one was kept. The argument is gone.

**Test.** `test/wizard/run.sh` has a static check, `browser_openers`, next to `bash -n` and shellcheck. It drops comment lines and trailing ` # ` comments, then fails on any line that runs a known opener in command position: `open`, `xdg-open`, `wslview`, `explorer.exe`, `sensible-browser`, `x-www-browser`, `www-browser`, `gnome-open`, `kde-open`, `gio open`, `cmd.exe`, `powershell`, or `python -m webbrowser`. Command position means at the start of a line, after `;`, `&`, `|`, `(`, `{` or a backtick, or after `then`, `else`, `do`, `exec`, `command`, `env`, `nohup` or `-v`. It also fails on the marks of a self-submitting page (`onload=`, `.submit()`, `<form`). Run against the script before this change, it reports all four opener lines of the old `open_url` and the manifest page. Run against the new script, it reports nothing. A message that merely contains the word "open", like "Open the Platform's project", doesn't trip it, because the word isn't in command position. One that puts it right after an opening parenthesis would, and should be reworded.
