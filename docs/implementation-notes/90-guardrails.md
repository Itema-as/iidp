# #90 Guardrails: Pod Security and admission policies on Application namespaces

Application namespaces now get guardrails from the admission control built into Kubernetes, as the ADR-0004 note decided. Pod Security enforces `baseline` and warns about and audits `restricted`. Four ValidatingAdmissionPolicies check image origin, container limits, Service types and Ingress hosts, and for now they only warn and audit. Nothing new runs on the node. The chart sets the `restricted` securityContext wherever the image allows it, and the templates build non-root images. The Vite React template's nginx moves to 8080 as a result.

Sources, checked on 2026-09-26, against Kubernetes v1.36 (k3s `v1.36.4+k3s1`, `infra/platform/variables.tf`):

- kubernetes.io, "Validating Admission Policy" (stable since v1.30): `validationActions` (Deny, Warn, Audit; Deny and Warn not together), `matchConditions`, `variables`, `failurePolicy`, and the audit annotation `validation.policy.admission.k8s.io/validation_failure` ("Audit annotations" reference page).
- The API reference for `ParamRef` (admissionregistration/v1): "If paramKind is namespace-scoped and this field is unset, the namespace of the object being evaluated for admission will be used"; `parameterNotFoundAction` is `Allow` or `Deny` and required.
- The API reference for `ValidatingAdmissionPolicySpec.validations[].expression`: the CEL variables `object`, `oldObject`, `request`, `params`, `namespaceObject`, `variables`, `authorizer`.
- kubernetes.io, "Pod Security Admission" and "Pod Security Standards": the labels `pod-security.kubernetes.io/<mode>: <level>` and the optional `<mode>-version` (default `latest`); restricted's controls (volume types, `allowPrivilegeEscalation: false`, `runAsNonRoot: true`, `runAsUser` not 0, seccomp `RuntimeDefault` or `Localhost`, drop `ALL`, add only `NET_BIND_SERVICE`).
- k3s docs (k3s-io/docs): the hardening guide's audit-log `kube-apiserver-arg` entries, and "Configuration File": drop-ins in `/etc/rancher/k3s/config.yaml.d/*.yaml`, and a `+` suffix on a key appends to it instead of replacing it.
- ArgoCD docs (argoproj/argo-cd, `docs/user-guide/sync-options.md`): `managedNamespaceMetadata` needs `CreateNamespace=true`, and adopting a namespace that already has client-side-applied metadata can drop it.
- cert-manager docs: the HTTP-01 Ingress solver's `serviceType` (`NodePort` by default, or `ClusterIP`) and the solver Pod's default limits (`--acme-http01-solver-resource-limits-cpu` 100m, `-memory` 64Mi).
- CloudNativePG: `pkg/versions/versions.go` at `v1.30.0` (the operator chart 0.29.0 installs appVersion 1.30.0) for the default images, `ghcr.io/cloudnative-pg/postgresql:18.4-system-trixie` and `ghcr.io/cloudnative-pg/cloudnative-pg:1.30.0`; the plugin-barman-cloud chart 0.8.0 values for `ghcr.io/cloudnative-pg/plugin-barman-cloud-sidecar`; the plugin's API reference for `ObjectStore.spec.instanceSidecarConfiguration.resources` (no default).

Everything below marked "seen" was run, not read: on k3s v1.36.4 itself (`rancher/k3s:v1.36.4-k3s1` under Podman, control plane only, with the audit drop-in and policy from this change), on kind v1.36.4 through `test/templates/run-restricted.sh`, and with `podman run`.

## Which namespaces, and how they get their labels

The issue asked for the Pod Security labels through the Environment Application's managed namespace metadata. The CLI's `internal/render` now writes, into every Environment's `application.yaml`:

```yaml
syncPolicy:
  managedNamespaceMetadata:
    labels:
      iidp.itema.no/application: shop
      iidp.itema.no/environment: prod
      pod-security.kubernetes.io/enforce: baseline
      pod-security.kubernetes.io/warn: restricted
      pod-security.kubernetes.io/audit: restricted
```

`iidp.itema.no/application` doubles as the marker the policy bindings select on (`namespaceSelector`, `Exists`). The Platform namespaces never carry it, so they are exempt without a list of them to keep. The alternative was a selector that excludes Platform namespaces by name (`NotIn [argocd, kube-system, ...]`). It would also cover a namespace made by hand, but every new Platform component would have to be added to it, and kind's own `local-path-storage` would need an entry too. `iidp.itema.no/environment` sits beside it so the namespace says the same thing as the ArgoCD Application and every chart object.

No `*-version` labels: each mode follows the API server (`latest`). Pinning `enforce-version` would keep an upgrade from tightening `baseline` under a running Application. `baseline` has not changed in a way that matters here, the node's k3s is bumped deliberately, and a pin is one more thing to bump.

Existing Environments (hello-staging, hello-prod, iprofil-prod) get the block by hand in the rollout (#96). Their namespaces were created by ArgoCD with no labels besides the `kubernetes.io/metadata.name` the API server sets, so the ArgoCD caveat about client-side-applied metadata does not apply.

## The four policies

`bootstrap/components/guardrails`, installed by a new bootstrap Application `guardrails`, the same shape as `platform-tls` (the bootstrap chart itself renders only Applications, which `bootstrap_test.go` enforces).

- **Only Pods for the two container rules.** Matching Deployments, StatefulSets, Jobs and CronJobs as well would warn ArgoCD when it applies them, but one expression then has to reach the Pod spec through a different path for each kind, and CEL type-checks it against each kind's schema. The checker reports a missing field on every kind but one, so the expression needs one `object.kind` branch per shape and a policy status full of type warnings. Every container runs in a Pod, so Pods alone leave no gap. Pod Security's `warn` already reports on the workload kinds. The policy status on k3s shows no type-checking warnings (seen).
- **UPDATE too, but not while deleting.** Pod images are mutable, so updates are checked. Every policy skips an object with `deletionTimestamp` (`matchConditions`). Otherwise, once they deny, removing the last finalizer of a non-compliant object would be refused and the object would never go away.
- **Images by repository.** An entry ending in `/` is a prefix (`ghcr.io/itema-as/`); any other entry is one repository, matched as `<entry>:` or `<entry>@`, so `ghcr.io/cloudnative-pg/postgresql-evil` does not match `ghcr.io/cloudnative-pg/postgresql` (seen). The entries are validated against image-reference characters before they are rendered into the CEL string, so none can break out of it.
- **Where each allowed image comes from:**
  - `ghcr.io/itema-as/`: every Application image, from the deploy workflow. The migration Job and, later, the Scheduled task CronJob (#91) run the Application's own image.
  - `ghcr.io/cloudnative-pg/cloudnative-pg`: the operator puts its own image in every database Pod and Job as the `bootstrap-controller` init container.
  - `ghcr.io/cloudnative-pg/postgresql`: the operator's default Postgres image. The chart's `Cluster` names none.
  - `ghcr.io/cloudnative-pg/plugin-barman-cloud-sidecar`: the Barman Cloud plugin injects it into every database Pod.
  - `docker.io/bitnami/kubectl`: the final Backup PreDelete hook, pinned by digest in `_helpers.tpl`. It is allowed by repository, not digest. Otherwise a chart release that moved the digest, on a Platform whose bootstrap is older, would have its hook refused once the policies deny, and a refused hook blocks the Environment's deletion.
  - `quay.io/jetstack/cert-manager-acmesolver`: cert-manager's HTTP-01 solver, which runs in the Application's namespace while a custom domain's certificate is issued.
- **The CNPG list stays in step** because it names repositories, not tags. An operator or plugin bump changes the tags, and the list does not care. If CNPG ever moves an image to a new repository, the kind end-to-end test catches it: it runs a real `Cluster` with the pinned operator and plugin, and fails on any guardrail failure in the fixture namespaces. The e2e runs on every change under `bootstrap/`, which is where the bump would be.
- **Limits.** The Barman Cloud sidecar had no resources, so the chart's `ObjectStore` now sets `instanceSidecarConfiguration.resources`: requests 10m CPU and 64Mi, limits 500m and 512Mi. The request is small because the node and the kind runner are near their CPU request ceiling. Without an explicit request, the request would default to the limit. The memory limit leaves room for a compressed base backup's upload. It has not been measured against a large database; watch for an OOM-killed sidecar. Changing the `ObjectStore` makes the plugin re-inject the sidecar, so each database Pod restarts once when its Environment moves to this chart. The operator's init container copies the `Cluster`'s resources, and so do its Jobs; the e2e shows no limits failure for them. cert-manager's solver Pod has default limits.
- **Services.** Nothing but `NodePort` and `LoadBalancer` is refused. `ExternalName` and `externalIPs` were not asked for, and nothing uses them. cert-manager's solver Service would have been a NodePort, cert-manager's default. The `letsencrypt-http01` ClusterIssuer now sets `serviceType: ClusterIP`, which Traefik routes to the same way.
- **Ingress hosts.** Every rule needs a host, and there is no `defaultBackend`: either would answer for every host. A host passes when it ends with `.<baseDomain>`, at any depth, but not the base domain itself.

### How the Ingress policy learns the declared domains

Chosen: **the chart renders a ConfigMap `iidp-domains` into the Environment's namespace, whose keys are the Environment's `domains`, and the binding reads it as the policy's parameter** (`paramKind: ConfigMap`, `paramRef.name: iidp-domains`, no `paramRef.namespace`, so the API server looks in the Ingress's own namespace). The check is `r.host in params.data`. Seen on k3s: a declared foreign host passes, an undeclared one is reported, and so is a rule with no host.

- The domains stay in one place, the values file the CLI already writes (`--domain`, `add-capability --domain`), and reach the policy with the chart that renders the Ingresses. There is no second copy for the CLI to keep in step, and no CLI change.
- The keys are quoted in the template: a valid hostname such as `1.5` would otherwise be a YAML float.
- Sync wave -1, so a newly declared domain is in the ConfigMap before its Ingress (wave 0) is applied.
- The name is the policy's, not the Environment's, so it assumes one Environment per namespace. That is how the CLI writes them, `<name>-<environment>`.

Considered:

- **A namespace annotation the CLI writes through `managedNamespaceMetadata`**, read with `namespaceObject`. It needs no parameter, and editing it needs rights over namespaces, not just the namespace. But the domains would then live in both `values.yaml` and `application.yaml`, every `--domain` edit would have to touch both, and existing Environments would need both backfilled. Nobody but ArgoCD writes to an Application namespace anyway (ADR-0002: developers have no Kubernetes access), so the trust is the same, the Platform repository.
- **One cluster-wide ConfigMap or CRD listing every Application's domains**, as the policy's parameter. It needs a writer that sees every Application, which the chart doesn't, and the CLI or gate would have to keep it current.

**`parameterNotFoundAction: Allow`, not `Deny`.** Seen on k3s v1.36.4: with `Deny`, an Ingress in a namespace without the ConfigMap is refused outright ("failed to configure binding: no params found for policy binding with `Deny` parameterNotFoundAction"), even though the binding's actions are only `Warn` and `Audit`. The API server treats a missing parameter as a binding it cannot set up, not as a failed check. Every Environment on a chart from before #90 has no ConfigMap, so with `Deny` their next Ingress change would be refused on the day the guardrails arrive. `Allow` skips the policy there instead. That leaves the Ingress hosts of an Application namespace without the ConfigMap unchecked. The chart renders it for every Environment with an image, so after the rollout moves every Environment to a chart from #90 on, the gap is only a namespace someone emptied of it by hand.

## Warn and Audit now, Deny later

Every binding ships `validationActions: [Warn, Audit]`, from `guardrails.validationActions` in `bootstrap/values.yaml`. The rollout flips it to `[Deny, Audit]`: one line in `bootstrap/values.yaml` and a bootstrap release, or one line in `platform.yaml` for a single Platform. The component refuses `Deny` together with `Warn`, as the API server does. Seen on k3s with `[Deny, Audit]`: the NodePort Service, the undeclared Ingress host and the Docker Hub image are refused with the policy's message, and a compliant Pod, Service, Ingress and ConfigMap still go through.

Pod Security's `enforce: baseline` denies from the start. Every Pod the Platform runs in an Application namespace passes it: the chart adds no capability and uses no host namespaces or paths. In the kind e2e's audit log, CloudNativePG's database Pods and initdb Jobs carry no Pod Security violation at all, not even against `restricted`. cert-manager's HTTP-01 solver Pod is not exercised there (Let's Encrypt never issues for `.test`); nothing here checked it against `baseline`, so the first custom domain issued after the rollout is the first real test. A failed solver shows up as a stuck Challenge and a Pod Security event in the namespace. Seen on k3s: a privileged Pod in a labelled namespace is refused.

## The audit log

The Audit action writes to the request's audit event, and k3s writes no audit log by default. Without one, Audit records nothing, and the rollout's "check the audit annotations" has nothing to read. So cloud-init now writes an audit policy and a k3s config drop-in (`/etc/rancher/k3s/config.yaml.d/50-iidp-audit.yaml`, `kube-apiserver-arg+`) before k3s first starts. `infra/README.md`, "Turning on the audit log", does the same on the running node.

- The policy (`infra/platform/cloud-init/audit-policy.yaml`) logs only create/update/patch on Pods, Services, Ingresses and the workload kinds, at Metadata level. That is enough for the admission annotations, and no object body, so no Secret, reaches the log. It is a few hundred events a day on this Platform. Rotation is at 50 MB with four old files, 30 days at most.
- Seen on `rancher/k3s:v1.36.4-k3s1` (under Podman, `--disable-agent`, since rootless Podman gives k3s's kubelet no cpuset cgroup): the drop-in with the `+` key is read (the API server runs with all five `audit-*` arguments), the log directory is created by the API server, and both the VAP `validation_failure` and Pod Security's `audit-violations` annotations appear in it.
- The kind harness gives its API server the same policy file (kubeadm `extraArgs`/`extraVolumes`), so the e2e reads the annotations the Platform would write.
- Not done: shipping the audit log to Grafana Cloud. Alloy tails Pod logs only. The rollout reads it over ssh (the `jq` line in `infra/README.md`). If the guardrails need watching after the rollout, an Alloy file source for it is the next step.

## The chart's securityContext

One helper, `application.securityContext`, included by every container the chart renders: the Deployment's, the migration Job's, the final Backup hook's and the Scheduled task CronJobs' (#91, which merged first; this branch added the helper to its template). A test (`guardrails_test.go`, `TestEveryContainerHasTheSecurityContext`) walks the pod template of every workload in every fixture, whatever its kind, and requires that the fixtures render all four kinds, so a workload added later without the helper fails it.

- Always `seccompProfile: RuntimeDefault` and `allowPrivilegeEscalation: false`. Seen with `podman run --security-opt no-new-privileges` on `nginx:1.30-alpine` as root: it starts and serves. `no_new_privs` stops a setuid binary from gaining privileges, and nginx's master starts its workers with setuid(), which is unaffected.
- **Capabilities are not dropped for an image that may run as root.** The issue asked for `drop: [ALL]`, adding back `NET_BIND_SERVICE` only for a Static site on port 80. Seen with `nginx:1.30-alpine`, iprofil's base: with `--cap-drop ALL --cap-add NET_BIND_SERVICE` it exits at once with `chown("/var/cache/nginx/client_temp", 101) failed (1: Operation not permitted)`. It needs `CHOWN`, `SETUID` and `SETGID` as well; with those four it starts and answers 200. Adding those back for every root image would be a guess about Adopted images we have not seen. An entrypoint that uses `gosu`, writes into a volume it has to `chown`, or binds a raw socket would each need something else. So a root image keeps the runtime's default set, which `baseline` allows, and `restricted`'s warning says what it lacks. The spec's own wording, "drop ALL capabilities where the image allows", covers this.
- **`runAsNonRoot` is not forced.** A new value, `runAsNonRoot` (default `false`), says the image runs as a non-root user with a numeric UID. With it, the chart sets `runAsNonRoot: true` and drops `ALL`, and every workload passes `restricted`. The final Backup hook always does: `bitnami/kubectl` runs as 1001 (image config, seen). The CLI writes `runAsNonRoot: true` only when it generated the Dockerfile from the Next.js or Vite React template (`templates.Framework.RunsAsNonRoot`): on Create, and on Adopt when the repository had no Dockerfile. `add-capability --staging` copies it from prod. It is left out otherwise, including for every Environment written before #90, so nothing live changes.
- **A numeric USER.** Seen on kind v1.36: with `runAsNonRoot: true`, the Next.js template's `USER nextjs` fails to start ("container has runAsNonRoot and image has non-numeric user (nextjs), cannot verify user is non-root"). The template now says `USER 1001:1001`. hello was generated before this, keeps `USER nextjs`, and has no `runAsNonRoot`, so it is unaffected. It would need both edits to move.

## The port change

The Vite React template now serves with `nginxinc/nginx-unprivileged:1.30-alpine`, the same stable branch #81 pinned. It runs as uid 101, listens on 8080 and exposes 8080 (image config, seen). There is no Dependabot; the Dockerfile's comment says the branch has to move.

**How existing Static sites on port 80 keep working: the chart's old default is kept for them, keyed on `runAsNonRoot`.** A Static site with `runAsNonRoot: true` listens on 8080. Without it, it listens on 80 exactly as before. iprofil, whose own Dockerfile runs root nginx on 80, and every Static site generated before #90 therefore change nothing and need no edit. A new Vite React Static site gets `runAsNonRoot: true` from the CLI and 8080.

Considered:

- **Honour `port` for Static sites.** It reads best, but every Static site the CLI ever wrote carries `port: 3000`, the default the CLI writes for every Kind, and never listened on it. A chart bump would move them all to 3000, and an old CLI writing to a Platform with the new chart would do the same to new ones. Leaving `port` ignored and keying on `runAsNonRoot` has neither problem.
- **A separate `staticSite.port`.** Another value to write and keep consistent with the image. The port really follows from who nginx runs as: an unprivileged process cannot bind below 1024 once its capabilities are dropped. So one value says both.

Moving an existing Static site to the unprivileged image takes both its Dockerfile edit and `runAsNonRoot: true` in its values. With only one of them, the new Pod never becomes ready and the old one keeps serving. `README.md` says so.

## The templates' CI job

The Templates job used to `docker run` each image and curl it. It now runs `test/templates/run-restricted.sh`, which creates a kind cluster on the pinned node image, loads the built image, and applies the chart with `runAsNonRoot: true` into a namespace that enforces, warns and audits `restricted`. It fails on any `Warning:` from the apply, a Pod that does not become Ready, `id -u` = 0 in the container, or anything but 200 on `/` at the template's port. Seen locally with Podman: Next.js runs as 1001 and answers on 3000, Vite React runs as 101 and answers on 8080, and neither raises a warning. A deliberately broken image, the Next.js template with `USER nextjs`, fails the script. The job now needs kind and helm, and takes about a minute longer.

## The end-to-end test

`testGuardrails` runs last in `TestBootstrap`, after every fixture Environment has been created, deployed, migrated and (shop-staging) deleted:

- the four fixture namespaces carry the CLI's labels (and `TestFixtureNamespacesCarryTheCLIsLabels`, which needs no cluster, keeps the fixtures' `managedNamespaceMetadata` equal to `render.NamespaceLabels`);
- the audit log has writes in those namespaces and none with a `validation_failure`. That covers the Application Deployments, Services, Ingresses and the `iidp-domains` ConfigMaps, the CloudNativePG Pods and Jobs with the Barman Cloud sidecar, the migration Jobs, shop-prod's Scheduled task Jobs (#91), shop-staging's final Backup hook and brochure's first deploy;
- a `NodePort` Service applied in shop-prod prints an `iidp-service-types` warning, is created, and its audit event carries the failure with the `Warn` and `Audit` actions and a successful response code;
- a privileged Pod is refused by Pod Security (`--dry-run=server`, so nothing runs).

The fixture Applications run nginx from Docker Hub (kind has no GHCR), so the fixture `platform.yaml` sets `guardrails.extraAllowedImages: [docker.io/library/nginx]`. That is the one image a test Platform adds; everything else is the Platform's own list. Nothing new runs on the kind node besides the sidecar's 10m CPU request per database.

The fixture Applications are root nginx images without `runAsNonRoot`, so Pod Security's `restricted` warnings for them are expected, and the test does not assert their absence. The templates' job proves `restricted` for what iidp generates.

## What the rollout (#96) has to do

1. Turn on the audit log on the live node (`infra/README.md`, "Turning on the audit log"). Without it, the Audit action records nothing there.
2. Bump `chartVersion` in `platform.yaml` in the same step as the bootstrap pin. A CLI from this release writes `runAsNonRoot: true` for a new Vite React Static site, whose image listens on 8080. An older chart ignores the value and routes to 80, so such a site would never become ready until its Environment is on this chart.
3. Add the `managedNamespaceMetadata` block to hello-staging, hello-prod and iprofil-prod, each with its own Application and Environment, and bump their chart version. That gives them the `iidp-domains` ConfigMap, and the database Pods restart once for the backup sidecar's resources. Neither gets `runAsNonRoot`: hello's image has `USER nextjs`, and iprofil's runs nginx as root on 80.
4. Read the audit log for failures in the Application namespaces, then set `guardrails.validationActions: [Deny, Audit]`.

## Not covered

- Traefik `IngressRoute` objects in an Application namespace could route any host. Nothing the Platform writes uses them. A fifth policy refusing `traefik.io` kinds in Application namespaces is the fix if that ever matters.
- Ephemeral containers (`kubectl debug`) go through `pods/ephemeralcontainers`, which the policies don't match, so the Platform admin can still debug with any image.
- Pod-level `resources` are not considered; only container limits count.
