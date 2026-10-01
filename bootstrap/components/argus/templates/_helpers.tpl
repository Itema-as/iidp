{{- define "argus.host" -}}
{{- printf "argus.%s" (required "baseDomain is required" .Values.baseDomain) -}}
{{- end -}}

{{/*
The image tag: image.tag when set, otherwise bootstrapRevision without its
v. A revision that is not a release tag names no published image, so
rendering fails; only this Application fails, not the rest of the bootstrap.
*/}}
{{- define "argus.tag" -}}
{{- if .Values.image.tag -}}
{{- .Values.image.tag -}}
{{- else if regexMatch "^v[0-9]+\\.[0-9]+\\.[0-9]+" (toString .Values.bootstrapRevision) -}}
{{- trimPrefix "v" (toString .Values.bootstrapRevision) -}}
{{- else -}}
{{- fail (printf "Argus's image tag cannot be derived from the bootstrap revision %q, which is not a v* release tag: pin the bootstrap to a release, or set argus.image.tag in platform.yaml" (toString .Values.bootstrapRevision)) -}}
{{- end -}}
{{- end -}}

{{- define "argus.labels" -}}
app.kubernetes.io/name: iidp-argus
app.kubernetes.io/part-of: iidp
{{- end -}}
