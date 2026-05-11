{{/*
Expand the name of the chart.
*/}}
{{- define "demarkus-server.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Fully qualified app name.
*/}}
{{- define "demarkus-server.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "demarkus-server.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "demarkus-server.labels" -}}
helm.sh/chart: {{ include "demarkus-server.chart" . }}
{{ include "demarkus-server.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "demarkus-server.selectorLabels" -}}
app.kubernetes.io/name: {{ include "demarkus-server.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "demarkus-server.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "demarkus-server.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/*
Names for the two token Secrets.
*/}}
{{- define "demarkus-server.tokensSecretName" -}}
{{- printf "%s-tokens" (include "demarkus-server.fullname" .) -}}
{{- end -}}

{{- define "demarkus-server.tokenValuesSecretName" -}}
{{- printf "%s-token-values" (include "demarkus-server.fullname" .) -}}
{{- end -}}

{{/*
TLS Secret name. Either user-provided or chart-generated via cert-manager.
*/}}
{{- define "demarkus-server.tlsSecretName" -}}
{{- if .Values.tls.existingSecret -}}
{{- .Values.tls.existingSecret -}}
{{- else if .Values.tls.certManager.enabled -}}
{{- printf "%s-tls" (include "demarkus-server.fullname" .) -}}
{{- else -}}
{{- /* No external TLS configured; server auto-generates a dev cert. */ -}}
{{- end -}}
{{- end -}}

{{/*
Admin token resolution. Order of precedence:
  1. .Values.tokens.admin.token (user-provided literal)
  2. Existing <release>-token-values Secret (preserves token across upgrades)
  3. Freshly generated randAlphaNum 64

This helper is called from tokens.yaml where both Secrets render in the same
template pass, so $rawToken is computed once and reused across both Secrets.

NOTE: `lookup` returns nil during `helm template` (no cluster context). Tests
should not assert on exact token values, only on structure.
*/}}
{{- define "demarkus-server.resolveAdminToken" -}}
{{- if .Values.tokens.admin.token -}}
{{- .Values.tokens.admin.token -}}
{{- else -}}
{{- $existing := lookup "v1" "Secret" .Release.Namespace (include "demarkus-server.tokenValuesSecretName" .) -}}
{{- $label := .Values.tokens.admin.label -}}
{{- if and $existing (index $existing.data $label) -}}
{{- index $existing.data $label | b64dec -}}
{{- else -}}
{{- randAlphaNum 64 -}}
{{- end -}}
{{- end -}}
{{- end -}}
