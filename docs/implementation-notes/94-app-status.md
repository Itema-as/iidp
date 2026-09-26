# #94 `iidp app status` through the Deploy gate's service

This note records the decisions taken while building [ADR-0007](../adr/0007-app-status-reads-through-the-deploy-gate.md)'s read endpoint and the command that calls it. For each question it gives the options considered and the answer chosen. The endpoint's contract is in [`docs/platform-repository.md`](../platform-repository.md#how-iidp-app-status-reads-the-platform-the-gates-service), its RBAC in [`bootstrap/README.md`](../../bootstrap/README.md#the-deploy-gate), and the command and its `--json` shape in the README ("`app status`").

Sources, checked on 2026-09-26:

- ArgoCD (Context7, `argoproj/argo-cd`): `OperationState` in `pkg/apis/application/v1alpha1/types.go` (`phase`, `message`, `startedAt`, `finishedAt`, `syncResult`); an Application's `status.sync.status` and `status.health.status`, and that 3.0 no longer persists per-resource health by default (`upgrading/2.14-3.0.md`, which is why nothing here reads `status.resources`); `status.summary.externalURLs`, which ArgoCD fills from Ingress hosts (`user-guide/external-url.md`); multi-source Applications report `revisions`, not `revision`.
- Kubernetes (kubernetes.io, `batch/v1` Job and CronJob reference, v1.36): `JobStatus.conditions` with `Complete` or `Failed` at status `True` as the terminal conditions, `startTime`, and `completionTime`, which is set only on success; `CronJobStatus.lastScheduleTime`.
- GitHub: `GET /repositories/{id}` is not in the REST reference, but GitHub has served it for years and `gh` uses it. Seen on 2026-09-26 with a user token: 200 with `full_name` and `permissions.pull` for a private repository the token can read, 404 for an id it cannot see.

## A read endpoint on the gate's service, and the App key stays out of it

`GET /v1/status/<app>`, served by the same `internal/deploygate` handler, HTTP server, Ingress and pod as `POST /v1/deploy` ("reuse the gate's service", and no new pod on a node near its CPU request ceiling). It is authorised by the caller's own GitHub token, and everything the endpoint asks GitHub goes with that token:

- **The Platform repository is cloned with the caller's token.** The binding (`applications/<app>/repository.yaml`) is only in the Platform repository, which is private. The alternative was the App's installation token, which the deploy path already mints. It was rejected because ADR-0005's Phase 2 note says the read endpoint "doesn't use the App key", and ADR-0007 puts the cost of a status call on the developer's own rate limit, not the App's. Anyone could otherwise make the gate mint App tokens by calling it with any GitHub token.
- **The access check is `GET /repositories/<repositoryId>` with the caller's token**, by the binding's numeric id, so a rename or a transfer changes nothing and a repository recreated under the same name is not the bound one. GitHub answers 404, not 403, for a private repository the user cannot see. Both that and a 200 without `permissions.pull` are refused with 403 "you cannot read <app>'s Application repository". A 401 from GitHub (a revoked or expired token) is 401, "run gh auth login again". Anything else from GitHub is 502.

One consequence: the caller must be able to read the Platform repository too. That is narrower than ADR-0007's "anyone who can read an Application repository". At Itema it changes nothing, because every developer who uses the CLI already needs the Platform repository ([ADR-0002](../adr/0002-cli-writes-desired-state-to-platform-repository.md)), and every org member can read every repository. If that ever changes, the fix is to read the binding some other way, not to widen the check.

**The order of the checks** is: token, name, clone, Application, binding, GitHub, and only then the cluster. The cluster is never read for a refused call, and the tests assert it. Only someone who can read the Platform repository learns whether an Application exists or is bound, and they can read that in the repository anyway. The refusals are the four the ticket asks for: no access (403), no binding (403, naming `iidp app bind`), a binding outside the org (403), and an unknown Application (404). An unknown Application is one with no live `application.yaml`, the same test `iidp app bind` uses.

## Reading the cluster: plain list calls with the standard library, not client-go

`internal/platformstate.Kube` is about 80 lines. It builds `https://$KUBERNETES_SERVICE_HOST:$KUBERNETES_SERVICE_PORT` from the environment, trusts the service account's `ca.crt`, and sends the service account's token. The token is re-read on every request, because it is a projected token the kubelet rotates. It only lists, narrowed by label, and decodes the items into small structs of the fields it uses.

The Argus backend decision (#101) expected this endpoint to use "a plain client-go `List`", and its comment on this ticket put the cost at about 20 MB more image and more idle memory. The standard library was chosen:

- The gate holds the App key and is the Platform's one write path. Keeping it small is what #101 itself argued when it kept Argus out of it.
- Five list calls need none of client-go's list/watch/relist machinery, which is what made it right for Argus's informers.
- The repository hand-writes its JWT signing, OIDC verification and registry client for the same reason, and `go.mod` stays at five direct dependencies.

Argus can still share the model. The structs in `platformstate/objects.go` use the API's own JSON names, so an informer's transform can decode an unstructured object into them (`runtime.DefaultUnstructuredConverter.FromUnstructured`, or a JSON round trip). `EnvironmentOf` reads nothing itself; it turns one Environment's objects into its state, and it is the function to share.

**`internal/platformstate` is created here**, as the #101 comment asked ("whichever ships first creates it"). It holds the `Status` shape, the objects, `EnvironmentOf`, `Read` and `Kube`. Argus's Condition and Activity (#100) are not in it. They are Argus's to define, and this ticket shows ArgoCD's own sync and health instead, which is what the ticket asks for. When Argus adds them, `iidp app status` can print them from the same package.

If the gate has no service account token (off the cluster, or a pod spec without it), `InCluster` fails. The gate then logs a warning and keeps deploying, and answers status calls with 503. A broken read path never takes the write path down.

## Finding the Environments by label, so Preview Environments need no change here

`Read` makes one call for the ArgoCD Applications in `argocd` with the label selector `iidp.itema.no/application=<app>`. Each one is an Environment, whatever its name. Its namespace is the Application's `spec.destination.namespace`, and its name is:

1. the `iidp.itema.no/environment` label;
2. otherwise, the namespace without the `<app>-` prefix (`shop-pr-7` gives `pr-7`);
3. otherwise, the ArgoCD Application's own name.

prod and staging carry both labels, because the CLI writes them into every `application.yaml` (checked on the live Platform: hello's and iprofil's have them). **A Preview Environment (#95) appears with no change here as soon as the ArgoCD Application its ApplicationSet generates carries `iidp.itema.no/application: <app>` in `metadata.labels`.** The template should carry `iidp.itema.no/environment: pr-<n>` as well. Without the environment label the name falls back to `pr-<n>` from the namespace `<app>-pr-<n>`. The namespace label that #90 needs is not enough on its own: the lookup starts from the ArgoCD Application, not from namespaces. Namespaces were considered as the starting point, since #90 guarantees their label. That would need `list namespaces` cluster-wide plus a list of every ArgoCD Application to match them, and a namespace outlives its ArgoCD Application while it is being deleted. `TestStatusShowsEveryEnvironmentToAReaderOfTheRepository` includes a preview-shaped ArgoCD Application with no environment label.

Environments come back as prod, then staging, then the rest by name, with a trailing number compared as a number (`pr-9` before `pr-10`). An Environment the Platform repository has but the cluster does not yet (the minutes after `iidp app create`, before ArgoCD picks the new `application.yaml` up) is listed with `argocd: null`, rather than missing.

Inside each namespace, three lists are narrowed by `iidp.itema.no/application=<app>`: Deployments, Jobs and CronJobs. The chart has put that label on every object since its first version. Pods are listed with the Deployment's own `matchLabels`, so migration and task pods are not counted as the Application's. The Application's Deployment is the only one the chart renders; if there were ever more, the oldest is taken.

## What each field comes from

- **ArgoCD**: `status.sync.status`, `status.health.status`, and `status.operationState` (phase, message, start and finish) as the last sync. An empty value is `Unknown`.
- **Image**: the Deployment's first container's image, split into repository and tag (a digest counts as the tag). The ticket asked for the Deployment's, and it is what the pods run once a rollout finishes. During a rollout the pods count shows the difference.
- **Pods**: the Deployment's pods that are not `Succeeded` or `Failed`: those with a `Ready` condition, the total, and the sum of their containers' `restartCount`.
- **Migration**: the newest Job labelled `app.kubernetes.io/component: migration`. The chart's `BeforeHookCreation` keeps the last run's Job until the next sync. `succeeded` is `Complete`, finished at `completionTime`. `failed` is `Failed`, finished at that condition's `lastTransitionTime`, since `completionTime` is only set on success. Anything else is `running`.
- **Tasks**: each CronJob labelled `app.kubernetes.io/component: scheduled-task`, by its `iidp.itema.no/task` label, with `spec.schedule` and `status.lastScheduleTime`. Its last run is the newest Job with the same task label, judged the same way as the migration. The chart keeps the last successful Job and three failed ones (#91). `lastSuccessfulTime` is not shown; the last run's own result says more.
- **Addresses**: `status.summary.externalURLs`, which ArgoCD derives from the Environment's Ingress hosts, with one URL per host, https preferred. That avoids a read on Ingresses.
- **Links**: built by the service from `platform.yaml` in its clone. ArgoCD's is `<argocdURL>/applications/argocd/<ArgoCD Application>`. Grafana's is an Explore link with the LogQL `{namespace="<namespace>"}` against the data source uid `grafanacloud-logs`, over the last hour. `namespace` is a label the k8s-monitoring chart's pod-log collection sets. Building the links in the service keeps `--json` and the text output the same, and keeps them working when the CLI falls back to the default gate URL.

## When the image was deployed: the Platform repository's commit, not ArgoCD's history

The ticket left the choice open: whichever is reliable. The time shown is when the Platform repository's newest commit that set the Environment's `image.tag` to the tag the cluster runs was made. `platformrepo.TagDeployedAt` walks the history of that Environment's `values.yaml`, newest first, reading `image.tag` at each commit. It skips commits made after the running tag was replaced, because the cluster may not have synced a newer deploy yet. It then walks back through the commits that kept the tag, and the oldest of that run set it. So a size change after a deploy does not move the time, and neither does a staging deploy ArgoCD has not applied yet (`TestStatusShowsEveryEnvironmentToAReaderOfTheRepository` covers both).

ArgoCD's history was rejected:

- `status.history` records syncs and the revisions synced, not images. Any change to the Environment (a Capability, a secret, a chart bump) is also a sync, and telling them apart needs the Platform repository anyway.
- ArgoCD keeps only its last 10 entries.
- The Deployment's newest ReplicaSet was considered too. It is wrong for a redeploy of an earlier tag, because Kubernetes reuses the old ReplicaSet with its old creation time. Any other change to the pod template also makes a new one.

The commit time is when the Deploy gate recorded the deploy. ArgoCD applies it within its polling interval (3 minutes), and the sync status shows whether it has. A Preview Environment has no commit here (ADR-0006), so its `deployedAt` is absent. Its last sync time is still shown.

The history needs a clone with history, so the service clones the Platform repository in full (`git.CloneWithHistory`), not shallow as the deploy path does. The repository is 55 KB today. The walk stops after 100 changes to the file, and runs one `git show` per commit until it finds the tag, which is usually one or two. If the repository ever grows large enough for the clone to matter, `--shallow-since` or a cached clone that fetches per call are the next steps.

## RBAC: `list` only, on five kinds

`bootstrap/components/deploy-gate/templates/rbac.yaml` adds a ServiceAccount `iidp-deploy-gate` with `automountServiceAccountToken: false`. The pod opts in itself, since it is the only user. It also adds:

- a **Role** in `argocd`: `list` on `applications.argoproj.io`;
- a **ClusterRole**: `list` on `pods`, `deployments` (`apps`), `jobs` and `cronjobs` (`batch`).

Each has one binding, to that service account alone. Nothing is fetched by name or watched, so `get` and `watch` are not granted either. There is no `secrets`, `configmaps`, `pods/log`, `namespaces` or `ingresses`. `TestDeployGateReadsTheClusterWithListOnly` fails on any verb beyond `get`, `list` and `watch`, on any verb but `list`, on any other kind, on the ArgoCD Applications granted cluster-wide, on a binding to another subject, or on a binding to a role the chart does not render.

ADR-0007 listed "ArgoCD Applications, Pods, Jobs and CronJobs". Deployments are added because the ticket asks for the Deployment's image. The workloads are cluster-wide because Environment namespaces, previews' included, are created on first sync, and RBAC cannot select namespaces by label. Listing pods lets the gate read every pod's spec, including literal `env` values, in every namespace. The chart passes the Application's secrets as Secret references, so it does not see those.

The narrower alternative was a RoleBinding the application chart renders into each Environment's namespace, binding a ClusterRole to `argocd/iidp-deploy-gate`. It grants nothing outside Application namespaces. It was rejected for now for three reasons. It couples every Environment's chart to the gate's service account name. `iidp app status` would show nothing for an Environment until it moved to a chart version with the binding, and the live ones are pinned to older charts. And ArgoCD, which applies the chart, would need to be able to create RoleBindings in Application namespaces, which is a wider grant to tighten. If the cluster-wide read is ever a concern, this is the move.

## The CLI

`iidp app status <app> [--json]`, in `internal/cli/app_status.go`.

- **The gate's URL** comes from `platform.yaml`, `https://deploy.<baseDomain>` (`Config.DeployGateURL`, as `app create` renders into the workflow), read from a clone of the Platform repository with the gh token. If that read fails, the CLI says so on stderr and asks `platform.DefaultDeployGateURL`. The gate then refuses with its own reason if the token cannot see anything. A hidden `--platform-repo` points the clone elsewhere, as for `app bind`.
- **Errors.** Each one names what went wrong:
  - a 404 from the gate is "unknown Application <app>: <the gate's reason>";
  - a 403 is "no access to <app>'s status: <reason>", which covers both no access and no binding, with the reason saying which;
  - a failed connection is "the Deploy gate at <url> cannot be reached";
  - a 502, 503 or 504 is "unavailable";
  - an answer that is not the gate's own JSON is "does not answer status calls; it may be older than iidp app status", which is what a gate from before this change answers.

  Nothing is retried. A status call is cheap to re-run by hand, and a retry would hide an outage from someone checking for one.
- **The output** is one block per Environment, in the order the service sends them. Times are shown in UTC, so the output reads the same on every machine and in tests. `--json` re-encodes the service's answer, `platformstate.Status`, indented. The README documents it as add-only.
- **Tests route HTTP, not URLs.** `Dependencies.HTTPClient` lets the tests swap in a transport that sends every request to the fake gate and records the URL asked for. The test therefore sees the real URL the CLI built from `platform.yaml`, and the fallback's, without a hidden `--gate-url` flag and without any network.

## Tests

- **The service** (`internal/deploygate/status_test.go`) runs through the HTTP boundary. The test's fake GitHub now answers `GET /repositories/{id}` per token: readable, readable without pull, GitHub's 404, or 401 for an unknown token. `fakeCluster` is an in-memory stand-in for the list calls, with label-selector filtering. The tests cover:
  - A reader sees prod (Postgres, a migration, two tasks: one whose newest run failed, one still running; a ready pod with restarts; a pending pod; a migration pod that is not counted; `http` and `https` URLs for one host), staging (neither Postgres nor tasks), a preview, and not another Application.
  - `deployedAt` after a later size change, and while the cluster runs an older tag.
  - The links.
  - The JSON keys, including `migration: null` and `tasks: []` for staging.
  - That GitHub was asked with the caller's token and the App was never used.
  - Refusals: no token, a token GitHub refuses, a user who cannot see the private repository (404), one without pull, an unknown Application, an invalid name, no binding, a broken binding, and a binding to another org. Each asserts that the cluster was never read, and the binding cases that GitHub was never asked.
  - An Environment ArgoCD has not picked up yet, an unreleased one, and a gate without cluster access (503).
- **`platformstate`** (`kube_test.go`): `Kube` against a stand-in API server. It checks the bearer token, that the token is re-read per request, the label selector, decoding, and an API refusal surfacing from `Read`. It also checks the Environment order.
- **The CLI** (`internal/cli/app_status_test.go`), through `cli.RunWith` against a fake gate:
  - The exact table output: a failed task run, a task not yet run, several addresses, links, a failed sync with its message, an Environment without an image, and one ArgoCD has not picked up.
  - `--json`, which round-trips to the gate's answer and has the documented keys.
  - Each error: no access, no binding, an unknown Application, an unreachable gate, a 503, and an old gate.
  - No login and an invalid name, with no call made.
  - The fallback URL when `platform.yaml` cannot be read.
- **The bootstrap** (`bootstrap_test.go`): the pod's service account and its mounted token, and the RBAC test above.
- **The kind e2e** (`testAppStatus`) runs after `testMigrationCommandFromIidpYAML`. It calls `GET /v1/status/shop` through Traefik with a developer token, which fakegithub (`test/e2e/testdata/fakegithub`) lets read shop's repository (id 700000002) and nothing else. It sees shop-prod Synced and Healthy with a ready pod, the tag the previous test deployed with a `deployedAt`, the migration succeeded, the task's run, and the addresses. The same token asking for brochure is refused with 403, because fakegithub answers 404 for its repository as GitHub does for a private one. The gate reads the cluster through the RBAC above, so a missing grant fails here. The e2e workflow now also runs when `internal/platformstate` changes.

## What was not built

- **Logs** (`iidp app logs`). ADR-0007 rejected this for now.
- **Argus's Condition and Activity.** They come with #97, in the same package.
- **Caching.** Each call clones the Platform repository and makes one GitHub call and up to 1 + 4 per Environment list calls. That is small next to a deploy, and a cache would need invalidation on every deploy.
- **A `--watch` mode.** It was not asked for, and `watch iidp app status shop` works.

## For the rollout (#96)

- Release, then bump the bootstrap. The bump creates the ServiceAccount, Role, ClusterRole and bindings and restarts the gate with its token mounted. The gate's first log lines show no "iidp app status is unavailable" warning when it has cluster access.
- hello and iprofil need nothing. Their ArgoCD Applications already carry the labels, and both are bound. Check with `iidp app status hello` from a login that can read hello's repository, and with one that cannot.
- Check that the Grafana link opens Explore on the logs. The data source uid `grafanacloud-logs` and the Explore URL's `panes` format are Grafana Cloud's defaults as documented, but were not tried against Itema's stack (`itema.grafana.net`).
- #95: the ApplicationSet's template should set `metadata.labels` `iidp.itema.no/application: <app>` and `iidp.itema.no/environment: pr-<n>` on the generated Applications (above).
