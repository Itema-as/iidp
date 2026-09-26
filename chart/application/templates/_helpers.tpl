{{/*
The Application's name, validated once here so every template can rely on it.
It becomes object names and the first label of the Platform address, so it
must be a DNS-1035 label (a Service name must start with a letter) with room
for the -staging suffix inside the 63-character limit.
*/}}
{{- define "application.name" -}}
{{- $name := required "application.name is required" .Values.application.name | toString -}}
{{- if not (regexMatch "^[a-z]([-a-z0-9]{0,53}[a-z0-9])?$" $name) -}}
{{- fail (printf "application.name must be lowercase letters, digits and dashes, start with a letter and be at most 55 characters, got %q" $name) -}}
{{- end -}}
{{- $name -}}
{{- end -}}

{{/*
The Environment, which must be prod or staging.
*/}}
{{- define "application.environment" -}}
{{- $environment := .Values.environment | toString -}}
{{- if not (has $environment (list "prod" "staging")) -}}
{{- fail (printf "environment must be prod or staging, got %q" $environment) -}}
{{- end -}}
{{- $environment -}}
{{- end -}}

{{/*
The name every object of this Environment carries. prod is the unadorned
Application name; staging carries the -staging suffix, the same shape as the
Platform address, so both Environments can share a namespace.
*/}}
{{- define "application.fullname" -}}
{{- $name := include "application.name" . -}}
{{- if eq (include "application.environment" .) "prod" -}}
{{- $name -}}
{{- else -}}
{{- printf "%s-%s" $name (include "application.environment" .) -}}
{{- end -}}
{{- end -}}

{{/*
Refuses values the chart cannot honour, with a message naming the value.
Every template includes this first, so the refusal happens whichever object
Helm renders first. It produces no output.
*/}}
{{- define "application.validate" -}}
{{- $_ := include "application.kind" . -}}
{{- $_ = include "application.domains" . -}}
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
{{- if and .Values.postgres.enabled (hasKey .Values.postgres "finalBackupTimeout") (not (regexMatch "^[1-9][0-9]*$" (.Values.postgres.finalBackupTimeout | toString))) -}}
{{- fail (printf "postgres.finalBackupTimeout must be a positive whole number of seconds, got %v" .Values.postgres.finalBackupTimeout) -}}
{{- end -}}
{{- /*
The checks below would otherwise only run inside the objects that use the
value, which an Environment without its first image does not render (see
application.released). Running them here refuses a broken values file the
same way whether or not the Environment has been released yet.
*/ -}}
{{- $_ = required "image.repository is required" .Values.image.repository -}}
{{- $_ = include "application.resources" . -}}
{{- if .Values.postgres.enabled -}}
{{- $_ = include "application.postgres.backupPath" . -}}
{{- $_ = include "application.postgres.objectStorageEndpoint" . -}}
{{- end -}}
{{- end -}}

{{/*
Whether the Environment has received its first image: "true" when image.tag
is set, empty otherwise. The CLI writes every new Environment with
image.tag: "" and only the deploy workflow's write-back (iidp ci set-image)
ever sets it, so an empty tag means "not released yet": the moments after
iidp app create, before an Adopt pull request is merged, and, with a staging
Environment, prod until the first v* tag promotes staging's image.

Every template renders its objects only when this is true, after
application.validate: an Environment without an image renders nothing at
all, which ArgoCD shows as Synced and Healthy, instead of a comparison
error. Nothing is created ahead of the first image either, the database
included: a Postgres Cluster with no Application to use it would hold the
node's memory and fill the bucket with backups of nothing for as long as the
first release takes, and a final-backup PreDelete hook would have no
Cluster to back up. The first image makes the Environment's first sync the
same as a brand-new Environment's today: the Cluster in wave -2, the
migration in wave -1, the Application in wave 0.
See docs/implementation-notes/47-unreleased-environment.md.
*/}}
{{- define "application.released" -}}
{{- $tag := .Values.image.tag -}}
{{- if and (not (kindIs "invalid" $tag)) (ne ($tag | toString) "") -}}true{{- end -}}
{{- end -}}

{{/*
The Application image, <repository>:<tag>, run by the Deployment and by the
migration Job. Only included from objects gated on application.released, so
the required tag is a guard against a template that forgets the gate.
*/}}
{{- define "application.image" -}}
{{- printf "%s:%s" (required "image.repository is required" .Values.image.repository | toString) (required "image.tag is required" .Values.image.tag | toString) -}}
{{- end -}}

{{/*
Whether the Postgres Capability is on.
*/}}
{{- define "application.postgres.enabled" -}}
{{- if .Values.postgres.enabled -}}true{{- end -}}
{{- end -}}

{{/*
The name of this Environment's CloudNativePG Cluster, and of the ObjectStore
and ScheduledBackup that belong to it: <fullname>-db.
*/}}
{{- define "application.postgres.cluster" -}}
{{- printf "%s-db" (include "application.fullname" .) -}}
{{- end -}}

{{/*
The CNPG-I plugin that archives WAL and takes base backups: the Barman
Cloud Plugin, which the bootstrap installs next to the operator.
*/}}
{{- define "application.postgres.backupPlugin" -}}
barman-cloud.cloudnative-pg.io
{{- end -}}

{{/*
Where this Environment's backups live in the Platform's backups bucket:
s3://<bucket>/<application>/<environment>/, so one bucket holds every
database and a prefix is one Environment.
*/}}
{{- define "application.postgres.backupPath" -}}
{{- printf "s3://%s/%s/%s/" (required "platform.backupsBucket is required when postgres.enabled" .Values.platform.backupsBucket | toString) (include "application.name" .) (include "application.environment" .) -}}
{{- end -}}

{{/*
The annotation that keeps the database's objects (the Cluster, its
ObjectStore and ScheduledBackup) when a sync would prune them. Prune=false
applies to syncs only: ArgoCD's cascade deletion of the Environment's
Application (the resources finalizer, what iidp app delete triggers)
honours Delete=false, not Prune=false, so it still removes them.
*/}}
{{- define "application.postgres.keepOnPrune" -}}
argocd.argoproj.io/sync-options: Prune=false
{{- end -}}

{{/*
The S3 endpoint of the backups bucket's location.
*/}}
{{- define "application.postgres.objectStorageEndpoint" -}}
{{- required "platform.objectStorageEndpoint is required when postgres.enabled" .Values.platform.objectStorageEndpoint | toString -}}
{{- end -}}

{{/*
The Secret CloudNativePG generates for the database owner, <cluster>-app,
whose uri key is the whole DATABASE_URL. Nothing in the chart handles the
credentials themselves.
*/}}
{{- define "application.postgres.appSecret" -}}
{{- printf "%s-app" (include "application.postgres.cluster" .) -}}
{{- end -}}

{{/*
The DATABASE_URL env entry, for the Deployment and the migration Job.
*/}}
{{- define "application.postgres.databaseURLEnv" -}}
- name: DATABASE_URL
  valueFrom:
    secretKeyRef:
      name: {{ include "application.postgres.appSecret" . }}
      key: uri
{{- end -}}

{{/*
The name shared by the final Backup PreDelete hook's ServiceAccount, Role,
RoleBinding and Job: <fullname>-final-backup. Only the Job carries the
PreDelete hook annotation; the RBAC are ordinary chart resources, present
whenever postgres.enabled and pruned with everything else
(docs/implementation-notes/39-final-backup-predelete-hook.md). The name is
stable, not per-attempt, so argocd.argoproj.io/hook-delete-policy:
BeforeHookCreation can replace a previous attempt's Job by name; the
Backup object itself is named with a run-time timestamp inside the Job's
own script instead, since it is created imperatively by kubectl and
BeforeHookCreation only ever reaches resources the chart declares.
*/}}
{{- define "application.postgres.finalBackupName" -}}
{{- printf "%s-final-backup" (include "application.fullname" .) -}}
{{- end -}}

{{/*
The kubectl image the final Backup PreDelete hook Job runs with.
bitnami/kubectl, not the distroless registry.k8s.io/kubectl: the hook's
script needs a shell (bash, for GNU date's -d) to build the Backup manifest
and poll its status, which a distroless image has no room for. Pinned by
digest, not by a floating version tag: Docker Hub's tag API
(hub.docker.com/v2/repositories/bitnami/kubectl/tags, checked 2026-09-22)
shows bitnami/kubectl now publishes only "latest" plus content-addressed
(sha256-*) attestation and signature tags -- no more per-Kubernetes-minor
floating tags like the "1.36" this used to read, so a digest is the only
way left to pin a reproducible build at all. This digest is "latest" as of
that check (kubectl client v1.37.0, verified locally with `kubectl version
--client`) and is the multi-arch manifest list, not a single platform's
image, so it pulls on both amd64 (the Platform's Hetzner node and most CI
runners) and arm64 (Apple Silicon, kind locally) alike. Bumping it is an
edit here, the same as any other pinned version in this repository.
*/}}
{{- define "application.postgres.finalBackupKubectlImage" -}}
docker.io/bitnami/kubectl@sha256:6e9c5284a0dac06e84de9f4d97852d2e6513442ee7ec3a66d35009eec86e1e62
{{- end -}}

{{/*
The custom domains, validated and sorted by how they get their certificate,
as JSON: {"wildcard": [...], "foreign": [...]}. A host directly under the
base domain (<label>.<baseDomain>) is covered by the Platform's wildcard
certificate; any other host is foreign and needs a certificate of its own.
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
The domain the Itema login cookie is set for, without the leading dot:
platform.loginCookieDomain, or the base domain when it is empty (what the
bootstrap's oauth2-proxy uses on a Platform without a cloudflareZone, and
on every Platform before #76).
*/}}
{{- define "application.login.cookieDomain" -}}
{{- .Values.platform.loginCookieDomain | default .Values.platform.baseDomain | toString -}}
{{- end -}}

{{/*
Refuses login.enabled when a host of the Environment is outside the login
cookie domain: the browser would never send that host the cookie set on
the sign-in callback, so every request would go back to sign in. Custom
domains outside it are named. It produces no output.
*/}}
{{- define "application.login.checkDomains" -}}
{{- $cookieDomain := include "application.login.cookieDomain" . -}}
{{- $suffix := printf ".%s" $cookieDomain -}}
{{- $platformHost := include "application.host" . -}}
{{- if not (hasSuffix $suffix $platformHost) -}}
{{- fail (printf "login.enabled needs the Platform address %q inside platform.loginCookieDomain %q, the domain the Itema login cookie is set for" $platformHost $cookieDomain) -}}
{{- end -}}
{{- $outside := list -}}
{{- range .Values.domains -}}
{{- $host := . | toString -}}
{{- if not (or (eq $host $cookieDomain) (hasSuffix $suffix $host)) -}}
{{- $outside = append $outside $host -}}
{{- end -}}
{{- end -}}
{{- if $outside -}}
{{- fail (printf "login.enabled needs every custom domain inside platform.loginCookieDomain %q, the domain the Itema login cookie is set for; outside it: %s" $cookieDomain (join ", " $outside)) -}}
{{- end -}}
{{- end -}}

{{/*
The Traefik middleware annotation of the Itema login Capability, for every
Ingress of an Environment with login.enabled: the bootstrap's shared
oauth2-proxy ForwardAuth middleware, in oauth2-proxy's own namespace. An
unauthenticated browser gets oauth2-proxy's own redirect to Entra ID
(bootstrap/components/oauth2-proxy-login/middleware.yaml). Empty without
login.

cert-manager's HTTP-01 challenge for a host on the ingress-http01.yaml
Ingress does not pass through it: the solver serves the challenge path from
an Ingress of its own, which carries no middleware, and Traefik picks that
router for the path because a longer rule wins
(docs/implementation-notes/76-login-in-zone-domains.md).
*/}}
{{- define "application.login.annotation" -}}
{{- if .Values.login.enabled -}}
traefik.ingress.kubernetes.io/router.middlewares: oauth2-proxy-itema-login-auth@kubernetescrd
{{- end -}}
{{- end -}}

{{/*
The Secret cert-manager writes a foreign host's certificate into, from a
list of the root context and the host: <fullname>-<host with dots replaced
by dashes>-tls. A validated host has only letters, digits, dashes and dots,
so the result is a valid Secret name whenever it is short enough.
*/}}
{{- define "application.tlsSecretName" -}}
{{- $root := index . 0 -}}
{{- $host := index . 1 -}}
{{- printf "%s-%s-tls" (include "application.fullname" $root) (replace "." "-" $host) -}}
{{- end -}}

{{/*
The paths block of one Ingress rule: everything under / to the Environment's
Service on its port. Every host of every Ingress routes the same way.
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

{{/*
The Kind, which must be web-service or static-site.
*/}}
{{- define "application.kind" -}}
{{- $kind := .Values.kind | toString -}}
{{- if not (has $kind (list "web-service" "static-site")) -}}
{{- fail (printf "kind must be web-service or static-site, got %q" $kind) -}}
{{- end -}}
{{- $kind -}}
{{- end -}}

{{/*
The Platform address of this Environment: <name>.<baseDomain> for prod and
<name>-staging.<baseDomain> for staging.
*/}}
{{- define "application.host" -}}
{{- printf "%s.%s" (include "application.fullname" .) (required "platform.baseDomain is required" .Values.platform.baseDomain) -}}
{{- end -}}

{{/*
The port the container listens on, as an integer. A Web service listens on
port and is told so through PORT; a Static site is an nginx image, which
listens on 80 and is not configured through the environment.
*/}}
{{- define "application.port" -}}
{{- if eq (include "application.kind" .) "static-site" -}}
80
{{- else -}}
{{- .Values.port | int -}}
{{- end -}}
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
The labels every object carries. iidp.itema.no/application and
iidp.itema.no/environment are what Grafana Alloy attributes logs by.
*/}}
{{- define "application.labels" -}}
{{ include "application.selectorLabels" . }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
iidp.itema.no/application: {{ include "application.name" . }}
iidp.itema.no/environment: {{ include "application.environment" . }}
{{- end -}}

{{/*
The resources a size maps to. The numbers are the Platform's convention and
live only here (docs/design.md, "Conventions the chart encodes").
*/}}
{{- define "application.resources" -}}
{{- $sizes := dict
  "small" (dict "cpu" "250m" "memory" "256Mi")
  "medium" (dict "cpu" "500m" "memory" "512Mi")
  "large" (dict "cpu" "1" "memory" "1Gi") -}}
{{- $size := .Values.size | toString -}}
{{- $resources := get $sizes $size -}}
{{- if not $resources -}}
{{- fail (printf "size must be one of %s, got %q" (join ", " (keys $sizes | sortAlpha)) $size) -}}
{{- end -}}
requests:
  cpu: {{ $resources.cpu | quote }}
  memory: {{ $resources.memory | quote }}
limits:
  cpu: {{ $resources.cpu | quote }}
  memory: {{ $resources.memory | quote }}
{{- end -}}
