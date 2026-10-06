{{/*
The Application's name. It becomes object names and the first label of the
Platform address, so it must be a DNS-1035 label (a Service name must start
with a letter) with room for the -staging suffix inside 63 characters.
*/}}
{{- define "application.name" -}}
{{- $name := required "application.name is required" .Values.application.name | toString -}}
{{- if not (regexMatch "^[a-z]([-a-z0-9]{0,53}[a-z0-9])?$" $name) -}}
{{- fail (printf "application.name must be lowercase letters, digits and dashes, start with a letter and be at most 55 characters, got %q" $name) -}}
{{- end -}}
{{- $name -}}
{{- end -}}

{{/*
The Environment: prod, staging, or pr-<number> for a Preview Environment.
*/}}
{{- define "application.environment" -}}
{{- $environment := .Values.environment | toString -}}
{{- if not (or (has $environment (list "prod" "staging")) (regexMatch "^pr-[1-9][0-9]*$" $environment)) -}}
{{- fail (printf "environment must be prod, staging or pr-<pull request number>, got %q" $environment) -}}
{{- end -}}
{{- $environment -}}
{{- end -}}

{{/*
The name every object of this Environment carries, and the first label of
its Platform address: the Application name, suffixed with the Environment
except for prod.
*/}}
{{- define "application.fullname" -}}
{{- $name := include "application.name" . -}}
{{- if eq (include "application.environment" .) "prod" -}}
{{- $name -}}
{{- else -}}
{{- $fullname := printf "%s-%s" $name (include "application.environment" .) -}}
{{- if gt (len $fullname) 63 -}}
{{- fail (printf "%s is longer than the 63 characters a Service name and a DNS label allow; the Application's name is too long for this Environment" $fullname) -}}
{{- end -}}
{{- $fullname -}}
{{- end -}}
{{- end -}}

{{/*
Refuses values the chart cannot honour. Every template includes this first,
so the refusal happens whichever object Helm renders first. No output.
*/}}
{{- define "application.validate" -}}
{{- $_ := include "application.kind" . -}}
{{- $_ = include "application.domains" . -}}
{{- if not (kindIs "bool" .Values.postgres.backups) -}}
{{- fail (printf "postgres.backups must be true or false, got %v" .Values.postgres.backups) -}}
{{- end -}}
{{- if hasKey (.Values.env | default dict) "PORT" -}}
{{- fail "env must not set PORT; it is injected from port" -}}
{{- end -}}
{{- if and .Values.postgres.enabled (ne (include "application.kind" .) "web-service") -}}
{{- fail "postgres.enabled needs kind: web-service; a Static site has no server to use a database" -}}
{{- end -}}
{{- if and .Values.postgres.migrationCommand (not .Values.postgres.enabled) -}}
{{- fail "postgres.migrationCommand needs postgres.enabled: true; there is no database to migrate" -}}
{{- end -}}
{{- if and .Values.postgres.enabled (hasKey (.Values.env | default dict) "DATABASE_URL") -}}
{{- fail "env must not set DATABASE_URL; the Postgres Capability injects it" -}}
{{- end -}}
{{- if .Values.login.enabled -}}
{{- $_ = include "application.login.checkDomains" . -}}
{{- end -}}
{{- $_ = include "application.tasks.check" . -}}
{{- $_ = include "application.login.groups" . -}}
{{- if and .Values.postgres.enabled (hasKey .Values.postgres "finalBackupTimeout") (not (regexMatch "^[1-9][0-9]*$" (.Values.postgres.finalBackupTimeout | toString))) -}}
{{- fail (printf "postgres.finalBackupTimeout must be a positive whole number of seconds, got %v" .Values.postgres.finalBackupTimeout) -}}
{{- end -}}
{{- /*
The checks below would otherwise only run inside the objects that use the
value, which an unreleased Environment does not render.
*/ -}}
{{- $_ = required "image.repository is required" .Values.image.repository -}}
{{- $_ = include "application.resources" . -}}
{{- if include "application.postgres.backups" . -}}
{{- $_ = include "application.postgres.backupPath" . -}}
{{- $_ = include "application.postgres.objectStorageEndpoint" . -}}
{{- end -}}
{{- end -}}

{{/*
Whether the Environment has received its first image: "true" when image.tag
is set, empty otherwise. Every template renders its objects only when this
is true, so an Environment without an image renders nothing, which ArgoCD
shows as Synced and Healthy instead of a comparison error. That includes the
database: a Cluster with no Application to use it would hold the node's
memory and fill the bucket with backups of nothing.
*/}}
{{- define "application.released" -}}
{{- $tag := .Values.image.tag -}}
{{- if and (not (kindIs "invalid" $tag)) (ne ($tag | toString) "") -}}true{{- end -}}
{{- end -}}

{{/*
The Application image. Only included from objects gated on
application.released, so the required tag guards against a template that
forgets the gate.
*/}}
{{- define "application.image" -}}
{{- printf "%s:%s" (required "image.repository is required" .Values.image.repository | toString) (required "image.tag is required" .Values.image.tag | toString) -}}
{{- end -}}

{{- define "application.postgres.enabled" -}}
{{- if .Values.postgres.enabled -}}true{{- end -}}
{{- end -}}

{{/*
Whether the database is backed up. WAL archiving, the daily base backup and
the final Backup hook all follow it. A Preview Environment turns it off: its
database is thrown away with the pull request and must not wait on a backup
to be deleted.
*/}}
{{- define "application.postgres.backups" -}}
{{- if and .Values.postgres.enabled .Values.postgres.backups -}}true{{- end -}}
{{- end -}}

{{/*
The name of the CloudNativePG Cluster, and of its ObjectStore and
ScheduledBackup.
*/}}
{{- define "application.postgres.cluster" -}}
{{- printf "%s-db" (include "application.fullname" .) -}}
{{- end -}}

{{- define "application.postgres.backupPlugin" -}}
barman-cloud.cloudnative-pg.io
{{- end -}}

{{/*
Where this Environment's backups live: one bucket holds every database and
a prefix is one Environment.
*/}}
{{- define "application.postgres.backupPath" -}}
{{- printf "s3://%s/%s/%s/" (required "platform.backupsBucket is required when postgres.enabled" .Values.platform.backupsBucket | toString) (include "application.name" .) (include "application.environment" .) -}}
{{- end -}}

{{/*
Keeps the database's objects when a sync would prune them. ArgoCD's cascade
deletion of the Environment's Application honours Delete=false, not
Prune=false, so iidp app delete still removes them.
*/}}
{{- define "application.postgres.keepOnPrune" -}}
argocd.argoproj.io/sync-options: Prune=false
{{- end -}}

{{- define "application.postgres.objectStorageEndpoint" -}}
{{- required "platform.objectStorageEndpoint is required when postgres.enabled" .Values.platform.objectStorageEndpoint | toString -}}
{{- end -}}

{{/*
The Secret CloudNativePG generates for the database owner, whose uri key is
the whole DATABASE_URL.
*/}}
{{- define "application.postgres.appSecret" -}}
{{- printf "%s-app" (include "application.postgres.cluster" .) -}}
{{- end -}}

{{- define "application.postgres.databaseURLEnv" -}}
- name: DATABASE_URL
  valueFrom:
    secretKeyRef:
      name: {{ include "application.postgres.appSecret" . }}
      key: uri
{{- end -}}

{{/*
The name shared by the final Backup hook's ServiceAccount, Role,
RoleBinding and Job. It is stable, not per attempt, so BeforeHookCreation
can replace a previous attempt's Job by name; the Backup object itself is
named with a timestamp inside the Job's script.
*/}}
{{- define "application.postgres.finalBackupName" -}}
{{- printf "%s-final-backup" (include "application.fullname" .) -}}
{{- end -}}

{{/*
The kubectl image the final Backup hook runs. bitnami/kubectl, not the
distroless registry.k8s.io/kubectl, because the script needs bash and GNU
date. Pinned by digest because bitnami/kubectl only publishes "latest";
this is the multi-arch manifest list, so it pulls on amd64 and arm64.
*/}}
{{- define "application.postgres.finalBackupKubectlImage" -}}
docker.io/bitnami/kubectl@sha256:6e9c5284a0dac06e84de9f4d97852d2e6513442ee7ec3a66d35009eec86e1e62
{{- end -}}

{{/*
Refuses Scheduled tasks the chart cannot render. The Deploy gate refuses
these first; this guards a hand-edited values file. The CronJob name length
is only checked with runTasks, so a Preview Environment's longer name does
not refuse staging's tasks it does not run. No output.
*/}}
{{- define "application.tasks.check" -}}
{{- if and .Values.tasks (eq (include "application.kind" .) "static-site") -}}
{{- fail "tasks needs kind: web-service; a Static site has no command of its own to run on a schedule" -}}
{{- end -}}
{{- $seen := dict -}}
{{- range .Values.tasks -}}
{{- $name := .name | default "" | toString -}}
{{- if not (regexMatch "^[a-z]([-a-z0-9]*[a-z0-9])?$" $name) -}}
{{- fail (printf "tasks: name %q must be lowercase letters, digits and dashes, start with a letter and not end with a dash" $name) -}}
{{- end -}}
{{- if hasKey $seen $name -}}
{{- fail (printf "tasks: name %q is used twice" $name) -}}
{{- end -}}
{{- $_ := set $seen $name true -}}
{{- if not (.schedule | default "" | toString) -}}
{{- fail (printf "tasks: %q has no schedule" $name) -}}
{{- end -}}
{{- if not (.command | default "" | toString) -}}
{{- fail (printf "tasks: %q has no command" $name) -}}
{{- end -}}
{{- if and $.Values.runTasks (gt (len (include "application.tasks.name" (list $ $name))) 52) -}}
{{- fail (printf "tasks: the CronJob name %q is longer than the 52 characters Kubernetes allows; shorten the task's name" (include "application.tasks.name" (list $ $name))) -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
The name of a Scheduled task's CronJob, from a list of the root context and
the task's name.
*/}}
{{- define "application.tasks.name" -}}
{{- printf "%s-%s" (include "application.fullname" (index . 0)) (index . 1) -}}
{{- end -}}

{{/*
The custom domains, validated and sorted by how they get their certificate,
as JSON: {"wildcard": [...], "foreign": [...]}. Only a host directly under
the base domain is covered by the wildcard certificate.
*/}}
{{- define "application.domains" -}}
{{- $platformHost := include "application.host" . -}}
{{- $suffix := printf ".%s" (.Values.platform.baseDomain | toString) -}}
{{- $wildcard := list -}}
{{- $foreign := list -}}
{{- $seen := dict -}}
{{- range .Values.domains -}}
{{- $host := . | toString -}}
{{- if or (gt (len $host) 253) (not (regexMatch `^([a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?\.)+[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$` $host)) -}}
{{- fail (printf "domains: %q is not a valid DNS hostname (lowercase letters, digits and dashes in dot-separated labels, at least two labels)" $host) -}}
{{- end -}}
{{- if eq $host $platformHost -}}
{{- fail (printf "domains: %q is this Environment's Platform address, which is always served; remove it" $host) -}}
{{- end -}}
{{- if hasKey $seen $host -}}
{{- fail (printf "domains: %q is listed twice" $host) -}}
{{- end -}}
{{- $_ := set $seen $host true -}}
{{- if and (hasSuffix $suffix $host) (not (contains "." (trimSuffix $suffix $host))) -}}
{{- $wildcard = append $wildcard $host -}}
{{- else -}}
{{- if gt (len (include "application.tlsSecretName" (list $ $host))) 253 -}}
{{- fail (printf "domains: %q is too long for the name of its TLS secret (%s-<host>-tls must be at most 253 characters)" $host (include "application.fullname" $)) -}}
{{- end -}}
{{- $foreign = append $foreign $host -}}
{{- end -}}
{{- end -}}
{{- dict "wildcard" $wildcard "foreign" $foreign | toJson -}}
{{- end -}}

{{/*
The domain the Itema login cookie is set for, without the leading dot.
*/}}
{{- define "application.login.cookieDomain" -}}
{{- .Values.platform.loginCookieDomain | default .Values.platform.baseDomain | toString -}}
{{- end -}}

{{/*
Refuses login.enabled when the Platform address is outside the login cookie
domain, which only a wrong hand-set platform.loginCookieDomain can cause: the
shared login's cookie would never reach it. No output.
*/}}
{{- define "application.login.checkDomains" -}}
{{- $cookieDomain := include "application.login.cookieDomain" . -}}
{{- $platformHost := include "application.host" . -}}
{{- if not (hasSuffix (printf ".%s" $cookieDomain) $platformHost) -}}
{{- fail (printf "login.enabled needs the Platform address %q inside platform.loginCookieDomain %q, the domain the Itema login cookie is set for" $platformHost $cookieDomain) -}}
{{- end -}}
{{- end -}}

{{/*
The custom domains outside the login cookie domain, as JSON:
{"hosts": [...]}, empty without login. The shared login's cookie never
reaches them, so they sign in through the bootstrap's host-only
oauth2-proxy, whose callback and cookies stay on each host. The Platform
address is inside the cookie domain, so none of them is directly under the
base domain: they are all among application.domains' foreign hosts.
*/}}
{{- define "application.login.hostOnlyDomains" -}}
{{- $hosts := list -}}
{{- if .Values.login.enabled -}}
{{- $cookieDomain := include "application.login.cookieDomain" . -}}
{{- range .Values.domains -}}
{{- $host := . | toString -}}
{{- if not (or (eq $host $cookieDomain) (hasSuffix (printf ".%s" $cookieDomain) $host)) -}}
{{- $hosts = append $hosts $host -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- dict "hosts" $hosts | toJson -}}
{{- end -}}

{{/*
The sign-in groups as oauth2-proxy's allowed_groups list, lowercased
because oauth2-proxy compares them as strings with the ID token's claim.
Only GUIDs pass, which also keeps anything else out of the Middleware's
query string.
*/}}
{{- define "application.login.groups" -}}
{{- $groups := .Values.login.groups -}}
{{- if kindIs "invalid" $groups -}}
{{- $groups = list -}}
{{- end -}}
{{- if not (kindIs "slice" $groups) -}}
{{- fail (printf "login.groups must be a list of Entra group object ids, got %v" $groups) -}}
{{- end -}}
{{- if and $groups (not .Values.login.enabled) -}}
{{- fail "login.groups needs login.enabled: true; sign-in groups restrict Itema login, which is off" -}}
{{- end -}}
{{- $seen := dict -}}
{{- $ids := list -}}
{{- range $groups -}}
{{- $id := . | toString | lower -}}
{{- if not (regexMatch "^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$" $id) -}}
{{- fail (printf "login.groups: %q is not an Entra group object id, a GUID such as 0f3b6a4e-8c1d-4e2f-9a7b-5c6d7e8f9a0b" (. | toString)) -}}
{{- end -}}
{{- if hasKey $seen $id -}}
{{- fail (printf "login.groups: %q is listed twice" $id) -}}
{{- end -}}
{{- $_ := set $seen $id true -}}
{{- $ids = append $ids $id -}}
{{- end -}}
{{- join "," $ids -}}
{{- end -}}

{{/*
The name of the Environment's own ForwardAuth Middleware, rendered only
with sign-in groups.
*/}}
{{- define "application.login.middleware" -}}
{{- printf "%s-itema-login" (include "application.fullname" .) -}}
{{- end -}}

{{/*
The Environment's own copy of the host-only Middleware, rendered only with
sign-in groups and custom domains outside the login cookie domain.
*/}}
{{- define "application.login.hostMiddleware" -}}
{{- printf "%s-itema-login-host" (include "application.fullname" .) -}}
{{- end -}}

{{/*
The Itema login middleware annotation, from a list of the root context and
whether the Ingress carries hosts outside the login cookie domain: the
bootstrap's shared or host-only middleware, or with sign-in groups the
Environment's own copy of it. Empty without login.

cert-manager's HTTP-01 challenge does not pass through it: the solver
serves the challenge path from an Ingress of its own, and Traefik picks that
router because a longer rule wins.
*/}}
{{- define "application.login.annotation" -}}
{{- $root := index . 0 -}}
{{- $hostOnly := index . 1 -}}
{{- if $root.Values.login.enabled -}}
{{- if include "application.login.groups" $root -}}
{{- $middleware := ternary (include "application.login.hostMiddleware" $root) (include "application.login.middleware" $root) $hostOnly -}}
traefik.ingress.kubernetes.io/router.middlewares: {{ printf "%s-%s@kubernetescrd" $root.Release.Namespace $middleware }}
{{- else -}}
traefik.ingress.kubernetes.io/router.middlewares: {{ ternary "oauth2-proxy-itema-login-host-auth@kubernetescrd" "oauth2-proxy-itema-login-auth@kubernetescrd" $hostOnly }}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
The Secret cert-manager writes a foreign host's certificate into, from a
list of the root context and the host.
*/}}
{{- define "application.tlsSecretName" -}}
{{- $root := index . 0 -}}
{{- $host := index . 1 -}}
{{- printf "%s-%s-tls" (include "application.fullname" $root) (replace "." "-" $host) -}}
{{- end -}}

{{/*
The paths block of one Ingress rule. Every host routes the same way.
*/}}
{{- define "application.ingressPaths" -}}
http:
  paths:
    - path: /
      pathType: Prefix
      backend:
        service:
          name: {{ include "application.fullname" . }}
          port:
            number: {{ include "application.port" . }}
{{- end -}}

{{- define "application.kind" -}}
{{- $kind := .Values.kind | toString -}}
{{- if not (has $kind (list "web-service" "static-site")) -}}
{{- fail (printf "kind must be web-service or static-site, got %q" $kind) -}}
{{- end -}}
{{- $kind -}}
{{- end -}}

{{- define "application.host" -}}
{{- printf "%s.%s" (include "application.fullname" .) (required "platform.baseDomain is required" .Values.platform.baseDomain) -}}
{{- end -}}

{{/*
The port the container listens on, as an integer. A Static site is an
unprivileged nginx on 8080 (non-root cannot bind below 1024). Its port value
is ignored: the CLI writes --port's default of 3000 for every Kind, which a
Static site does not listen on.
*/}}
{{- define "application.port" -}}
{{- if eq (include "application.kind" .) "static-site" -}}
8080
{{- else -}}
{{- .Values.port | int -}}
{{- end -}}
{{- end -}}

{{/*
The securityContext of every container the chart renders: what Pod
Security's restricted level, enforced in Application namespaces, asks for.
runAsNonRoot needs the image to have a numeric USER.
*/}}
{{- define "application.securityContext" -}}
seccompProfile:
  type: RuntimeDefault
allowPrivilegeEscalation: false
runAsNonRoot: true
capabilities:
  drop:
    - ALL
{{- end -}}

{{/*
The labels the Service and the Deployment select Pods by. They must stay
stable across chart versions, because a Deployment's selector is immutable.
*/}}
{{- define "application.selectorLabels" -}}
app.kubernetes.io/name: {{ include "application.name" . }}
app.kubernetes.io/instance: {{ include "application.fullname" . }}
{{- end -}}

{{/*
The labels every object carries. Grafana Alloy attributes logs by the
iidp.itema.no ones.
*/}}
{{- define "application.labels" -}}
{{ include "application.selectorLabels" . }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
iidp.itema.no/application: {{ include "application.name" . }}
iidp.itema.no/environment: {{ include "application.environment" . }}
{{- end -}}

{{- define "application.resources" -}}
{{- include "application.resourcesFor" (.Values.size | toString) -}}
{{- end -}}

{{/*
A Scheduled task's run gets the smallest size, whatever the Application's,
so it takes as little of the single node as it can.
*/}}
{{- define "application.tasks.resources" -}}
{{- include "application.resourcesFor" "small" -}}
{{- end -}}

{{/*
The resources of the size given as the context. Memory is requested at its
limit because it cannot be taken back from a container. CPU is requested at
a fifth of its limit, so idle Environments do not book the single node's
CPU that nobody uses.
*/}}
{{- define "application.resourcesFor" -}}
{{- $sizes := dict
  "small" (dict "cpu" "250m" "cpuRequest" "50m" "memory" "256Mi")
  "medium" (dict "cpu" "500m" "cpuRequest" "100m" "memory" "512Mi")
  "large" (dict "cpu" "1" "cpuRequest" "200m" "memory" "1Gi") -}}
{{- $size := . -}}
{{- $resources := get $sizes $size -}}
{{- if not $resources -}}
{{- fail (printf "size must be one of %s, got %q" (join ", " (keys $sizes | sortAlpha)) $size) -}}
{{- end -}}
requests:
  cpu: {{ $resources.cpuRequest | quote }}
  memory: {{ $resources.memory | quote }}
limits:
  cpu: {{ $resources.cpu | quote }}
  memory: {{ $resources.memory | quote }}
{{- end -}}
