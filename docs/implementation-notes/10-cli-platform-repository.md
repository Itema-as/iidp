# #10 CLI: Platform repository writer and `app create` for a bare Web service

Questions that came up while building `iidp app create` and the Platform repository writer, and the answer chosen for each. The layout written is documented in [`docs/platform-repository.md`](../platform-repository.md).

## No `bootstrap/README.md` yet

The ticket's Platform repository layout and `platform.yaml` fields were to be taken from `bootstrap/README.md` if it existed on `main`. It does not (the bootstrap ticket has not landed), so the fields are the ones the spec names: `baseDomain`, `chartVersion`, `chartRepository` (default `oci://ghcr.io/<org>/charts/application`, where the release workflow pushes the chart), `argocdURL`, `grafanaURL`. Unknown fields are ignored so the bootstrap and later tickets can add theirs. `docs/platform-repository.md` is now the place the layout is written down for the CLI and the bootstrap alike.

## The `git` binary rather than go-git

Options: `github.com/go-git/go-git` in-process, or a small package (`internal/git`) shelling out to `git`.

Chosen: the binary. Every developer running `iidp` has git and `gh` installed already; the commits should carry the developer's own identity and, if configured, signature, which is what `git commit` does for free and go-git would have to reimplement; and the operations needed are four (shallow clone, add, commit, push). The package wraps those and nothing else. The cost is a process dependency the tests share: the CLI tests run real git against a local bare repository, which is what the spec's testing seam asks for anyway.

## How the token reaches git

Options: put the token in the clone URL, pass it as an `http.extraheader`, or configure a credential helper.

Chosen: a credential helper configured with `-c credential.helper=` (clearing any the developer has) and `-c credential.helper=!f() { echo username=x-access-token; echo "password=$IIDP_GIT_TOKEN"; }; f`, with the token in the child process environment. The token never appears on a command line or in the clone's `.git/config`. `GIT_TERMINAL_PROMPT=0` makes a refused credential fail instead of prompting. For `file://` remotes (the tests) the helper is configured and never consulted.

The token itself comes from `gh auth token` behind `github.TokenSource`, so tests inject a fake. A missing login is reported before anything is cloned, naming the Platform repository.

## Write access is checked by the push

The ticket asks for a clear error on missing write access. The GitHub API check (`GET /repos/{owner}/{repo}` and the `permissions` field) belongs with the fake GitHub API of #11; until then, the push is the check. A refused push is reported with git's output and a line saying to ask the Platform admin for write access to the Platform repository. Only a push rejected as non-fast-forward (`[rejected]` in git's output) triggers the retry; a `[remote rejected]` or a 403 does not.

## ArgoCD multi-source syntax

Checked against the current Argo CD documentation ("Multiple Sources for an Application" and "Helm"): the chart source carries `helm.valueFiles` entries prefixed with `$values/`, the git source carries `ref: values` and must not set `chart`; when it sets no `path` it is used solely for values; `$values` always resolves to the root of that source. The Application written follows that form exactly, with the OCI registry named as `repoURL` without the `oci://` scheme and the chart name in `chart`, the way the chart README already documented.

## Name and namespace of an Environment's ArgoCD Application

Options: name the ArgoCD Application after the Application (`shop`) and let both Environments share a namespace, which the chart's object naming (`shop`, `shop-staging`) allows; or name it `<name>-<environment>` and give each Environment its own namespace.

Chosen: `<name>-<environment>` for both, as the ticket instructions say. ArgoCD Application names must be unique in the `argocd` namespace across the whole Platform, and a namespace per Environment is what `iidp app delete` and a future per-Environment quota want. The chart's own object naming is unaffected. A namespace per Environment costs nothing on the TLS side: since #8 the wildcard certificate is Traefik's default certificate rather than a Secret in the Application's namespace, so a new namespace needs no secret replicated into it; `docs/platform-repository.md` says so.

## Finalizer and labels on the ArgoCD Application

`resources-finalizer.argocd.argoproj.io` is set so that deleting the ArgoCD Application (what `iidp app delete` will do by removing the file, with prune on) deletes the Environment's resources rather than orphaning them. The labels `iidp.itema.no/application` and `iidp.itema.no/environment` mirror the ones the chart puts on every object, so ArgoCD's list can be filtered the same way Grafana's can.

## The values file is written whole, with an empty image tag

Every value the chart documents is written, including those equal to the chart defaults (`size`, `port`, `probe.path`, `env: {}`), so the file is a complete description of the Environment and later commands have keys to edit rather than add. `image.tag` is written as an empty string: no image exists when the Application is created, and the deploy workflow's first run writes the tag. A placeholder like `latest` would have produced an `ImagePullBackOff` instead of the chart's clear "required value" failure. Each file starts with two comment lines saying what it is and that it is machine-written.

## `--kind` is required

Options: default `--kind` to `web-service`, the only Kind that works today, or require it.

Chosen: required. The wizard asks the Kind, and a default would silently choose one once `static-site` exists. `--kind static-site` is accepted as a value and refused with "not available yet", so scripts written now fail clearly rather than getting a Web service.

## `--yes` and the missing confirmation

The ticket says non-TTY runs without `--yes` must still work because no wizard exists yet. The flag is accepted and has no effect: the command prints what it is about to write and proceeds. When the wizard and its summary screen land, `--yes` is what skips the confirmation, and scripts that already pass it keep working.

## Name length: 40

The chart accepts up to 55 characters (a Service name must fit `<name>-staging` in 63). The CLI stops at 40 so that objects later tickets derive from the Environment name (Postgres cluster, migration Job, Ingress hosts on the base domain) also fit, and because a 40-character Application name is already unreadable in a URL. Raising it is a one-constant change in `internal/platformrepo`.

## The test seam

`cli.Run(args, stdin, stdout, stderr)` stays the production seam. Tests use `cli.RunWith(..., cli.Dependencies{TokenSource, BeforePush})`: `TokenSource` replaces the `gh` CLI, and `BeforePush` runs between the commit and each push attempt, which is how the tests move `main` behind the CLI's back for the retry and conflict cases (the ticket's recommended injectable callback). The hidden `--platform-repo` flag points the writer at `file://<bare repository>`; it is hidden rather than removed because it is also the honest way to try the command against a scratch repository.

The tests set `GIT_AUTHOR_*` and `GIT_COMMITTER_*` so commits work on CI runners without a git identity, and point `GIT_CONFIG_GLOBAL` at an empty file with `GIT_CONFIG_NOSYSTEM=1` so the developer's own configuration (commit signing, hooks, credential helpers) does not leak into the tests.

## `internal/render` has example tests

The rendered documents are asserted field by field through the CLI seam, as the testing decisions ask. The two `Example` tests in `internal/render` additionally hold the complete files as their expected output. They are the readable specification of the layout and fail on any change to it, which is the intended cost: the layout is a contract with the bootstrap and the deploy workflow.

## `gopkg.in/yaml.v3` is now in the binary

The chart notes recorded yaml.v3 as imported only by the chart tests. `platform.yaml` has to be parsed and the two files rendered, so the binary imports it now. No new dependency was added.

## What the review changed, and what it did not

A two-axis review (repository standards, then the ticket's acceptance criteria) was run before the pull request. Fixed: the GHCR namespace is now `platform.Registry`, derived from the org in the one package allowed to know it, instead of being spelled in two places; the CLI tests assert the Platform repository URL, name and registry through `internal/platform` rather than as literals; a failed `os.Stat` other than "not found" is an error instead of being read as "does not exist"; the files of an Environment are written in a fixed order; `LC_ALL=C` is set for git so the `[rejected]` match does not depend on the developer's locale; a second rejected push says that `main` moved twice and to run the command again; the name length check runs after the label check so it only ever counts ASCII; the required-flag test matches cobra's `"name" not set` and covers `--kind`; the commit author is asserted; and a refused push (simulated with a `pre-receive` hook, the way a permission denial reaches git) is tested to name the Platform repository and ask for write access.

Left as they were: the `Example` tests in `internal/render` spell the registry and Platform repository URL literally because they are the function's inputs in that test, not the compiled-in constants, and Example output must be literal; `render.Environment` mirrors `platformrepo.Application` field by field because `render` is the leaf package `platformrepo` imports, so it cannot take the latter's type; and the Data Clump the two make is the price of keeping the two documents' rendering free of git and file-system concerns.

## Package layout

`internal/git` (the binary wrapper), `internal/github` (the token source), `internal/render` (the two documents), `internal/platformrepo` (`platform.yaml`, name validation, the clone-write-commit-push-retry sequence) and the command in `internal/cli/app.go`. This follows the `wizard, github, platformrepo, render` list in `docs/design.md`; `git` is split from `platformrepo` because the Adopt path will need the same four operations on an Application repository.
