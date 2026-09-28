---
status: accepted
date: 2026-09-28
---

# Argus is a separate component that watches Kubernetes with client-go

Argus, the live view of the Platform, is its own component (`cmd/iidp-argus`, `bootstrap/components/argus`), not part of the Deploy gate's service. It learns about changes by watching Kubernetes directly with client-go's informers, over the dynamic client, and never through ArgoCD's API. It turns what it sees into Applications, Environments, Capabilities and Platform components with `internal/platformstate`, the same interpretation `iidp app status` uses, and streams them to browsers over server-sent events. It only reads: one ClusterRole with get, list and watch on ten resources, and nothing on Secrets, ConfigMaps or logs.

This is the first time the repository depends on client-go. Until now it has kept `go.mod` small and talked to the Kubernetes API with the standard library, as the Deploy gate still does.

## Considered options

- **ArgoCD's API.** It already knows every Environment's sync and health. Rejected: it needs a hand-minted token of at least `role:readonly`, which includes pod logs; Argus would go blind whenever ArgoCD is down, which is when a view of the Platform matters most; and its event stream drops events for a slow reader and replays nothing.
- **Inside the Deploy gate's service.** One component fewer. Rejected: the gate is the single write path and holds the `iidp-deploy` App's key, so it should stay small, and turning Argus off must never touch deploys.
- **A hand-written watcher** over plain HTTP, like the gate's reads. It would keep client-go out of the repository. Rejected: list, watch, resume and relist after a dropped connection or an expired resource version are exactly what informers get right, and a live view that silently stops updating is worse than none.

## Consequences

- `go.mod` gains `k8s.io/client-go`, `k8s.io/apimachinery` and `k8s.io/klog/v2`, at the version matching the node's k3s, and about 30 indirect modules. `k8s.io/api` is not among them.
- Only `cmd/iidp-argus` may import client-go. `TestOnlyArgusLinksClientGo` fails if the CLI, the Deploy gate or `internal/argus` link it, or if Argus links the typed clientsets. Their binaries are unchanged.
- Argus builds its informers from `tools/cache` directly rather than with `dynamicinformer`, whose one interface pulls in every typed clientset. The binary is 12.1 MiB instead of 26.4 MiB, linking 87 `k8s.io` packages instead of 393.
- Every informer has a transform that keeps only `platformstate`'s cut-down struct. With about 30 Environments the steady heap is 5.9–7.5 MiB, against 46 MiB without transforms, within the 64 MiB target and the 96 Mi limit.
- Upgrading k3s by a minor version means moving client-go with it.
- The Deploy gate records each Deploy it accepts or refuses as a Kubernetes Event in `argocd` (`DeployAccepted`, `DeployRefused`), so Argus sees a Deploy from its first moment rather than when ArgoCD notices the commit. That is one of the gate's two writes in the cluster (ADR-0007's notes); it creates Events and nothing else.
- Every signed-in member of the tenant sees every Application's state through Argus, including what `iidp app status` shows, as they already could through ArgoCD's read-only default. `iidp app status`'s repository check now guards only its own path (the note on ADR-0007).
