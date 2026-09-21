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
{{- if and .Values.postgres.migrationCommand (not .Values.postgres.enabled) -}}
{{- fail "postgres.migrationCommand needs postgres.enabled: true; there is no database to migrate" -}}
{{- end -}}
{{- if and .Values.postgres.enabled (hasKey (.Values.env | default dict) "DATABASE_URL") -}}
{{- fail "env must not set DATABASE_URL; the Postgres Capability injects it" -}}
{{- end -}}
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
