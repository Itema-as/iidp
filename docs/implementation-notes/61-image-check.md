# #61 The Deploy gate refuses image tags that don't exist

This note records the decisions taken while adding the image check to the Deploy gate ([60](60-deploy-gate.md), [ADR-0005](../adr/0005-private-application-repositories-on-github-free.md)'s fourth check). For each question it gives the options considered and the answer chosen. The gate's contract, now with the check as step 6, is in [`docs/platform-repository.md`](../platform-repository.md#how-a-deploy-reaches-the-platform-repository-the-deploy-gate). The registry behaviour described here was probed by hand against ghcr.io and Docker Hub on 2026-09-24, anonymously, since no `read:packages` token was available. Docker Hub's rate-limit rules were checked with Context7 (`/docker/docs`) the same day.

## What is checked, and where

The image is the Environment's `image.repository` from its `values.yaml`, plus the requested tag. The check runs in the callback `SetImageTag` already calls on each fresh clone of the Platform repository, right after the binding and ref checks. So it reads the same `values.yaml` the tag is written into, and a call the gate refuses for its token, its repository or its ref never makes the gate ask a registry anything. On the retry after a moved `main`, it runs again on the new clone, like every other check.

A tag the Environment already runs is not checked. Nothing will be committed, and `ci set-image` repeats a call it lost the answer to, so the repeat must stay harmless while a registry is down. Such a tag was either checked when it was committed or predates the check.

The code is `internal/registry` (the protocol, standard library only, like `internal/oidc`) and `internal/deploygate/image.go` (reading `values.yaml`, the credential files, and turning the outcome into a refusal). `gate.go` changes by one field on `Gate` and three lines in the callback. That keeps the change clear of #66, which adds the migration command to the same deploy call.

## The protocol

A `HEAD` on `https://<registry>/v2/<name>/manifests/<tag>`, the request a pull makes first. With no credential, both registries answer 401 with `WWW-Authenticate: Bearer realm="…",service="…",scope="repository:<name>:pull"`. The gate asks the realm for a token, with `service` from the challenge and always its own `repository:<name>:pull` scope, whatever the challenge says, so it never asks for more than reading. It then repeats the `HEAD` with `Authorization: Bearer <token>`. A `Basic` challenge gets the credential directly. The token answer's `token` or `access_token` is used.

The `Accept` header lists four media types: the OCI image index and image manifest, and the Docker manifest list and v2 manifest. The index types matter. `ghcr.io/linuxcontainers/alpine:latest` and Docker Hub's `nginx:1.27-alpine` both answered with `Content-Type: application/vnd.oci.image.index.v1+json`, and a registry answers 404 to a client that does not accept the type it holds, so a check accepting only manifests would call every multi-platform image missing. The fake registry in the tests does the same.

Image references are parsed the way container engines parse them. A first component with a dot or a colon, or `localhost`, is a registry host, and anything else is on Docker Hub. Docker Hub's API is at `registry-1.docker.io`, and a one-component name is under `library/`. That matters only for the e2e's fixture images (below): every real Application's image is `ghcr.io/itema-as/<application>`. The name must match the distribution specification's lowercase grammar and the tag the OCI tag grammar (already checked by #60), so nothing but a repository and a tag reaches the URL.

## Outcomes and status codes

| Outcome | Status | Message |
|---|---|---|
| The tag exists (200) | commits as before | |
| The registry has no such tag (404) | 422 | `refused: the image <repository>:<tag> does not exist, so nothing was deployed. Push it first (did the workflow's push fail?), or check the tag` |
| Unreachable, timed out (30 seconds for the whole check), 5xx, 429, or an answer the check does not understand | 503 | `couldn't check that the image <repository>:<tag> exists, so nothing was deployed: <what the registry did>. Run the deploy again once the registry answers` |
| The registry refuses the gate's access (401 or 403 on the token or the manifest) | 503 | `couldn't check …: <what the registry did>. Either <repository> was never pushed (GHCR answers a missing image like a private one), or the Platform's GHCR pull token cannot read it` |

**422 for a missing tag, not 404.** The gate's 404 already means the Application or the Environment does not exist, and #60's table says so. The request is well formed and authorised, and names an image that isn't there. `ci set-image` prints any status other than 502, 503 and 504 as the gate worded it, so the status only has to be distinct.

**503 for "couldn't check".** `ci set-image` already retries 503 twice, ten seconds apart, so a registry blip is absorbed without the developer noticing. When the retries are used up it prints the gate's message after its own "the Deploy gate at … is unavailable: HTTP 503:". That prefix is not quite right when it is the registry that is unavailable, but the message after it says what happened. Changing `ci set-image`'s wording is left alone because #66 is changing that file.

**A refusal of access is "couldn't check", not "missing".** Anonymously, ghcr.io's token endpoint answered `403 {"errors":[{"code":"DENIED",…}]}` for `itema-as/no-such-image-iidp`, which does not exist, exactly as it does for a private image it may not show. So after a first push that failed completely, GHCR does not say "missing". I could not see what it answers with a valid token for a package that does not exist, since I had none. So the gate does not guess: a denial is a refusal that names both explanations. A 404 from an authorised manifest request (a tag missing from a package that exists) is the case the ticket is about, and ghcr.io answered exactly that for `linuxcontainers/alpine:no-such-tag-iidp`.

## Where the pull token is sent

The credential is for one host, `ghcr.io` (`deploygate.GHCR`). The gate sends it as basic auth only to that host, and to a token realm only when the realm is `https` on that same host. ghcr.io's realm is `https://ghcr.io/token`, so that is enough. Every other registry, and a realm anywhere else, is asked anonymously. `values.yaml` is written by the CLI and the gate, never by hand, but a hand-edited `image.repository`, or a registry's challenge, still cannot send the Platform's token to another host. For images on ghcr.io the credential is always used, since the gate cannot know in advance whether a package is public. ghcr.io accepts a valid token for a public package too.

## How the gate gets the pull token: a Secret cloud-init writes next to `registries.yaml`

The node's copy of the token is k3s's `/etc/rancher/k3s/registries.yaml` ([59](59-ghcr-pull-token.md)). The gate is a pod in `argocd` and cannot read it. Options considered:

- **A new key in `argocd/platform-repo-github-app`,** the Secret the gate already mounts. Rejected. That Secret is ArgoCD's repository credential, labelled for ArgoCD, and its name and keys are already a contract with ArgoCD and the gate. A GHCR token there would be read by ArgoCD's repo server along with it, and rotating one credential would mean rewriting the other's Secret.
- **The wizard encrypting it into the Platform repository with SOPS,** applied by ArgoCD like other Platform secrets. Rejected. The token would have a second source of truth, in git, next to `terraform.tfvars`. Rotating it would need two recipes (tfvars and the node for the node's copy, `iidp secret` or the wizard for the gate's), and it would need a new wizard step and a KSOPS source for the gate's Application.
- **A `hostPath` mount of `registries.yaml`.** Rejected. The file is root-only (mode 600, as #59 requires) and the gate runs as a non-root user with a read-only root filesystem. A `hostPath` volume would also tie the gate to the node's filesystem and make the kind e2e fake a k3s file.
- **Chosen: cloud-init writes the Secret's manifest, and `iidp-bootstrap` applies it.** `local.ghcr_pull_secret_yaml` in `bootstrap.tf` renders the Secret `argocd/ghcr-pull-token` (keys `username` and `token`) from the same two OpenTofu variables as `registries.yaml`. cloud-init writes it to `/etc/iidp/ghcr-pull-token.yaml` (mode 600, root) with `write_files`, next to `registries.yaml`. `iidp-bootstrap` applies it with `kubectl apply --server-side` before the root Application, on every run, so the gate finds it when ArgoCD first installs it. The same local is the sensitive output `ghcr_pull_secret_yaml`. The rotation recipe in `infra/README.md`, "Adding or rotating the GHCR pull token", gains one step: pipe that output over ssh into the file and apply it. So rotation stays one recipe, with one `terraform.tfvars` edit and one `tofu apply` for both copies.

Details of the chosen option:

- **A file, not a script variable.** The App key's Secret is written from a base64 variable baked into the script. Doing the same here would put the token into `/usr/local/sbin/iidp-bootstrap`, which is mode 755. It would also make a later `iidp-bootstrap` run re-apply the first boot's token over a rotated one, unless the rotation also passed it in the environment. With a file that the recipe keeps current, a re-run always re-applies the current token. The script never reads the token itself. #59's test that the token never appears in the script still holds.
- **The live node's `iidp-bootstrap` is older and has no such step,** and cloud-init never runs again. That is why the recipe applies the Secret itself rather than re-running the script, the same reasoning #59 gave for `registries.yaml`.
- **Server-side apply,** so the token is not repeated in a `kubectl.kubernetes.io/last-applied-configuration` annotation. It is in the Secret's data anyway, but in plain text in the annotation.
- **The volume is not optional.** The gate refuses to start without the files (`IIDP_GATE_GHCR_DIR`, checked at startup like the App credential). Without them it could only check public images, and every private image would be refused as "couldn't check". A pod stuck in `ContainerCreating` with a `FailedMount` event naming `ghcr-pull-token` says what is missing sooner and more plainly. It starts on its own once the Secret exists. The files are read on every check, so a rotated token reaches a running gate when the kubelet refreshes the volume, with no restart.

## The kind e2e checks for real, against Docker Hub

The fixture's images are `docker.io/library/nginx`, which the kind node pulls from Docker Hub for shop's Environments anyway. The gate's check is therefore not exempted. It asks Docker Hub, anonymously, whether `nginx:1.27-alpine` exists before committing brochure's first deploy. `testDeployGate` also asserts that a tag Docker Hub does not have (`iidp-e2e-no-such-tag`) is refused with 422 naming `docker.io/library/nginx:iidp-e2e-no-such-tag`, before the real deploy. This exercises the anonymous path for a public image end to end: challenge, realm on another host (`auth.docker.io`), token, `HEAD`. No exemption setting exists, so the Platform has no switch that turns the check off.

The harness creates `argocd/ghcr-pull-token` with a placeholder token, because the gate does not start without it. The token is never sent: the gate sends it to ghcr.io only, and kind checks nothing on ghcr.io.

The cost is one more dependency on Docker Hub, which the e2e already has for its pulls. A `HEAD` is a version check, and Docker's documentation says version checks do not count as pulls, so the check does not use up the runner's anonymous pull limit.

The one local run on Podman before the pull request failed before it reached the gate. MinIO's images on quay.io had started to require authentication, which #73 fixed by replacing MinIO with versitygw. The e2e run on the pull request, on top of #73, is the proof that the check passes in kind.

## Tests

- **The gate** (`internal/deploygate/image_test.go`) is tested through its HTTP boundary against `fakeRegistry`: one TLS server standing in for every registry host, reached through a dialer the gate's HTTP client is given. `values.yaml` keeps the real `ghcr.io/itema-as/shop` and `docker.io/library/nginx`, and the host rules are the ones that run in production. The fake answers like GHCR and Docker Hub: a 401 challenge, a realm that gives a token for a public repository to anyone and for a private one only with the pull credential (403 DENIED otherwise), 404 for a tag it lacks, and 404 to a client that does not accept an index. The cases:
  - a private image: accepted and committed, the token request carrying the pull token as basic auth, the manifest `HEAD` carrying the bearer token, and only ghcr.io asked;
  - a missing tag, on a deploy and on a promotion: 422 naming the image and tag, nothing committed;
  - a public image on Docker Hub: accepted, the token fetched anonymously from `auth.docker.io`, the manifest asked of `registry-1.docker.io` at `library/nginx`, and no request carrying basic auth. A missing Docker Hub tag gets 422;
  - a challenge whose realm is on another host: no credential sent there, refused as "couldn't check";
  - unreachable, a 500 on the manifest, a 502 on the token, a 429, and a pull token the registry refuses: 503 "couldn't check", nothing committed;
  - a repeat of a tag the Environment already runs, with the registry unreachable: 200 and `unchanged`;
  - a call from another repository: refused without a single registry request.

  Every existing gate test now runs against the fake as well: all their tags exist.
- **The bootstrap** tests assert the Application hands the component `ghcrSecret: ghcr-pull-token`, and that the component mounts it, not optional, with only `username` and `token`, at the directory `IIDP_GATE_GHCR_DIR` names. They check the App credential's volume the same way.
- **cloud-init** (`infra/platform/registries_test.go`): both renders, the Go stand-in and real OpenTofu, now also assert one `write_files` entry for `/etc/iidp/ghcr-pull-token.yaml`: 0600, root, base64 that decodes to the v1 Secret `argocd/ghcr-pull-token` with exactly the given username and token. They also assert that `iidp-bootstrap` applies it before the root Application.

## What was not built

- **Checking the platform.** An index that lacks `linux/amd64` exists but would not run on the node. The gate checks existence, as the ticket asks, not what a pull would select.
- **Binding the tag to the token** (the tag equal to the commit on `main`), left open by [60](60-deploy-gate.md), is still not done.
- **Caching registry tokens.** A deploy costs three small requests. That is nothing next to the clone and push it guards.

## What #62 must do differently

Follow `infra/README.md`, "Adding or rotating the GHCR pull token", with its new step 5, **before** bumping the bootstrap to a release that carries this change. The gate's pod does not start until `argocd/ghcr-pull-token` exists. The issue lists the pull token (its step 3) after the bump (its step 2). That order still works, but `deploy-gate` shows `Progressing` until the token is added. `60-deploy-gate.md`'s "Moving existing Applications over" now lists the token first. #62's check that a made-up tag is refused is a 422 naming `ghcr.io/itema-as/hello:<tag>`.
