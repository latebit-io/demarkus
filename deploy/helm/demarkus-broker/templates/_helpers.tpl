{{/*
Expand the name of the chart.
*/}}
{{- define "demarkus-broker.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Fully qualified app name.
*/}}
{{- define "demarkus-broker.fullname" -}}
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

{{- define "demarkus-broker.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "demarkus-broker.labels" -}}
helm.sh/chart: {{ include "demarkus-broker.chart" . }}
{{ include "demarkus-broker.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "demarkus-broker.selectorLabels" -}}
app.kubernetes.io/name: {{ include "demarkus-broker.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "demarkus-broker.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "demarkus-broker.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/*
Broker namespace. Defaults to the release namespace; override only when
the issuances Secret deliberately lives elsewhere (rare).
*/}}
{{- define "demarkus-broker.brokerNamespace" -}}
{{- default .Release.Namespace .Values.server.brokerNamespace -}}
{{- end -}}

{{/*
Names for the chart-managed Secrets.
*/}}
{{- define "demarkus-broker.configSecretName" -}}
{{- printf "%s-config" (include "demarkus-broker.fullname" .) -}}
{{- end -}}

{{- define "demarkus-broker.issuancesSecretName" -}}
{{- default (printf "%s-issuances" (include "demarkus-broker.fullname" .)) .Values.server.issuancesSecret -}}
{{- end -}}

{{- define "demarkus-broker.refreshTokensSecretName" -}}
{{- default (printf "%s-refresh-tokens" (include "demarkus-broker.fullname" .)) .Values.server.refreshTokensSecret -}}
{{- end -}}

{{/*
Cookie key resolution. Order of precedence:
  1. .Values.server.cookieKey (operator-supplied literal)
  2. Existing config Secret (preserves the key across helm upgrades)
  3. Freshly generated 32-byte key, base64-encoded

Generated only ONCE on first install; subsequent helm-upgrades read the
existing Secret via `lookup` so the cookie key — and therefore in-flight
state cookies — survive chart updates.

NOTE: `lookup` returns nil during `helm template` (no cluster context),
so a fresh `randAlphaNum` runs on every offline render. Tests should not
assert on exact key values, only on structure.
*/}}
{{- define "demarkus-broker.resolveCookieKey" -}}
{{- if .Values.server.cookieKey -}}
{{- .Values.server.cookieKey -}}
{{- else -}}
{{- $existing := lookup "v1" "Secret" .Release.Namespace (include "demarkus-broker.configSecretName" .) -}}
{{- if and $existing (index $existing.data "cookie-key") -}}
{{- index $existing.data "cookie-key" | b64dec -}}
{{- else -}}
{{- randAlphaNum 32 | b64enc -}}
{{- end -}}
{{- end -}}
{{- end -}}
