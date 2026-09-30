# #128 The kind e2e job's image cache

Decisions taken while stopping the kind e2e job (`kind bootstrap`, `.github/workflows/e2e.yaml`) from pulling images from rate-limited registries. The code is `test/e2e/images.go`.

## The problem

ArgoCD's Redis image, `ecr-public.aws.com/docker/library/redis:8.6.4-alpine`, is a default of the argo-cd chart that `bootstrap/versions.yaml` pins. Pulls of it from ECR Public were refused with `429 Too Many Requests` three times on 2026-09-27 and 2026-09-28, and twice more during the Phase 2 rollout. Without Redis, ArgoCD's repo server times out on every manifest, and the run gives up after ten minutes. GitHub-hosted runners share their addresses, so strangers spend the same anonymous limit, and every kind run pulled the image fresh.

## Which images the job pulled, and from where

The harness now logs, at the end of every run, which images the node pulled from a registry and which it found already present, read from the kubelet's `Pulled` events (`LogImageSources`). A run of `main`'s harness (run 36583236516, before this change) showed the node pulling these:

| Registry | Images | Covered |
|---|---|---|
| ECR Public | `ecr-public.aws.com/docker/library/redis:8.6.4-alpine` (ArgoCD) | yes |
| Docker Hub | `rancher/mirrored-library-traefik:3.7.8` (Traefik, as k3s pins it) | yes |
| Docker Hub | `viaductoss/ksops:v4.5.1` (the repo server's KSOPS init container) | yes |
| Docker Hub | `docker.io/grafana/alloy:v1.19.2` (the monitoring collectors) | yes |
| Docker Hub | `docker.io/library/nginx:1.30-alpine`, `docker.io/library/nginx:1.30.0-alpine` (the fixture Applications) | yes |
| Docker Hub | `kindest/node:v1.36.4@sha256:099e…` (the kind node itself, pulled by `kind create cluster` on the runner) | yes |
| quay.io | ArgoCD, cert-manager, oauth2-proxy, node-exporter, prometheus-config-reloader | no |
| ghcr.io | Dex, CloudNativePG and its Barman Cloud plugin, Postgres, the Alloy Operator, versitygw | no |
| registry.k8s.io | external-dns, kube-state-metrics | no |

Events from namespaces the test deletes, such as `shop-staging` and the preview's, are gone with them. The images those namespaces ran also run elsewhere in the fixture, so the list is complete for what the node pulls. The kind node image isn't in it: the runner's engine pulls it before the node exists, so no kubelet sees the pull. It is the largest image in the job, about 870 MB unpacked.

Covered means Docker Hub and ECR Public, the two registries with an anonymous pull limit that runners have hit. quay.io, ghcr.io and registry.k8s.io have not refused the job, and caching their images would roughly triple the archive. If one of them starts refusing, adding its host to `rateLimitedRegistries` covers the images the harness already reads from the charts.

Three other registry calls stay in the run:

- **The chart downloads.** helm fetches the argo-cd, Traefik and k8s-monitoring charts, and ArgoCD fetches the Platform components' charts. Those come from chart repositories and GitHub, not an image registry.
- **The harness's own image builds.** The Deploy gate, fakegithub and the git server are built on the runner from `docker.io/library/alpine:3.22`. The runner's engine pulls that base image, and so far it has never been refused.
- **The Deploy gate's image check.** The gate asks Docker Hub whether brochure's nginx tag exists (#61), by design. That is a manifest lookup, not a pull, and preloading can't replace it. The `v0.3.2` tag's run failed on a gate call that got no answer within two minutes, and nothing showed why. A gate call that fails without an answer, or with an answer that isn't JSON, now prints the tail of `argocd/iidp-deploy-gate`'s log. The gate logs every call it finishes, so a call with no line of its own never finished.

## How the images get into the node

The kind node image comes first, in `Create` (`ensureNodeImage`). kind pulls the node image unless `docker inspect --type=image <name>` finds it, and after `save` and `load` Docker's classic image store no longer resolves a reference by digest. So the harness pulls the pinned reference by its digest, tags it `iidp-e2e.local/kind-node:sha256-<first 16 digits of the digest>`, caches it under that name, `docker load`s it on a warm cache, and hands kind the local name. The digest in the name ties the cached image to the pin, and moving the pin gives a new name and a new archive.

`TestBootstrap` then calls `PreloadImages` right after creating the cluster, before anything that would pull. For each image:

1. If the cache holds its archive, `kind load image-archive` loads it, and the harness logs `image cache: loaded <image> from <archive>, with no registry pull`.
2. Otherwise the container engine on the runner pulls it for the node's platform, retrying a refused pull. `docker save --platform linux/<arch>` (or `podman save`) writes it to the cache, and `kind load image-archive` loads it.

Every Pod asks for these images by tag with `imagePullPolicy: IfNotPresent`, so the kubelet uses the loaded image (`Container image ... already present on machine`). `TagNodeImage`, which names nginx after a preview's head SHA, no longer runs `crictl pull` first when the node has the image, because `crictl pull` asks the registry even then.

- **Pulled on the runner, not in the node.** Pulling in the node with `crictl pull` and exporting with `ctr images export` was tried first, but kind's containerd discards the compressed layers after unpacking, and the export fails with `content digest ... not found`.
- **One platform in the archive.** With Docker's containerd image store, `docker save` exports every platform of a multi-platform image and fails on those it didn't pull. `--platform` limits it to the one pulled. Podman only ever pulls one.
- **By tag, not digest.** The issue asked for a digest pin where the chart has one. The argo-cd chart 10.9.2 names Redis by tag only (`redis:8.6.4-alpine`), and so do the other charts for their covered images, so the archive carries the tag the Pod asks for. The only digest pin among them is the kind node image's, handled above.
- **A moving tag stays where the cache caught it.** `nginx:1.30-alpine` is a moving tag: the cache keeps the build it first pulled until the list of images changes or the cache is evicted (GitHub drops a cache unused for seven days). The fixture only needs some nginx 1.30 on Alpine, and a fixed build makes runs more alike. The Deploy gate's image check still asks Docker Hub about the live tag, which only has to exist.

## Where the image references come from

Nothing here names an image version that `bootstrap/versions.yaml` or a chart already pins (`RegistryImages`):

- **Redis**: the argo-cd chart rendered exactly as `InstallArgoCD` applies it (`argoCDManifest`). A bump of `argocd.chart` moves Redis with it.
- **Traefik**: the k3s Traefik chart rendered with the pinned `k3s.traefik.image`.
- **KSOPS**: `ksops.image`.
- **Alloy**: the Alloy Operator picks Alloy's image when it runs, so the image isn't in the rendered k8s-monitoring chart. The chart pins the tag it expects the operator to run (`collector.alloy.pinnedImageTag` in `templates/collectors/_collector_version.tpl`, `v1.19.2` at chart 4.5.2, the tag the node pulled). The harness pulls the pinned chart, reads that tag, and adds the operator's default repository, `docker.io/grafana/alloy`. The tag lives in an internal file of a third-party chart, so a release may move it. If it does, the kind job logs `image cache: leaving Alloy to the node` and the node pulls Alloy as before, rather than a caching detail failing the run. `TestRegistryImagesFollowThePinnedCharts` is what fails, in the lint job. That the operator runs the tag the chart pins is inferred from the chart's own comment and from the census, where the node ran exactly that tag.
- **nginx**: the fixture's tags, listed next to the tests that deploy them (`fixtureImages`).

`TestRegistryImagesFollowThePinnedCharts` renders the pinned charts and checks for Redis, Traefik, KSOPS and Alloy. It fetches the charts from their repositories, so it runs only when `IIDP_REQUIRE_CHART_TOOLS` is set, as it is in the e2e workflow's lint job. A plain `go test ./...` with helm installed doesn't need the network for it.

A new image from a rate-limited registry, say from a chart bump, isn't caught: the node pulls it as before, and the run's "pulled from a registry" list shows it. Failing the run for that would fail it for the harness's coverage, not for the product.

## Retries

A pull that fails with a rate limit (`429`, `toomanyrequests`), a 5xx or a network error (a timeout, a connection reset or refused, an unexpected EOF) is retried after 10, 20, 40, 80 and 80 seconds: six attempts over about four minutes. That's long enough for a per-second or per-minute limit to clear, and well inside the job's 25 minutes. Any other failure, such as a tag that doesn't exist, fails at once. `TestRetryPullRetriesARateLimit` feeds the retry loop the ECR 429 from the issue twice and checks that the third attempt gets through.

The retries only get a cold cache past a short refusal. In the failed runs from the issue, the kubelet kept retrying Redis for ten minutes and was refused throughout, which is longer than these four minutes. What keeps the job off the registry is the cache: a cold cache only happens after an image list change or an eviction. A longer backoff would mostly move the failure later in a 25-minute job.

## The cache in CI

`actions/cache/restore` restores `.e2e-image-cache` (`IIDP_E2E_IMAGE_CACHE`) before the test, and `actions/cache/save` saves it after.

- **Keyed on the image references.** Once every image is in the node, `PreloadImages` writes `images.txt`, one archive name (the reference plus the platform) per line, and removes archives no longer on the list. The save step's key is `e2e-images-<hash of images.txt>`.
- **Restore the newest.** The restore step's primary key never matches (`e2e-images-run-<run id>`), so it restores the newest `e2e-images-` cache that this ref or `main` can see. Images still on the list load from it, and images that changed are pulled.
- **Save only when the list changes.** The save step is skipped when the restored key already equals the new one, which is most runs. A bumped pin changes the list and gets a new cache. That cache is saved even when the test fails later, because the images are good either way.

Pull requests restore `main`'s caches, and the caches they save are only visible to that pull request, so a pull request that bumps a pin pulls the new images once per branch until `main` saves them. The cache is 733 MB compressed with the kind node image (371 MB without it), well within GitHub's 10 GB per repository. Unset, `IIDP_E2E_IMAGE_CACHE` means a temporary directory that is removed after the run, so a local run pulls fresh but still gets the retries.

## Proof

The first three runs are of the commit before the kind node image was cached.

- **Cold cache** (run 36585090874, first attempt). `Cache not found for input keys: e2e-images-run-36585090874, e2e-images-`. The harness pulled all six images on the runner, each on the first attempt, in under two minutes. It saved `e2e-images-167180040e4a…` (371 MB), and `TestBootstrap` passed in 675 s. The node pulled nothing from Docker Hub or ECR Public: its "pulled from a registry" list holds only ghcr.io, quay.io and registry.k8s.io images.
- **Warm cache** (the same run, second attempt). `Cache restored from key: e2e-images-167180040e4a…`. The log has one `image cache: loaded <image> from <archive>, with no registry pull` line per image, Redis's included. Redis, Traefik and KSOPS are in the node's "found already present" list, and `Save the image cache` was skipped. `TestBootstrap` passed in 710 s.
- **Locally** under Podman (`KIND_EXPERIMENTAL_PROVIDER=podman`, arm64), `TestBootstrap` passed in 668 s with a cold cache directory. That run predates Alloy's addition, and Alloy was the only Docker Hub image the node still pulled.
- **Retries.** No 429 came up in these runs. `TestRetryPullRetriesARateLimit` covers the cold-cache path through two 429s, and `TestTransientPullError` covers which failures are retried.
- **The kind node image** (run 36721472254, the commit that caches it). The run restored the six-image cache, pulled only `kindest/node` (first attempt), created the cluster from `iidp-e2e.local/kind-node:sha256-099e049362a1526b`, loaded the other six, and saved `e2e-images-413cf0ff121b…` (733 MB), because the list had grown. `TestBootstrap` passed in 624 s. On its second attempt, all seven images loaded with no registry pull, the kind node's included, the node's "pulled from a registry" list had nothing from Docker Hub or ECR Public, the save step was skipped, and `TestBootstrap` passed in 575 s. Locally under Podman, a cache seeded with the node image's archive gave the same `loaded … with no registry pull` for it, and `TestBootstrap` passed in 546 s.
- **A chart bump.** `TestRegistryImagesFollowThePinnedCharts` reads Redis, Traefik, KSOPS and Alloy from the charts at the versions `bootstrap/versions.yaml` pins, so moving a pin moves the images, and the cache key with them.
