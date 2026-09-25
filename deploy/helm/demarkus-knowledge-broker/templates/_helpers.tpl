{{/*
Expand the name of the chart.
*/}}
{{- define "demarkus-knowledge-broker.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* Release name unless overridden; the umbrella pins "broker". */}}
{{- define "demarkus-knowledge-broker.fullname" -}}
{{- default .Release.Name .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "demarkus-knowledge-broker.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "demarkus-knowledge-broker.labels" -}}
helm.sh/chart: {{ include "demarkus-knowledge-broker.chart" . }}
{{ include "demarkus-knowledge-broker.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "demarkus-knowledge-broker.selectorLabels" -}}
app.kubernetes.io/name: {{ include "demarkus-knowledge-broker.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "demarkus-knowledge-broker.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "demarkus-knowledge-broker.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/*
Broker namespace. Defaults to the release namespace; override only when
the broker's state Secrets deliberately live elsewhere (rare).
*/}}
{{- define "demarkus-knowledge-broker.brokerNamespace" -}}
{{- default .Release.Namespace .Values.server.brokerNamespace -}}
{{- end -}}

{{/*
Names for the chart-managed Secrets.
*/}}
{{- define "demarkus-knowledge-broker.configSecretName" -}}
{{- printf "%s-config" (include "demarkus-knowledge-broker.fullname" .) -}}
{{- end -}}

{{- define "demarkus-knowledge-broker.refreshTokensSecretName" -}}
{{- default (printf "%s-refresh-tokens" (include "demarkus-knowledge-broker.fullname" .)) .Values.server.refreshTokensSecret -}}
{{- end -}}

{{/* Signing-key Secret: the broker generates and persists its ECDSA key here on first start when no key is configured. */}}
{{- define "demarkus-knowledge-broker.signingKeySecretName" -}}
{{- default (printf "%s-signing-key" (include "demarkus-knowledge-broker.fullname" .)) .Values.server.signingKeySecret -}}
{{- end -}}

{{/* Env var carrying webClients[i]'s secret; rendered into the config and the pod alike. */}}
{{- define "demarkus-knowledge-broker.webClientSecretEnv" -}}
{{- printf "WEB_CLIENT_SECRET_%d" (int .) -}}
{{- end -}}

{{/* Dynamic-clients Secret: RFC 7591 registrations (client_id -> redirect URIs). */}}
{{- define "demarkus-knowledge-broker.dynamicClientsSecretName" -}}
{{- default (printf "%s-dynamic-clients" (include "demarkus-knowledge-broker.fullname" .)) .Values.server.dynamicClientsSecret -}}
{{- end -}}

{{/*
Per-world write-token Secret name. The broker provisions one of
these lazily on the first write to each world (see
worldWriteTokenStore.Provision in
tools/internal/broker/gateway/world_write_tokens.go). The
prefix is hardcoded in the Go side — keep this helper byte-for-byte
in sync; a drift here surfaces at runtime as a "forbidden" on the
broker's first write attempt and is exactly the kind of silent
misconfig RBAC is supposed to catch up front.
*/}}
{{- define "demarkus-knowledge-broker.writeTokenSecretName" -}}
{{- printf "demarkus-broker-write-token-%s" .worldName -}}
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
{{- define "demarkus-knowledge-broker.mcpTLSMountPath" -}}
/etc/demarkus-knowledge-broker/tls/mcp
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
{{- define "demarkus-knowledge-broker.mcpPort" -}}
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
{{- define "demarkus-knowledge-broker.resolveCookieKey" -}}
{{- if .Values.server.cookieKey -}}
{{- .Values.server.cookieKey -}}
{{- else -}}
{{- $existing := lookup "v1" "Secret" .Release.Namespace (include "demarkus-knowledge-broker.configSecretName" .) -}}
{{- if and $existing (index $existing.data "cookie-key") -}}
{{- index $existing.data "cookie-key" | b64dec -}}
{{- else -}}
{{- randAlphaNum 32 | b64enc -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/* Namespace of the world token Secrets when a world sets none. */}}
{{- define "demarkus-knowledge-broker.worldsNamespace" -}}
{{- default .Release.Namespace .Values.worldDefaults.namespace -}}
{{- end -}}

{{/*
Socket address every derived world is dialed at (host:port), or empty when
each world resolves its own Service: worldDefaults.dialAddress, else the
umbrella's global.knowledgeService Service in the worlds namespace.
*/}}
{{- define "demarkus-knowledge-broker.dialAddress" -}}
{{- $global := default dict .Values.global -}}
{{- if .Values.worldDefaults.dialAddress -}}
{{- .Values.worldDefaults.dialAddress -}}
{{- else if $global.knowledgeService -}}
{{- printf "%s.%s.svc.cluster.local:6309" $global.knowledgeService (include "demarkus-knowledge-broker.worldsNamespace" .) -}}
{{- end -}}
{{- end -}}

{{/*
Authority suffix for derived internalAddress (<name>.<authorityDomain>:6309),
or empty to leave the broker's <name>.<namespace>.svc.cluster.local default.
Mirrors the knowledge-server chart's default so SNI matches its certificate.
*/}}
{{- define "demarkus-knowledge-broker.authorityDomain" -}}
{{- $global := default dict .Values.global -}}
{{- if .Values.worldDefaults.authorityDomain -}}
{{- .Values.worldDefaults.authorityDomain -}}
{{- else if $global.authorityDomain -}}
{{- $global.authorityDomain -}}
{{- else if $global.knowledgeService -}}
{{- printf "%s.%s.svc.cluster.local" $global.knowledgeService (include "demarkus-knowledge-broker.worldsNamespace" .) -}}
{{- end -}}
{{- end -}}

{{/*
Resolved world list as YAML. Source is .Values.worlds, else global.worlds
(umbrella). Each entry is merged over worldDefaults, then namespace,
tokensSecret (<name>-tokens), internalAddress and dialAddress are filled.
Consumers read named fields: include ... | fromYamlArray.
*/}}
{{- define "demarkus-knowledge-broker.worlds" -}}
{{- $global := default dict .Values.global -}}
{{- $source := .Values.worlds -}}
{{- if empty $source -}}{{- $source = default list $global.worlds -}}{{- end -}}
{{- $namespace := include "demarkus-knowledge-broker.worldsNamespace" . -}}
{{- $dialAddress := include "demarkus-knowledge-broker.dialAddress" . -}}
{{- $authorityDomain := include "demarkus-knowledge-broker.authorityDomain" . -}}
{{- $worlds := list -}}
{{- range $configured := $source -}}
{{- $world := mergeOverwrite (deepCopy $.Values.worldDefaults) (deepCopy (default dict $configured)) -}}
{{- $name := default "" $world.name -}}
{{- if empty $world.namespace -}}{{- $_ := set $world "namespace" $namespace -}}{{- end -}}
{{- if empty $world.tokensSecret -}}{{- $_ := set $world "tokensSecret" (printf "%s-tokens" $name) -}}{{- end -}}
{{- if and (empty $world.internalAddress) $authorityDomain -}}
{{- $_ := set $world "internalAddress" (printf "%s.%s:6309" $name $authorityDomain) -}}
{{- end -}}
{{- if and (empty $world.dialAddress) $dialAddress -}}{{- $_ := set $world "dialAddress" $dialAddress -}}{{- end -}}
{{- $worlds = append $worlds $world -}}
{{- end -}}
{{- toYaml $worlds -}}
{{- end -}}
