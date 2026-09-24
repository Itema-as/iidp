{{/*
The gate's host and URL: deploy.<baseDomain>, which the Platform's wildcard
certificate covers. The URL is the OIDC audience too.
*/}}
{{- define "deploy-gate.host" -}}
{{- printf "deploy.%s" (required "baseDomain is required" .Values.baseDomain) -}}
{{- end -}}

{{- define "deploy-gate.url" -}}
{{- printf "https://%s" (include "deploy-gate.host" .) -}}
{{- end -}}

{{/*
The image tag: image.tag when set, otherwise the bootstrap release version
(bootstrapRevision v1.2.3 gives 1.2.3), the tag the release workflow
publishes the gate under. A revision that is not a release tag (a branch, a
commit) names no published image, so rendering fails with a message saying
so; only this Application fails, not the rest of the bootstrap.
*/}}
{{- define "deploy-gate.tag" -}}
{{- if .Values.image.tag -}}
{{- .Values.image.tag -}}
{{- else if regexMatch "^v[0-9]+\\.[0-9]+\\.[0-9]+" (toString .Values.bootstrapRevision) -}}
{{- trimPrefix "v" (toString .Values.bootstrapRevision) -}}
{{- else -}}
{{- fail (printf "the Deploy gate's image tag cannot be derived from the bootstrap revision %q, which is not a v* release tag: pin the bootstrap to a release, or set deployGate.image.tag in platform.yaml" (toString .Values.bootstrapRevision)) -}}
{{- end -}}
{{- end -}}

{{- define "deploy-gate.labels" -}}
app.kubernetes.io/name: iidp-deploy-gate
app.kubernetes.io/part-of: iidp
{{- end -}}
