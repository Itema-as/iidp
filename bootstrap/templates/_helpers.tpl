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
