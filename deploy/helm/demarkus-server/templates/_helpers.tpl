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
Admin token paths/operations, rendered as a quoted, comma-joined TOML array
body (e.g. `"/**"` or `"publish", "read"`). Used by the token-bootstrap Job to
build the admin entry in tokens.toml. Kept as helpers so the Job script and any
test render the same shape.
*/}}
{{- define "demarkus-server.tokenAdminPaths" -}}
{{- range $i, $p := .Values.tokens.admin.paths }}{{ if $i }}, {{ end }}{{ $p | quote }}{{- end -}}
{{- end -}}
{{- define "demarkus-server.tokenAdminOps" -}}
{{- range $i, $o := .Values.tokens.admin.operations }}{{ if $i }}, {{ end }}{{ $o | quote }}{{- end -}}
{{- end -}}
