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
Chart-managed mount path for the MCP gateway's TLS Secret. Single
source of truth for secret-config.yaml (renders certFile/keyFile into
the broker's config.yaml) and deployment.yaml (mounts the Secret
volume here). Not operator-facing — operators only choose the
kubernetes.io/tls Secret name; the chart owns the in-pod path.

kubernetes.io/tls Secrets always project `tls.crt` + `tls.key`, so
the rendered paths are fixed and the chart never needs to surface
key-name knobs.
*/}}
{{- define "demarkus-broker.mcpTLSMountPath" -}}
/etc/demarkus-broker/tls/mcp
{{- end -}}

{{/*
Extract the numeric port from server.mcp.addr (a `host:port` listen
string like `:8081`). Used by deployment.yaml (containerPort),
service.yaml (port + targetPort numbering), and networkpolicy.yaml
(ingress port). Single source of truth keeps the four manifests in
sync no matter how an operator overrides server.mcp.addr.

Returns the port as an integer. Fails template render with a clear
message if the addr lacks a parseable port suffix — better than
silently rendering containerPort: 0 and producing a pod that crashes
on bind.
*/}}
{{- define "demarkus-broker.mcpPort" -}}
{{- $addr := .Values.server.mcp.addr -}}
{{- if not $addr -}}
{{- fail "server.mcp.addr is required (e.g. \":8081\")" -}}
{{- end -}}
{{- $port := splitList ":" $addr | last -}}
{{- if not $port -}}
{{- fail (printf "server.mcp.addr %q has no parseable port suffix" $addr) -}}
{{- end -}}
{{- /* Sprig's `int` is a best-effort cast that silently coerces
       non-numeric strings to 0 (`{{ "abc" | int }}` → 0). Letting
       a typo in server.mcp.addr (e.g. `:abc`, `localhost`, missing
       colon) sail through would render containerPort: 0 and crash
       the pod with no breadcrumb. Validate the suffix is purely
       digits and within the legal port range BEFORE casting. */ -}}
{{- if not (regexMatch "^[0-9]+$" $port) -}}
{{- fail (printf "server.mcp.addr %q has a non-numeric port suffix %q" $addr $port) -}}
{{- end -}}
{{- $portInt := $port | int -}}
{{- if or (lt $portInt 1) (gt $portInt 65535) -}}
{{- fail (printf "server.mcp.addr %q port %d is outside the legal 1..65535 range" $addr $portInt) -}}
{{- end -}}
{{- $portInt -}}
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
