# #110 Every Application runs as non-root, and Pod Security enforces restricted

#90 enforced Pod Security `baseline` on Application namespaces and only warned about `restricted`. The chart's `runAsNonRoot` value decided whether a container was held to non-root with every capability dropped, and whether a Static site listened on 8080 or 80. The CLI wrote it only for a Dockerfile it had generated, so it went stale as soon as a developer changed the base image. This removes the value. Every container runs as non-root, every Static site listens on 8080, and Application namespaces enforce `restricted`. The decisions in the issue's "Decided (Phase 3 grilling, 2026-09-30)" section are binding. This note records how they were carried out and what was checked.

Sources, checked on 2026-09-30:

- kubernetes.io, "Pod Security Standards" (v1.36): restricted's controls, the ones `chart/application/guardrails_test.go`'s `restrictedViolations` checks. "Pod Security Admission": `warn` and `audit` are also evaluated on workload resources (their Pod template), `enforce` only on Pods.
- cert-manager `v1.21.2` (the chart `bootstrap/versions.yaml` pins), `pkg/issuer/acme/http/pod.go`: the HTTP-01 solver Pod sets `runAsNonRoot` from `--acme-http01-solver-run-as-non-root` (default `true`, `internal/apis/config/controller/v1alpha1/defaults.go`), the `RuntimeDefault` seccomp profile, and on its container `allowPrivilegeEscalation: false` and `capabilities.drop: [ALL]`. So it passes restricted. Read, not seen: kind never issues a certificate.
- Images, seen with Podman: `nginxinc/nginx-unprivileged:1.30-alpine` has `User=101` and `ExposedPorts 8080/tcp`, and both `1.30-alpine` and `1.30.0-alpine` exist on Docker Hub. `node:24-slim` has no `User` (so it runs as root), and its `node` user is `uid=1000 gid=1000`.

## The chart

- `runAsNonRoot` is gone from `values.yaml`, and so is its "must be true or false" check. `application.securityContext` takes no argument now. It always sets `runAsNonRoot: true`, `capabilities.drop: [ALL]`, `seccompProfile: RuntimeDefault` and `allowPrivilegeEscalation: false`, on the Deployment, the migration Job, the final Backup hook and each Scheduled task CronJob.
- `application.port` is 8080 for every Static site. `port` stays ignored for them, for the reason #90 gave.
- **A leftover `runAsNonRoot` is ignored, not refused.** Chosen so that a chart bump needs no values edit in the same commit. The chart has no values schema, so Helm passes an unknown key through. The values file and the chart version are read by two different ArgoCD Applications (below), so asking for both edits at once would open a window where one of them is missing. `TestALeftoverRunAsNonRootChangesNothing` renders `runAsNonRoot: false` and still gets non-root on 8080.
- Fixtures: `static-site-non-root.yaml`, `postgres-non-root.yaml` and `refuse-run-as-non-root-not-bool.yaml` are deleted, since every fixture is non-root now. `TestEveryWorkloadPassesRestricted` runs the offline restricted check on every workload of every rendering fixture, and still requires that the fixtures render all four workload kinds between them.

## The CLI

- `render.NamespaceLabels` enforces `restricted`. `warn` and `audit` stay `restricted`. Pod Security evaluates `enforce` only on Pods, but `warn` and `audit` on the workloads too. So a Deployment whose Pods would be refused still says so when ArgoCD applies it, and the audit log records it. Without them, the only sign would be a ReplicaSet event.
- `render.Environment.RunAsNonRoot`, `platformrepo.Application.RunAsNonRoot` and `templates.Framework.RunsAsNonRoot` are gone. `render.CopyValuesForStaging` drops a `runAsNonRoot` key, so `add-capability --staging` doesn't copy a value from a prod written by an older CLI.
- The previews ApplicationSet gets the same labels through `NamespaceLabels`.

## Adopt

Adopt changed nothing in an existing Dockerfile before. Now it changes one that would run as root, when the fix is known (`internal/apprepo/nonroot.go`, `fixNonRoot`). Otherwise the pull request says what to change, under "Before merging: the Dockerfile may run as root", and so does the CLI's output. When nothing else is missing, the "nothing to add" refusal carries the same advice.

How it reads a Dockerfile:

- **Only the final stage counts**, the one the image is built from. Its user is the last `USER` in that stage, or in the stage it is built `FROM` (following `FROM <stage>` back), or else the base image's own user.
- Global `ARG` defaults are substituted into `FROM` (`node:${NODE_VERSION}` is how the Next.js template names its image). The repository is compared without `docker.io/` and `library/`. `--platform` and `AS name` are kept.
- Continuation lines, comments inside them, BuildKit here-documents and lower-case instructions are handled. A parser directive (`# escape=`) is not. With a backtick escape, a continued line would be misread.

The known fixes, as the issue decided them:

| The final stage | Adopt writes |
|---|---|
| A numeric `USER` other than 0, or the unprivileged nginx with no `USER` | nothing |
| Official `node` image, no `USER`, `USER root` or `USER 0` | `USER 1000:1000` added at the end of the stage, before its trailing `CMD`/`ENTRYPOINT`/`EXPOSE`/`HEALTHCHECK`/`LABEL`/`STOPSIGNAL` and the comments above them, with a one-line comment. A `USER root` is kept, since the `RUN` lines after it may need it. |
| Official `node` image, `USER node` (or `node:node`) | `USER 1000:1000` in its place |
| A named `USER` created in the stage's chain by `useradd`/`adduser` with `-u N`, `--uid N` or `--uid=N` | `USER N` in its place, the group part kept as written. For hello: `USER 1001`. |
| Official `nginx` image, for a Static site | `nginxinc/nginx-unprivileged` at the same tag (a tag from a build argument stays the same argument), and `EXPOSE 80` becomes `EXPOSE 8080` |

- **`USER N`, not `N:N`, for a named user.** With a number alone, the runtime takes the group from `/etc/passwd`, the same group the named user had. Adding a group would guess it.
- **The `useradd`/`adduser` must end with the user's name**, the last word of that command, and the name must get one number. Two different numbers for one name get advice instead. Reading option by option would need each tool's flags (busybox, Debian and shadow's `useradd` disagree about `-g`), and the last word is the login name in every one of them.
- **`USER node` on a Node image counts as known** although the issue's list doesn't name it. The node image's `node` user is 1000 (seen), and the issue's own fix for a Node image is "`USER 1000:1000`, the `node` user". A `node` user that the Dockerfile creates itself without a number gets advice.
- **nginx gets advice instead of the fix** when:
  - the Application is a Web service, whose port is its `--port`, not 8080;
  - the Dockerfile asks for root;
  - the image is pinned by digest, which the unprivileged image doesn't share;
  - the image's name comes from a build argument;
  - the stage runs any `RUN`, which the unprivileged image runs as 101;
  - or it `COPY`s or `ADD`s anything under `/etc/nginx`. A config of its own has to listen on 8080 and keep its pid and temporary files where 101 can write, and iidp would be guessing.
- Anything else that may run as root (another base image, `scratch`, a `USER` from a build argument, a named user with no number shown) gets advice ending with what the Platform needs: a numeric `USER` other than 0 in the final stage.

The wizard's summary lists the Dockerfile among the files when Adopt will change it (`Detection.Files` takes the Kind now, which the nginx rule needs).

`internal/apprepo/nonroot_test.go` is the table: 40 cases, including hello's own Dockerfile. `TestTheTemplatesDockerfilesNeedNoFix` checks that the Next.js and Vite React templates already run as a numeric non-root user. The CLI tests cover an existing non-root Dockerfile left alone, an nginx Static site changed (diff status `M`, the exact new file, the body and the output), and a Python image left alone with advice in the body and the output.

## Create

The Other stub now ends with a real `USER 1000:1000` line, below its comments, and says to keep it last. The stub is all comments otherwise and doesn't build until it is filled in, so an instruction before any `FROM` breaks nothing that worked. The Next.js template already said `USER 1001:1001` and the Vite React template uses the unprivileged nginx (#90). Only their comments and READMEs, which mentioned `runAsNonRoot: true` in the values, changed.

## The kind end-to-end test

The fixture namespaces enforce `restricted`, because their `application.yaml` files are kept equal to what the CLI writes. The fixture Applications moved from `docker.io/library/nginx` to `docker.io/nginxinc/nginx-unprivileged` at the same tags. Web services listen on `port: 8080`, and the platform.yaml `extraAllowedImages` entry follows. The unprivileged image is Alpine too, so the migration command's busybox `nc` is still there.

`testGuardrails` now also fails on:

- any audit event in a fixture namespace with a `pod-security.kubernetes.io/audit-violations` annotation. Audit is `restricted`, and it is evaluated on Pods and on Deployments, Jobs and CronJobs alike;
- any Pod create there refused with 403;
- no successful create of CloudNativePG's instance Pod `shop-db-1` and its `shop-db-1-initdb` Job in `shop-prod`. Both were admitted by a namespace that enforces restricted.

The privileged Pod check now expects `violates PodSecurity "restricted`.

The issue asked to stop if CloudNativePG's Pods didn't pass `restricted`. They do. In the kind run for #146 (`TestBootstrap` passed), the fixture namespaces enforced `restricted`, and a privileged Pod was refused with `violates PodSecurity "restricted`. `shop-db-1` and `shop-db-1-initdb` were created, and both databases became healthy. The migration Jobs, the Scheduled task's Jobs, shop-staging's final Backup hook and brochure's first deploy all ran. Of 79 audited writes in the fixture namespaces, none carried a Pod Security violation, and no Pod was refused.

## What the rollout has to do

In order, in the comment on #110: hello's and iprofil's own pull requests first, then the chart release and bump, then `restricted` enforced.

- **hello**'s Dockerfile has `USER nextjs`. Its values have no `runAsNonRoot`, so on chart 0.3.4 nothing forces non-root. The new chart forces it on the app, the migration Job and the heartbeat task, and a named user can't start under it. So `USER 1001:1001` (Itema-as/hello#3) has to reach staging, prod (a signed `v*` tag) and any open preview before the chart bump.
- **iprofil** runs `nginx:1.30-alpine` as root on 80. Chart 0.3.4 serves it on 80 because its values have no `runAsNonRoot`, and the new chart serves it on 8080. The chart version sits in `application.yaml`, which the `applications` Application applies. The image tag sits in `values.yaml`, which `iprofil-prod` reads from `main` itself. So a commit that changed both could be rendered by `iprofil-prod` with the new values and the old chart, until the next poll of `applications`. The way through keeps the image and its port in the one file, `values.yaml`, on 0.3.4. First `runAsNonRoot: true` goes in by hand. Then the unprivileged image (Itema-as/iprofil#20) is merged, and the Deploy gate writes its tag into the same file. Every render after that has both, and chart 0.3.4 serves it on 8080. The chart bump then changes nothing about the port, whichever Application reconciles first.

## Not covered

- An Environment whose image still runs as root, or with a named user, has its new Pods refused once its namespace enforces `restricted` or it moves to this chart. Its old Pods keep running until something replaces them, such as a rollout, an eviction or a node reboot. Hello and iprofil are the only Applications, and the rollout moves both first.
- A Static site whose image listens elsewhere than 8080 can no longer say so. `port` is still ignored for Static sites.
- Adopt reads only `Dockerfile` at the repository root, the file the deploy workflow builds.
