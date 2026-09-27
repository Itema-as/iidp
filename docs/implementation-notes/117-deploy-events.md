# #117 The Deploy gate records Deploys and refusals as Kubernetes Events

This note records the decisions taken while making the Deploy gate record each Deploy or Promote it accepts or refuses as a Kubernetes Event, the first piece of Argus ([#115](https://github.com/Itema-as/iidp/issues/115), designed in [#106](https://github.com/Itema-as/iidp/issues/106) and its correction). For each question it gives the options considered and the answer chosen. The Event's contract is in [`docs/platform-repository.md`](../platform-repository.md#how-a-deploy-reaches-the-platform-repository-the-deploy-gate), and its RBAC in [`bootstrap/README.md`](../../bootstrap/README.md#the-deploy-gate).

Sources, checked on 2026-09-27 (Context7, `kubernetes.io` and `kubernetes/kubernetes`):

- The `events.k8s.io/v1` Event reference. `eventTime` (a MicroTime), `action`, `reason`, `reportingController`, `reportingInstance` and `type` cannot be empty for a new Event. `action`, `reason` and `reportingInstance` are at most 128 characters, and `note` at most 1 kB. `type` is `Normal` or `Warning` since v1.
- The API server has refused an Event whose namespace differs from its object's since 1.4 (CHANGELOG-1.4, kubernetes#30533).
- `pkg/registry/core/event/strategy.go`: the selectable fields include `involvedObject.name`, `reason`, `type` and `reportingComponent`, the core name for `reportingController`.

Not confirmed from a source: that `eventTime` must have exactly six fractional digits (the gate sends exactly that, as metav1.MicroTime writes it), and the rest of the API server's validation. The Event the gate builds passes `kubeconform -strict` against the v1.33 schema, and the kind e2e checks it against a real API server.

## An `events.k8s.io/v1` Event in `argocd`, regarding the Environment's ArgoCD Application

As #106 decided, corrected: the Event's `regarding` is the ArgoCD Application `<app>-<environment>` in `argocd`, so the Event is in `argocd` too. The name is `render.Environment.Name()`, the one the CLI gives every Environment's `application.yaml`, so the gate does not read it back from the clone. `regarding` has no `uid`: the gate would need a `get` on the ArgoCD Application to learn it, one more request and one more permission per Deploy. Argus matches on the annotations and the name, not the uid. `kubectl get events -n argocd --field-selector involvedObject.name=shop-prod` shows them. `kubectl describe application` may not, if it filters by the object's uid (not checked).

- **`reason`**: `DeployAccepted` or `DeployRefused`. **`type`**: `Normal` for accepted, `Warning` for refused, so a refusal is among the Warnings Argus seeds its feed with after a restart (#115).
- **`action`**: `Deploy` or `Promote`, which the API requires; the `iidp.itema.no/kind` annotation says the same in lower case, for machines.
- **`reportingController`**: `iidp.itema.no/deploy-gate`. **`reportingInstance`**: the pod's hostname, which is its name.
- **`note`**: `<Deploy|Promote> <app> <environment> <tag> accepted`, or `<Deploy|Promote> <app> <environment> refused: <reason>`, where the reason is the gate's own refusal message without its leading `refused: `, the same words CI prints. It is cut to 1024 bytes on a character boundary.
- **The name** is `<app>-<environment>.<nanoseconds in hex><8 random hex digits>`: client-go's shape (`<object>.<hex time>`) plus a random suffix, so two Deploys of the same Environment never collide, even two in the same nanosecond on two replicas. `generateName` was the alternative. It was not chosen because the name would not sort by time, and a collision, however rare, would be the gate's to retry.
- **The annotations** are the ticket's six. `iidp.itema.no/commit` is the full SHA the response already carries.

## Which calls are recorded

**Every call the gate answers 200** is `DeployAccepted`, including one that commits nothing because the Environment already runs the tag with the same migration command and tasks. That call is still an accepted Deploy (a repeated `v*` tag push, or `ci set-image` repeating a call it lost the answer to), so it is recorded, without `iidp.itema.no/commit`, and its note says nothing was committed. Argus then has no commit to join on and falls back to the tag (#106), which is already serving. Leaving it out was the alternative. It would make "every Deploy it accepts" untrue and hide a repeated promote from the feed.

**A refusal is recorded once the gate knows which Environment the call is for**: after the binding shows the caller is the Application's own repository, and the gate has decided the Environment from the ref. So the refusals recorded are the image check's (422 for a missing tag, 503 when the registry cannot be asked), a migration command without Postgres and tasks on a Static site (409), and anything else that fails after that point, such as a push that fails (500). The `iidp.itema.no/refusal` annotation carries the status.

Everything before that point is not recorded, because there is no Environment to point at, or no reason to believe the caller:

- an untrusted token, a repository outside the org, a ref that neither deploys nor promotes, and a malformed request, all refused before the Platform repository is cloned;
- an unknown Application, an unbound one, and a call from a repository other than the bound one (the ticket's "unknown Application, or a caller whose repository isn't bound");
- an Environment the Application does not have (404), or one the ref may not deploy, such as `prod` from `main` when there is a `staging` (403).

The last case has an Application behind it, and the Event could have pointed at the Environment the call named. It is not recorded for two reasons. The gate never decided on that Environment, so no Deploy of it was possible. And the rule stays one line: an Event exists only for an Environment the gate would have written. The generated workflow asks for `auto`, so this refusal only comes from a hand-edited workflow, which CI shows. It is the one judgement call here; recording it is a small change in `authorize` if Argus wants it.

Not recording a refusal from another repository also matters for trust: an Event is created only for a caller that has proved to be the Application's own repository, so no other repository in the org can put Events on an Application it does not own.

## Never failing or delaying the Deploy

The Event is created after the answer is written, in a goroutine with its own 10-second timeout, so the API server's latency never reaches CI, and a slow or hanging API server does not hold the handler. A failure is logged (`the Deploy's Event was not recorded`, with the error) and nothing else happens: no retry, since a missed Event only means Argus draws that Deploy from Waiting for ArgoCD onward (#106). On SIGTERM the gate waits for the Events in flight after the server has shut down, each bounded by its timeout. Without a service account token (off the cluster), the gate records nothing and says so at start, in the same warning as `iidp app status`.

## One POST with the standard library, reusing `platformstate.Kube`

`deploygate.KubeEvents` is one POST to `/apis/events.k8s.io/v1/namespaces/argocd/events`, using the `platformstate.Kube` the gate already builds for `iidp app status`: its API server URL, its CA, and its service account token, re-read per request. It is built like #114's `KubePatcher`, which sends the ArgoCD refresh the same way. As in #94, client-go was not added for one call; `go.mod` is unchanged. The request code lives in `internal/deploygate/events.go`, not in `internal/platformstate`, which reads and is shared with Argus (#116). The gate takes an `EventSink` interface, so the tests use an in-memory sink and `KubeEvents` is tested alone against a stand-in API server.

## RBAC: `create` on `events.k8s.io` events in `argocd`, in its own Role

A Role `iidp-deploy-gate-events` with one rule, `create` on `events` in `events.k8s.io`, and a RoleBinding to `argocd/iidp-deploy-gate`. Like #114's `iidp-deploy-gate-refresh`, it takes the release namespace, which is `argocd`: the bootstrap renders the gate there, and `TestDeployGateFollowsTheBootstrapAndThePlatformValues` checks it. So both of the gate's writes are Roles in `argocd`, each of its own, and the status Role stays list-only. Core `v1` events are not granted: the gate only uses `events.k8s.io`.

`TestDeployGateOnlyWritesTheRefreshAndEventsInArgoCD` renders the chart. It fails if any Role or ClusterRole grants anything beyond `list` other than these two writes, `patch` on ArgoCD Applications and `create` on `events.k8s.io` events, both in a Role in `argocd`. It also fails if either write Role holds any other rule, if anything narrows by `resourceNames`, or if a write binding names another role or subject. `TestDeployGateReadsTheClusterWithListOnly` still bounds the reads.

## Tests

- **The gate** (`internal/deploygate/events_test.go`), through its HTTP boundary with the fake issuer, fake GitHub and fake registry, and `fakeEvents` as the sink:
  - an accepted Deploy (main to staging) and an accepted Promote (a `v*` tag to prod) each create one Event with every annotation, the `regarding` in `argocd`, the note, and an `eventTime` in the API server's MicroTime format; a second Deploy of the same Environment gets a different name; a repeated Promote that commits nothing is still accepted, without a commit;
  - a missing image creates `DeployRefused` with 422 and the reason, and a migration command without Postgres one with 409;
  - no Event for an unknown Application, an unbound one, another repository, another org, a disallowed ref, an Environment the Application does not have, one the ref may not deploy, or an untrusted token;
  - a failing sink leaves the Deploy committed and answered 200, and logs the failure; a sink that hangs does not hold the answer.
- **`KubeEvents`**: the method, path, token, content type and body, and an API server's refusal.
- **The bootstrap** (`bootstrap_test.go`): the test above.
- **The kind e2e** (`checkDeployEvents`, in `testDeployGate`): after its four calls there are exactly two Events from the gate in `argocd`, both regarding `brochure-prod`: `DeployRefused` for the missing tag, with 422, and `DeployAccepted` with the Platform repository commit. That is the real API server accepting the Event, and the Role letting the gate create it.

## For the rollout

- Release, then bump the bootstrap. The bump creates the Role and RoleBinding. After the next Deploy, `kubectl get events -n argocd --field-selector reason=DeployAccepted` shows it, and the gate's log has no "was not recorded" line.
- Events expire after the API server's `--event-ttl`. kube-apiserver's default is one hour, and k3s is not known to change it (not checked on the node). That is enough for Argus's live view and its restart seeding (#115).
