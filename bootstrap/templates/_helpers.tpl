{{/*
The pinned versions, from versions.yaml next to Chart.yaml.
*/}}
{{- define "iidp-bootstrap.versions" -}}
{{- .Files.Get "versions.yaml" -}}
{{- end -}}

{{/*
Host part of argocdURL, used for the ingress, the Dex redirect and the
ArgoCD external URL.
*/}}
{{- define "iidp-bootstrap.argocdHost" -}}
{{- (urlParse .Values.argocdURL).host -}}
{{- end -}}

{{/*
Sync policy shared by every component Application. Automated with prune and
self-heal, server-side apply (the cert-manager and ArgoCD CRDs are too large
for client-side apply), and unlimited retries so that components which
depend on another one's CRDs or namespace converge on their own. Each retry
refreshes to the newest revision (retry.refresh): without it a retrying sync
stays pinned to the commit that failed, ArgoCD starts no new automated sync
while it runs, and a fix pushed afterwards never applies until someone
terminates the operation by hand. Takes a list of extra sync options.
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

{{/*
Destination: always this cluster.
*/}}
{{- define "iidp-bootstrap.destination" -}}
server: https://kubernetes.default.svc
{{- end -}}

{{/*
The Itema login cookie's domain, without the leading dot: cloudflareZone
when it is set, baseDomain otherwise. oauth2-proxy sets its session cookie
for it and redirects back only to hosts under it, so any host inside it (a
Platform address, or a custom domain inside the zone) can be a protected
Application. The CLI derives the application chart's
platform.loginCookieDomain from the same two platform.yaml fields
(docs/implementation-notes/76-login-in-zone-domains.md). The sign-in
callback stays on auth.<baseDomain>, and a browser only accepts a cookie
there if that host is inside the cookie's domain, so a baseDomain outside
cloudflareZone is refused.
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
