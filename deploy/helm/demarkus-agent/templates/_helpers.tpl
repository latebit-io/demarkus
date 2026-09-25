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
{{- $flagged := list -}}
{{- range default list $global.worlds }}{{ if .hub }}{{ $flagged = append $flagged .name }}{{ end }}{{ end -}}
{{- if gt (len $flagged) 1 -}}
{{- fail (printf "global.worlds flags %d hubs (%s); exactly one world may set hub: true" (len $flagged) (join ", " $flagged)) -}}
{{- end -}}
{{- first $flagged | default "" -}}
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

{{/*
Hubs: config.hubs when set (an explicit [] is crawl-only), else mark://<hub>.
Topology from global.worlds must name a hub.
*/}}
{{- define "demarkus-agent.hubs" -}}
{{- $hubs := .Values.config.hubs -}}
{{- if kindIs "invalid" $hubs -}}
{{- $hubs = list -}}
{{- $hub := include "demarkus-agent.hub" . -}}
{{- if $hub -}}
{{- $hubs = append $hubs (printf "mark://%s" $hub) -}}
{{- else if and (empty .Values.worlds) (default dict .Values.global).worlds -}}
{{- fail "global.worlds needs one world with hub: true (or set agent.config.hubs: [] for crawl-only)" -}}
{{- end -}}
{{- end -}}
{{- toYaml $hubs -}}
{{- end -}}

{{/*
Endpoints: one per world (hub included) when a shared dial address is
known, with the SNI the knowledge server certificate carries;
a config.endpoints entry replaces its authority's derived entry whole.
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
{{- range $authority, $endpoint := default dict .Values.config.endpoints -}}
{{- $_ := set $endpoints $authority $endpoint -}}
{{- end -}}
{{- toYaml $endpoints -}}
{{- end -}}

{{/*
World token Secrets: tokens.fromWorldSecrets, else the hub's
<hub>-token-values (the knowledge-server chart's raw admin token) unless
tokens.existingSecret is set or tokens.inline already holds <hub>:6309.
*/}}
{{- define "demarkus-agent.fromWorldSecrets" -}}
{{- $sources := .Values.tokens.fromWorldSecrets -}}
{{- $hub := include "demarkus-agent.hub" . -}}
{{- if and (empty $sources) $hub (empty .Values.tokens.existingSecret) -}}
{{- $hostPort := printf "%s:6309" $hub -}}
{{- if not (hasKey (default dict .Values.tokens.inline) $hostPort) -}}
{{- $sources = list (dict "hostPort" $hostPort "secret" (printf "%s-token-values" $hub) "key" "admin") -}}
{{- end -}}
{{- end -}}
{{- toYaml (default list $sources) -}}
{{- end -}}
