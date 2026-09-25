{{/*
Expand the name of the chart.
*/}}
{{- define "demarkus-agent.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* Release name unless overridden; the umbrella pins "agent". */}}
{{- define "demarkus-agent.fullname" -}}
{{- default .Release.Name .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
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

{{/*
Derived crawl topology. A world list (.Values.worlds, else global.worlds)
plus a hub give seeds, hubs, endpoints and the hub publish token; every
explicit config.* or tokens.* entry overrides its derived list.
*/}}
{{- define "demarkus-agent.worldNames" -}}
{{- $global := default dict .Values.global -}}
{{- $names := list -}}
{{- if .Values.worlds -}}
{{- $names = .Values.worlds -}}
{{- else -}}
{{- range default list $global.worlds }}{{ $names = append $names .name }}{{ end -}}
{{- end -}}
{{- toYaml $names -}}
{{- end -}}

{{/* Hub world: .Values.hub, else the global world flagged hub: true. */}}
{{- define "demarkus-agent.hub" -}}
{{- $global := default dict .Values.global -}}
{{- if .Values.hub -}}
{{- .Values.hub -}}
{{- else -}}
{{- range default list $global.worlds }}{{ if .hub }}{{ .name }}{{ end }}{{ end -}}
{{- end -}}
{{- end -}}

{{/* Shared socket address (host:port) for derived endpoints, or empty. */}}
{{- define "demarkus-agent.dialAddress" -}}
{{- $global := default dict .Values.global -}}
{{- if .Values.dialAddress -}}
{{- .Values.dialAddress -}}
{{- else if $global.knowledgeService -}}
{{- printf "%s.%s.svc.cluster.local:6309" $global.knowledgeService .Release.Namespace -}}
{{- end -}}
{{- end -}}

{{/* SNI suffix for derived endpoints (<name>.<authorityDomain>), or empty. */}}
{{- define "demarkus-agent.authorityDomain" -}}
{{- $global := default dict .Values.global -}}
{{- if .Values.authorityDomain -}}
{{- .Values.authorityDomain -}}
{{- else if $global.authorityDomain -}}
{{- $global.authorityDomain -}}
{{- else if $global.knowledgeService -}}
{{- printf "%s.%s.svc.cluster.local" $global.knowledgeService .Release.Namespace -}}
{{- end -}}
{{- end -}}

{{/* Seeds: config.seeds, else mark://<name> for every non-hub world. */}}
{{- define "demarkus-agent.seeds" -}}
{{- $seeds := .Values.config.seeds -}}
{{- if empty $seeds -}}
{{- $hub := include "demarkus-agent.hub" . -}}
{{- $seeds = list -}}
{{- range include "demarkus-agent.worldNames" . | fromYamlArray -}}
{{- if ne . $hub }}{{ $seeds = append $seeds (printf "mark://%s" .) }}{{ end -}}
{{- end -}}
{{- end -}}
{{- toYaml $seeds -}}
{{- end -}}

{{/* Hubs: config.hubs, else mark://<hub>. */}}
{{- define "demarkus-agent.hubs" -}}
{{- $hubs := .Values.config.hubs -}}
{{- if empty $hubs -}}
{{- $hubs = list -}}
{{- with include "demarkus-agent.hub" . }}{{ $hubs = append $hubs (printf "mark://%s" .) }}{{ end -}}
{{- end -}}
{{- toYaml $hubs -}}
{{- end -}}

{{/*
Endpoints: one per world (hub included) when a shared dial address is
known, with the SNI the knowledge server certificate carries;
config.endpoints entries win per authority.
*/}}
{{- define "demarkus-agent.endpoints" -}}
{{- $endpoints := dict -}}
{{- $dial := include "demarkus-agent.dialAddress" . -}}
{{- $domain := include "demarkus-agent.authorityDomain" . -}}
{{- if $dial -}}
{{- range include "demarkus-agent.worldNames" . | fromYamlArray -}}
{{- $endpoint := dict "dialAddress" $dial -}}
{{- if $domain }}{{ $_ := set $endpoint "serverName" (printf "%s.%s" . $domain) }}{{ end -}}
{{- $_ := set $endpoints . $endpoint -}}
{{- end -}}
{{- end -}}
{{- $endpoints = mergeOverwrite $endpoints (deepCopy (default dict .Values.config.endpoints)) -}}
{{- toYaml $endpoints -}}
{{- end -}}

{{/*
World token Secrets: tokens.fromWorldSecrets, else the hub's
<hub>-token-values (the knowledge-server chart's raw admin token) when no
other token source is configured.
*/}}
{{- define "demarkus-agent.fromWorldSecrets" -}}
{{- $sources := .Values.tokens.fromWorldSecrets -}}
{{- if and (empty $sources) (empty .Values.tokens.existingSecret) (empty .Values.tokens.inline) -}}
{{- $sources = list -}}
{{- with include "demarkus-agent.hub" . -}}
{{- $sources = append $sources (dict "hostPort" (printf "%s:6309" .) "secret" (printf "%s-token-values" .) "key" "admin") -}}
{{- end -}}
{{- end -}}
{{- toYaml (default list $sources) -}}
{{- end -}}
