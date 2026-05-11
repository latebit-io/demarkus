{{/*
Expand the name of the chart.
*/}}
{{- define "demarkus-agent.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Fully qualified app name. Truncated at 63 chars per DNS-1123.
*/}}
{{- define "demarkus-agent.fullname" -}}
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

{{/*
Chart name and version, used by chart label.
*/}}
{{- define "demarkus-agent.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Common labels applied to every resource.
*/}}
{{- define "demarkus-agent.labels" -}}
helm.sh/chart: {{ include "demarkus-agent.chart" . }}
{{ include "demarkus-agent.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{/*
Selector labels (subset of common labels used for matchLabels / selectors).
*/}}
{{- define "demarkus-agent.selectorLabels" -}}
app.kubernetes.io/name: {{ include "demarkus-agent.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/*
ServiceAccount name. Defaults to fullname when create=true and name is unset.
*/}}
{{- define "demarkus-agent.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "demarkus-agent.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/*
Tokens Secret name. Either the user-provided existingSecret or <fullname>-tokens.
*/}}
{{- define "demarkus-agent.tokensSecretName" -}}
{{- if .Values.tokens.existingSecret -}}
{{- .Values.tokens.existingSecret -}}
{{- else -}}
{{- printf "%s-tokens" (include "demarkus-agent.fullname" .) -}}
{{- end -}}
{{- end -}}
