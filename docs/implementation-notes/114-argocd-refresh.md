# #114 The Deploy gate asks ArgoCD to refresh after each Deploy

This note records the decisions taken while making the Deploy gate ask ArgoCD to refresh an Environment's ArgoCD Application right after a Deploy or Promote commit. For each question it gives the options considered and the answer chosen. The contract is in [`docs/platform-repository.md`](../platform-repository.md#how-a-deploy-reaches-the-platform-repository-the-deploy-gate) ("After the commit"), the RBAC in [`bootstrap/README.md`](../../bootstrap/README.md#the-deploy-gate), and the amended decision in [ADR-0007](../adr/0007-app-status-reads-through-the-deploy-gate.md).

Sources, checked on 2026-09-27 against ArgoCD **v3.5.3**, the appVersion of the argo-cd chart `bootstrap/versions.yaml` pins (see [95-preview-environments.md](95-preview-environments.md)). Read at the tag in `argoproj/argo-cd`:

- `pkg/apis/application/v1alpha1/application_annotations.go`: `argocd.argoproj.io/refresh` "indicates that app needs to be refreshed. Removed by application controller after app is refreshed. Might take values 'normal'/'hard'".
- `util/argo/argo.go`, `RefreshApp`: ArgoCD's own refresh request, which its webhook handler (`util/webhook/webhook.go`) and `argocd app get --refresh` use. It is a JSON merge patch (`types.MergePatchType`) of `{"metadata":{"annotations":{"argocd.argoproj.io/refresh":"normal"}}}`. The gate sends the same bytes.
- `controller/appcontroller.go`, `needRefreshAppStatus`: a requested refresh of either type compares with `CompareWithLatestForceResolve`, so the branch `main` is resolved to its newest commit rather than the cached one.
- Context7 (`/argoproj/argo-cd`, master): the same, plus a newer `argocd.argoproj.io/refresh-timestamp` that v3.5.3 does not have (below).

## Refresh, not a webhook

ArgoCD polls the Platform repository every 3 minutes, so a Deploy waited up to 3 minutes before ArgoCD noticed it. There were two ways to shorten that:

- **A GitHub webhook to ArgoCD.** It needs ArgoCD's API reachable from GitHub and a webhook secret shared between GitHub and ArgoCD. It would also refresh on every push to the Platform repository, including the CLI's.
- **The gate asks for the refresh itself.** This was chosen. The gate already runs in the cluster with a service account, and it knows exactly which Environment it just changed. It needs no public endpoint and no new secret.

The patch goes only to the Environment the commit changed. A **normal** refresh is enough, because a new commit is exactly what it looks for. `hard` also throws away the manifest cache, which costs a full re-render and gains nothing here.

## Which ArgoCD Application: `<app>-<environment>`, by name

The gate patches `argocd/<app>-<environment>`, the name the CLI gives every Environment's ArgoCD Application (`render.Environment.Name`, used by `render.ArgoCDApplication`). The status endpoint finds ArgoCD Applications by label rather than by name, so that Preview Environments, whose names the ApplicationSet chooses, appear too. That is not needed here. The gate only deploys `prod` and `staging`, which the CLI always names this way, and looking them up by label first would cost one more call for every Deploy. If the naming ever changes, `render.Environment.Name` changes with it.

An Environment whose ArgoCD Application does not exist yet answers 404. That can happen with a first deploy within minutes of `iidp app create`, before ArgoCD has picked up the new `application.yaml`. The 404 is logged like any other failure, and ArgoCD applies the commit when it creates the Application.

## When, and what a failure does

- **Only after a commit.** The refresh is sent when the call made a commit, for Deploys and Promotes alike. It is not sent for a call that found the Environment already running the tag (`unchanged`), and not for a refused one.
- **After the Platform repository's write lock is released.** It is sent from `serveDeploy` once `deploy` has returned, so a slow API server never holds up the next deploy in the queue.
- **Not tied to the caller's connection.** It uses `context.WithoutCancel` with its own 10-second timeout. If CI hangs up after the push, the refresh is still sent.
- **Never fails the Deploy.** The commit is already pushed, and ArgoCD's poll still picks it up. A failure is logged as one `WARN` line, `argocd refresh failed`, with the Application, the Environment, the ArgoCD Application, the commit and the error. The answer to CI is the same 200 either way. The API server's refusal or an unreachable API server only brings back the 3-minute wait.
- **Without cluster access** (no service account token, or off the cluster) the gate skips the refresh. Its start-up warning now says deploys wait for ArgoCD's poll, as well as that `iidp app status` is unavailable.

## How it talks to the cluster: the status reads' connection, with the standard library

`deploygate.KubePatcher` wraps the `platformstate.Kube` that `iidp app status` reads with. That means the same API server address, the same CA, and the service account token re-read on every request. It sends one `PATCH` with `Content-Type: application/merge-patch+json`. It is about 30 lines, for the same reasons `Kube` is: `go.mod` keeps its five direct dependencies, and the gate that holds the App key stays small ([94-app-status.md](94-app-status.md)). It lives in `internal/deploygate/refresh.go`, not in `internal/platformstate`, which reads the cluster and writes nothing. `Gate.ArgoCD` is an interface (`Patcher`), so the deploy tests can stand in for the API server.

A merge patch sets the one annotation and leaves every other one alone. It carries no `resourceVersion`, so it cannot conflict with ArgoCD's own writes to the Application. That is why ArgoCD's `RefreshApp` retries on conflict and the gate does not need to.

## RBAC: `patch` on ArgoCD Applications in `argocd`, in its own Role

`bootstrap/components/deploy-gate/templates/rbac.yaml` adds a Role and a RoleBinding named `iidp-deploy-gate-refresh`. The Role grants `patch` on `applications.argoproj.io`, and nothing else. Because it is a Role, it applies only in the gate's own namespace, `argocd`. The binding is to the gate's service account alone. It is kept apart from `iidp-deploy-gate-status`, so the status Role stays list-only and matches its name.

- **Why not `resourceNames`?** It would limit the patch to named Applications, but the Environments come and go, and the chart has no list of them.
- **Why not `update`?** It would need a `get` first, and it could replace the whole object. `patch` is the narrowest verb that sets an annotation.

What a misused grant allows is patching any ArgoCD Application in `argocd`, the Platform's own included, which could change what it deploys. The gate's code only ever sends the refresh annotation. The gate also already holds the App key, which can rewrite the Platform repository those Applications come from, so the grant adds little to what a compromised gate could already do. ADR-0007's note records the change: the service is no longer read-only in the cluster.

`TestDeployGateOnlyWritesTheRefreshAndEventsInArgoCD` renders the component. It fails if any Role or ClusterRole grants anything beyond `list` other than this one `patch` and #117's `create` on `events.k8s.io` events, if the refresh Role holds any rule but that one (a `list` included), if anything narrows by `resourceNames`, or if the refresh binding names another role or subject. `TestDeployGateReadsTheClusterWithListOnly` still holds the status roles to `list` on their five kinds, and now expects the two new RBAC objects.

## A refresh asked for during another one

ArgoCD v3.5.3 removes the annotation when it finishes a refresh. Suppose a second Deploy of the same Environment sets it while the first refresh is still running. The merge patch is then a no-op, because the value is already `normal`, and ArgoCD may remove the annotation without having seen the second commit. That second commit then waits for the poll, as every Deploy did before. It takes two Deploys of one Environment within a second or two, which the gate's write lock and CI's build time make rare. ArgoCD's master branch adds `argocd.argoproj.io/refresh-timestamp` for this. When the Platform's ArgoCD has it, the gate can set it in the same patch.

## Tests

- **The deploy path** (`internal/deploygate/refresh_test.go`), through the gate's HTTP boundary with `fakeArgoCD` as `Gate.ArgoCD`:
  - A deploy from `main` to staging sends one patch for `shop-staging`, and a `v*` promote to prod then sends one for `shop-prod`. The test checks the path and the exact patch bytes.
  - A repeated deploy of the tag the Environment runs (`unchanged`), and a refused deploy, send nothing.
  - A patch that fails with the API server's 403 still answers 200 with the commit. The commit is on `main`, and the log has the `WARN` line naming `shop-prod` and the 403, followed by the usual `deployed` line.
- **`KubePatcher`** against a stand-in API server: `PATCH`, the path, `application/merge-patch+json`, the bearer token and the body; and a 403 surfacing as `platformstate.StatusError`.
- **The bootstrap** (`bootstrap_test.go`): the two RBAC tests above.
- Each deploy-path test was checked to fail with the refresh call removed, or sent unconditionally, and the RBAC test with `update` added to the refresh Role.

## What was not built

- **An end-to-end check in kind.** The e2e harness's deploys would exercise the real RBAC, but nothing there waits on ArgoCD's timing, so a missing grant would only show as a `WARN` line. A test that asserts the sync starts well before the poll would be slow and timing-dependent.
- **Waiting for the sync.** The gate answers once the refresh is asked for. Following the rollout is `iidp app status`'s job, and Argus's (#97).

## For the rollout (#96)

- Release, then bump the bootstrap. The bump creates `Role/iidp-deploy-gate-refresh` and its binding.
- Deploy hello. Its ArgoCD Application should show a sync of the new commit within seconds of the gate's `deployed` log line, and the gate should log no `argocd refresh failed`.
