{{- define "iidp-bootstrap.versions" -}}
{{- .Files.Get "versions.yaml" -}}
{{- end -}}

{{- define "iidp-bootstrap.argocdHost" -}}
{{- (urlParse .Values.argocdURL).host -}}
{{- end -}}

{{/*
Sync policy shared by every component Application, taking a list of extra
sync options. Server-side apply because the cert-manager and ArgoCD CRDs are
too large for client-side apply. Unlimited retries let components that
depend on another one's CRDs or namespace converge on their own, and
retry.refresh makes each retry use the newest revision: otherwise a retrying
sync stays pinned to the failed commit and a fix pushed afterwards never
applies.
*/}}
{{- define "iidp-bootstrap.syncPolicy" -}}
automated:
  prune: true
  selfHeal: true
syncOptions:
  - CreateNamespace=true
  - ServerSideApply=true
  - ServerSideDiff=true
  {{- range . }}
  - {{ . }}
  {{- end }}
retry:
  limit: -1
  refresh: true
  backoff:
    duration: 10s
    factor: 2
    maxDuration: 3m
{{- end -}}

{{- define "iidp-bootstrap.destination" -}}
server: https://kubernetes.default.svc
{{- end -}}

{{/*
The Itema login cookie's domain, without the leading dot: cloudflareZone
when set, baseDomain otherwise. Any host inside it can be a protected
Application. The CLI derives the application chart's
platform.loginCookieDomain from the same two fields. The sign-in callback is
on auth.<baseDomain> and a browser only accepts the cookie there if that host
is inside its domain, so a baseDomain outside cloudflareZone is refused.
*/}}
{{- define "iidp-bootstrap.loginCookieDomain" -}}
{{- $base := required "baseDomain is required" .Values.baseDomain | toString -}}
{{- $zone := .Values.cloudflareZone | default "" | toString -}}
{{- if $zone -}}
{{- if not (or (eq $base $zone) (hasSuffix (printf ".%s" $zone) $base)) -}}
{{- fail (printf "baseDomain %q must be inside cloudflareZone %q: the Itema login cookie is set for the zone from auth.<baseDomain>" $base $zone) -}}
{{- end -}}
{{- $zone -}}
{{- else -}}
{{- $base -}}
{{- end -}}
{{- end -}}
