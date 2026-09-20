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
{{- $kind := .Values.kind | toString -}}
{{- if eq $kind "static-site" -}}
{{- fail "kind static-site is not implemented yet; only web-service renders" -}}
{{- else if ne $kind "web-service" -}}
{{- fail (printf "kind must be web-service or static-site, got %q" $kind) -}}
{{- end -}}
{{- if hasKey (.Values.env | default dict) "PORT" -}}
{{- fail "env must not set PORT; it is injected from port" -}}
{{- end -}}
{{- end -}}

{{/*
The Platform address of this Environment: <name>.<baseDomain> for prod and
<name>-staging.<baseDomain> for staging.
*/}}
{{- define "application.host" -}}
{{- printf "%s.%s" (include "application.fullname" .) (required "platform.baseDomain is required" .Values.platform.baseDomain) -}}
{{- end -}}

{{/*
The port the container listens on, as an integer.
*/}}
{{- define "application.port" -}}
{{- .Values.port | int -}}
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
