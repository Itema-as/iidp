# #81 The vite-react template serves with nginx's stable branch

Decisions taken while moving the vite-react template off `nginx:1.27-alpine`. Sources, checked on 2026-09-26:
- Docker Hub's tag API (`hub.docker.com/v2/repositories/library/nginx/tags/<tag>`);
- GitHub's Dependabot options reference and its "Optimizing the creation of pull requests" tutorial (the `github/docs` sources);
- dependabot-core's Docker ecosystem at `main`: `file_parser.rb`, `update_checker.rb`, `tag.rb`, `version.rb` and `requirement.rb`.

## The tag: `nginx:1.30-alpine`

nginx rebuilds only its current stable (even minor) and mainline (odd minor) branches. Docker Hub on 2026-09-26:

| Tag | Last pushed | Digest |
|---|---|---|
| `1.27-alpine` | 2025-04-16 | `sha256:65645c7b…` |
| `1.28-alpine` | 2026-03-25 | `sha256:a8b39bd9…` |
| `1.30-alpine` | 2026-09-23 | `sha256:98522025…` |
| `stable-alpine` | 2026-09-23 | `sha256:98522025…` |
| `1.31-alpine` | 2026-09-26 | `sha256:df221db8…` |
| `mainline-alpine` | 2026-09-26 | `sha256:df221db8…` |
| `1.32-alpine` | not found | |

`1.30-alpine` has the same digest as `stable-alpine`, so 1.30 is still the stable branch, as the issue said. The image built from the rendered template runs nginx 1.30.5 on Alpine 3.24.2.

**Pinned to the branch, not `stable-alpine`.** The moving tag would follow each new stable branch with no template change. It would also change the minor version under every Static site on its next build, with no pull request and nothing in the Application's history to show it. A branch pin still gets every patch and Alpine fix, because nginx rebuilds `1.30-alpine` for as long as 1.30 is stable. Moving to the next branch then shows up as a one-line change: here through Dependabot (below), and in an Application through its own Dockerfile. The Dockerfile now has a comment saying the pin is a stable branch that has to move, so an Application's owner knows the line needs attention after the template has handed it over.

## Other occurrences

Updated so the fixtures match what the template generates:
- `test/e2e/fixtures/platform-repo/applications/shop/{prod,staging}/values.yaml`: `1.30-alpine`.
- `test/e2e/bootstrap_test.go`: brochure's deploys and the refused calls use `1.30-alpine`, and so does the commit subject the test expects. brochure keeps the fixture's tag so the kind node already has it from shop. `shopDeployTag` is now `1.30.0-alpine`. It has to be a different tag from the fixture's so the Deployment changes, and it has to exist on Docker Hub because the gate checks it. It exists (pushed 2026-04-17). It's a frozen patch tag, which is fine for a test that only needs a real tag that differs from the fixture's.
- `internal/deploygate/image_test.go`: `1.30-alpine`. The test runs against a fake registry, so only the tag in the request path changed.

Left as they are, because they record what was true when each ticket was done:
- `09-e2e-fixture-application.md`: why `nginx:1.27-alpine` was chosen as the fixture image. The reasons (busybox `sh`/`nc`, 200 on `/`) hold for 1.30.
- `11-cli-create-path.md`: a timing measured with `nginx:1.27-alpine`.
- `61-image-check.md`: the media type Docker Hub answered for `nginx:1.27-alpine`, and the tag the e2e checked at the time.
- `66-migration-command-in-repo.md`: the e2e's deploy tag at the time, `1.27.0-alpine`.

Rewriting them would make them say something that never happened. Their code now uses the tags listed above.

## Dependabot

`.github/dependabot.yml` adds weekly version updates for the `docker` ecosystem in `/internal/templates/*` (a glob, which `directories` supports), `/test/e2e/gitserver` and `/test/e2e/testdata/fakegithub`.

What it actually tracks. The parser reads `FROM` lines with a literal tag or digest, and skips a line whose tag is a build argument:
- vite-react's `nginx:1.30-alpine`;
- both e2e images' `docker.io/library/alpine:3.22`.

It does not track:
- `node:${NODE_VERSION}` in nextjs and vite-react. The `ARG` default `24-slim` is Node's LTS major, which Docker Hub rebuilds, so it doesn't go stale the way a dead nginx branch does;
- the "other" stub, whose `FROM` lines are comments.

The glob still covers every template, so a future template with a literal base tag is tracked without a config change. The docker ecosystem also reads Kubernetes-style YAML in those directories, and there is none there. The Platform fixture's `values.yaml` files, which split `repository` and `tag`, are left out. They're fixtures, not something Dependabot should bump.

**nginx's odd minors are ignored.** Dependabot's docker update checker keeps a tag's suffix (`alpine`) and its precision (two segments), then proposes the highest version among the matching tags. From `1.30-alpine` it would propose `1.31-alpine`, the mainline branch, today. An `ignore` rule for `nginx` lists the odd branches 1.31 to 1.39 as ranges (`">= 1.31, < 1.32"`, …). Docker ignore versions use Bundler requirement syntax, and dependabot-core's docker `Requirement` splits a comma-separated string into one requirement. A range, rather than a bare `1.31`, also catches `1.31.x` if the template is ever pinned to a patch. With that rule the next pull request is `1.32-alpine`, the next stable branch, due around April 2027.

Other options considered:
- `update-types: ["version-update:semver-minor"]` would ignore 1.32 too, and then Dependabot would never propose anything for a two-segment tag;
- Renovate can match versions with a regex (`allowedVersions: "/^1\\.\\d*[02468]-alpine$/"`), so it would need no list. It's another app to install on the org for a single rule.

The cost of a list: it runs out after 1.39, around 2030. The comment in the config says to extend it.

**Grouped by dependency name.** gitserver's and fakegithub's Dockerfiles share one Alpine base on purpose (fakegithub builds from the base the git server already pulled). `group-by: dependency-name`, which is generally available on github.com, bumps both in one pull request instead of one per directory.

`cmd/iidp-deploy-gate/Dockerfile` (the gate's release image, which the harness also builds from) also pins `alpine:3.22`. It was left out, since the issue is about the templates' base images. Adding `/cmd/iidp-deploy-gate` to `directories` would bring it into the same Alpine pull request.

Not verified: a Dependabot run. The config only takes effect once it's on `main`. The `dependabot` CLI (github.com/dependabot/cli) could dry-run it locally; I didn't try it. The expectations above come from reading dependabot-core's source. The first weekly run will show it in the repository's Dependabot tab (Insights → Dependency graph → Dependabot). The expected first pull requests: Alpine for the two e2e images (3.22 is behind), and nothing for nginx until 1.32 exists.

## Applications already generated

Create and Adopt never rewrite an Application's `Dockerfile`, and a release doesn't reach it the way it reaches the deploy workflow. A Static site generated before this change keeps `FROM nginx:1.27-alpine` until someone changes that one line to `FROM nginx:1.30-alpine` in its repository. The README says so after the paragraph on the reusable deploy workflow, which is the place that says what a release does and doesn't change in an Application.

Which Applications are affected, as far as I could check:
- `hello` is Next.js, so it's not affected;
- `iprofil` already uses `nginx:1.30-alpine` (Itema-as/iprofil#14);
- GitHub code search across `Itema-as` for `nginx:1.27` finds only this repository. The index can lag, so a repository created or adopted very recently might not show up.

## Proof

- `go vet ./...` and `go test ./...` pass. That includes the `deploygate` test asking the fake Docker Hub for `library/nginx/manifests/1.30-alpine`.
- The template rendered with `cmd/renderfixture` builds with Podman. The container answers 200 on `/`, and `nginx -v` reports 1.30.5 on Alpine 3.24.2. CI's Templates job repeats the build and the curl.
- The shop fixture's migration command relies on busybox `nc -z -w 5` in the image. It works in both new tags: `1.30-alpine` (Alpine 3.24.2) and `1.30.0-alpine` (Alpine 3.23.4), each checked with `podman run`.
- The kind e2e (`TestBootstrap`), which pulls the new fixture tags from Docker Hub, was not run locally. The e2e workflow runs it on this pull request, because `test/e2e/**` changed.
