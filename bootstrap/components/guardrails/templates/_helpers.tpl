{{/*
validationActions, checked here so a mistake fails rendering rather than
the API server's admission of the binding.
*/}}
{{- define "guardrails.validationActions" -}}
{{- $actions := .Values.validationActions | default list -}}
{{- if not $actions -}}
{{- fail "validationActions must name at least one of Deny, Warn, Audit" -}}
{{- end -}}
{{- range $actions -}}
{{- if not (has (. | toString) (list "Deny" "Warn" "Audit")) -}}
{{- fail (printf "validationActions: %q is not one of Deny, Warn, Audit" (. | toString)) -}}
{{- end -}}
{{- end -}}
{{- if and (has "Deny" $actions) (has "Warn" $actions) -}}
{{- fail "validationActions: Deny and Warn cannot be used together; switching the guardrails on is [Deny, Audit]" -}}
{{- end -}}
{{- toJson $actions -}}
{{- end -}}

{{/*
The binding of one policy. Takes a list of the root context and the policy
name.
*/}}
{{- define "guardrails.binding" -}}
{{- $root := index . 0 -}}
{{- $name := index . 1 -}}
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingAdmissionPolicyBinding
metadata:
  name: {{ $name }}
  labels:
    app.kubernetes.io/part-of: iidp
spec:
  policyName: {{ $name }}
  validationActions: {{ include "guardrails.validationActions" $root }}
  # Application namespaces only; Platform namespaces are never checked.
  matchResources:
    namespaceSelector:
      matchExpressions:
        - key: {{ $root.Values.applicationNamespaceLabel }}
          operator: Exists
{{- end -}}

{{/*
The allowed image list as a CEL list literal. Entries are checked to be
plain image-reference characters, so nothing in them can break out of the
string literal.
*/}}
{{- define "guardrails.allowedImagesCEL" -}}
{{- $entries := list -}}
{{- range concat (.Values.allowedImages | default list) (.Values.extraAllowedImages | default list) -}}
{{- $entry := . | toString -}}
{{- if not (regexMatch "^[a-z0-9][a-z0-9._/:@-]*$" $entry) -}}
{{- fail (printf "allowed image %q must be lowercase letters, digits and . _ / : @ -" $entry) -}}
{{- end -}}
{{- $entries = append $entries (printf "'%s'" $entry) -}}
{{- end -}}
{{- if not $entries -}}
{{- fail "allowedImages is empty: no Application could run" -}}
{{- end -}}
[{{ join ", " $entries }}]
{{- end -}}

{{/*
Only Pods are matched for the container policies: every container of every
workload runs in one, and one resource kind keeps the expressions
type-checked against one schema.
*/}}
{{- define "guardrails.podRules" -}}
resourceRules:
  - apiGroups: [""]
    apiVersions: ["v1"]
    operations: ["CREATE", "UPDATE"]
    resources: ["pods"]
{{- end -}}
