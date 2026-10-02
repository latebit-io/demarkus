{{- define "demarkus-knowledge-server.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Release name unless overridden. The umbrella's global.knowledgeService names
this Service for the agent, so it names these resources too.
*/}}
{{- define "demarkus-knowledge-server.fullname" -}}
{{- $global := default dict .Values.global -}}
{{- default (default .Release.Name $global.knowledgeService) .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "demarkus-knowledge-server.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "demarkus-knowledge-server.labels" -}}
helm.sh/chart: {{ include "demarkus-knowledge-server.chart" . }}
{{ include "demarkus-knowledge-server.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "demarkus-knowledge-server.selectorLabels" -}}
app.kubernetes.io/name: {{ include "demarkus-knowledge-server.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "demarkus-knowledge-server.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "demarkus-knowledge-server.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{- define "demarkus-knowledge-server.configName" -}}
{{- printf "%s-config" (include "demarkus-knowledge-server.fullname" . | trunc 56 | trimSuffix "-") -}}
{{- end -}}

{{- define "demarkus-knowledge-server.tlsSecretName" -}}
{{- if .Values.tls.existingSecret -}}
{{- .Values.tls.existingSecret -}}
{{- else -}}
{{- printf "%s-tls" (include "demarkus-knowledge-server.fullname" . | trunc 59 | trimSuffix "-") -}}
{{- end -}}
{{- end -}}

{{/* Name of the chart-rendered self-signed Issuer. */}}
{{- define "demarkus-knowledge-server.selfSignedIssuerName" -}}
{{- printf "%s-selfsigned" (include "demarkus-knowledge-server.fullname" . | trunc 52 | trimSuffix "-") -}}
{{- end -}}

{{- define "demarkus-knowledge-server.policyKey" -}}
{{- printf "policy-%s.md" . -}}
{{- end -}}

{{/*
Authority suffix for derived world authorities: worldDefaults.authorityDomain,
else global.authorityDomain, else this Service's cluster DNS name (the same
suffix the dynamic-worlds wildcard certificate covers).
*/}}
{{- define "demarkus-knowledge-server.authorityDomain" -}}
{{- $global := default dict .Values.global -}}
{{- $configured := default (default "" $global.authorityDomain) .Values.worldDefaults.authorityDomain -}}
{{- default (printf "%s.%s.svc.cluster.local" (include "demarkus-knowledge-server.fullname" .) .Release.Namespace) $configured -}}
{{- end -}}

{{- define "demarkus-knowledge-server.bucketPrefix" -}}
{{- $global := default dict .Values.global -}}
{{- default (default "" $global.bucketPrefix) .Values.worldDefaults.bucketPrefix -}}
{{- end -}}

{{/*
Resolved world list as YAML. Source is .Values.worlds, else global.worlds
(umbrella). Each entry is merged over worldDefaults, then the derivable
fields are filled: authorities [<name>.<authorityDomain>], bucket.url
<bucketPrefix><name>, bucket.worldID from a top-level worldID, tokenSecret
<name>-tokens / tokens.toml, staticTokenSecret <name>-static-tokens /
tokens.toml, profile knowledge. Consumers read named fields: include ... |
fromYamlArray.
*/}}
{{- define "demarkus-knowledge-server.worlds" -}}
{{- $global := default dict .Values.global -}}
{{- $source := .Values.worlds -}}
{{- if empty $source -}}{{- $source = default list $global.worlds -}}{{- end -}}
{{- $authorityDomain := include "demarkus-knowledge-server.authorityDomain" . -}}
{{- $bucketPrefix := include "demarkus-knowledge-server.bucketPrefix" . -}}
{{- $worlds := list -}}
{{- range $configured := $source -}}
{{- $world := mergeOverwrite (deepCopy $.Values.worldDefaults) (deepCopy (default dict $configured)) -}}
{{- $name := default "" $world.name -}}
{{- if empty $world.authorities -}}
{{- $_ := set $world "authorities" (list (printf "%s.%s" $name $authorityDomain)) -}}
{{- end -}}
{{- $bucket := default dict $world.bucket -}}
{{- if and (empty $bucket.url) $bucketPrefix $name -}}
{{- $_ := set $bucket "url" (printf "%s%s" $bucketPrefix $name) -}}
{{- end -}}
{{- if and (empty $bucket.worldID) $world.worldID -}}
{{- $_ := set $bucket "worldID" $world.worldID -}}
{{- end -}}
{{- $_ := set $world "bucket" $bucket -}}
{{- range $field, $suffix := dict "tokenSecret" "-tokens" "staticTokenSecret" "-static-tokens" -}}
{{- $secret := default dict (get $world $field) -}}
{{- if empty $secret.name -}}
{{- $_ := set $secret "name" (printf "%s%s" $name $suffix) -}}
{{- end -}}
{{- if empty $secret.key -}}
{{- $_ := set $secret "key" "tokens.toml" -}}
{{- end -}}
{{- $_ := set $world $field $secret -}}
{{- end -}}
{{- if empty $world.profile -}}{{- $_ := set $world "profile" "knowledge" -}}{{- end -}}
{{- $worlds = append $worlds $world -}}
{{- end -}}
{{- toYaml $worlds -}}
{{- end -}}

{{- define "demarkus-knowledge-server.validate" -}}
{{- $global := default dict .Values.global -}}
{{- if and $global.knowledgeService .Values.fullnameOverride (ne $global.knowledgeService .Values.fullnameOverride) -}}
{{- fail (printf "fullnameOverride %q differs from global.knowledgeService %q; the broker and agent dial the latter" .Values.fullnameOverride $global.knowledgeService) -}}
{{- end -}}
{{- if lt (int .Values.replicaCount) 2 -}}
{{- fail "replicaCount must be at least 2 for production availability" -}}
{{- end -}}
{{- $udpPort := int .Values.server.udpPort -}}
{{- $healthPort := int .Values.server.healthPort -}}
{{- $peerPort := int .Values.server.peerPort -}}
{{- $httpPort := int .Values.server.httpPort -}}
{{- $bearerPort := int .Values.server.bearerPort -}}
{{- range $name, $port := dict "server.udpPort" $udpPort "server.healthPort" $healthPort "server.peerPort" $peerPort "server.httpPort" $httpPort "server.bearerPort" $bearerPort "service.bearerPort" (int .Values.service.bearerPort) -}}
{{- if or (lt $port 1) (gt $port 65535) -}}
{{- fail (printf "%s must be between 1 and 65535" $name) -}}
{{- end -}}
{{- end -}}
{{- if eq $peerPort $udpPort -}}
{{- fail "server.peerPort must differ from server.udpPort" -}}
{{- end -}}
{{- if eq $udpPort $healthPort -}}
{{- fail "server.healthPort must differ from server.udpPort" -}}
{{- end -}}
{{- if or (eq $bearerPort $udpPort) (eq $bearerPort $peerPort) -}}
{{- fail "server.bearerPort must differ from server.udpPort and server.peerPort" -}}
{{- end -}}
{{- if eq (int .Values.service.bearerPort) $udpPort -}}
{{- fail "service.bearerPort must differ from server.udpPort" -}}
{{- end -}}
{{- if lt (int .Values.server.maxIncomingStreams) 1 -}}
{{- fail "server.maxIncomingStreams must be positive" -}}
{{- end -}}
{{- if and .Values.tls.existingSecret .Values.tls.certManager.enabled -}}
{{- fail "tls.existingSecret and tls.certManager.enabled are mutually exclusive; set exactly one" -}}
{{- end -}}
{{- if not (or .Values.tls.existingSecret .Values.tls.certManager.enabled) -}}
{{- fail "tls.existingSecret or tls.certManager.enabled is required" -}}
{{- end -}}
{{- if .Values.tls.certManager.enabled -}}
{{- if and .Values.tls.certManager.selfSigned.create .Values.tls.certManager.issuerRef.name -}}
{{- fail "tls.certManager.selfSigned.create and tls.certManager.issuerRef.name are mutually exclusive; set exactly one" -}}
{{- end -}}
{{- if not (or .Values.tls.certManager.selfSigned.create .Values.tls.certManager.issuerRef.name) -}}
{{- fail "tls.certManager.issuerRef.name is required when cert-manager is enabled (or set tls.certManager.selfSigned.create)" -}}
{{- end -}}
{{- end -}}
{{- if .Values.serviceAccount.create -}}
{{- if empty .Values.serviceAccount.workloadIdentity.gsa -}}
{{- fail "serviceAccount.workloadIdentity.gsa is required when serviceAccount.create is true" -}}
{{- end -}}
{{- else if empty .Values.serviceAccount.name -}}
{{- fail "serviceAccount.name is required when serviceAccount.create is false" -}}
{{- end -}}
{{- if .Values.networkPolicy.enabled -}}
{{- if empty .Values.networkPolicy.ingressFromNamespace -}}
{{- fail "networkPolicy.ingressFromNamespace is required when NetworkPolicy is enabled" -}}
{{- end -}}
{{- if or (empty .Values.networkPolicy.agent.namespace) (empty .Values.networkPolicy.agent.podLabels) -}}
{{- fail "networkPolicy.agent.namespace and podLabels are required when NetworkPolicy is enabled" -}}
{{- end -}}
{{- if not .Values.networkPolicy.allowUnrestrictedHTTPS -}}
{{- fail "networkPolicy.allowUnrestrictedHTTPS must be true when built-in NetworkPolicy is enabled; otherwise disable it and provide CNI or proxy egress controls" -}}
{{- end -}}
{{- if and .Values.networkPolicy.externalCIDRs (ne .Values.service.externalTrafficPolicy "Local") -}}
{{- fail "service.externalTrafficPolicy must be Local when networkPolicy.externalCIDRs is set" -}}
{{- end -}}
{{- end -}}
{{- if or (eq $httpPort $healthPort) (eq $httpPort $udpPort) -}}
{{- fail "server.httpPort must differ from server.healthPort and server.udpPort" -}}
{{- end -}}
{{- include "demarkus-knowledge-server.validateBroker" . -}}
{{- $worlds := include "demarkus-knowledge-server.worlds" . | fromYamlArray -}}
{{- if and (empty $worlds) (not (include "demarkus-knowledge-server.provisioningEnabled" .)) -}}
{{- fail "worlds (or global.worlds) must contain at least one world, or enable provisioning" -}}
{{- end -}}
{{- $names := dict -}}
{{- $authorities := dict -}}
{{- $buckets := dict -}}
{{- $worldIDs := dict -}}
{{- $tokenSecrets := dict -}}
{{- range $index, $world := $worlds -}}
{{- $bucket := default dict $world.bucket -}}
{{- $location := printf "worlds[%d]" $index -}}
{{- if empty $world.name -}}
{{- fail (printf "%s.name is required" $location) -}}
{{- end -}}
{{- if or (gt (len $world.name) 63) (not (regexMatch "^[a-z0-9]([-a-z0-9]*[a-z0-9])?$" $world.name)) -}}
{{- fail (printf "%s.name must be a valid lowercase DNS label" $location) -}}
{{- end -}}
{{- if hasKey $names $world.name -}}
{{- fail (printf "%s.name %q is duplicated" $location $world.name) -}}
{{- end -}}
{{- $_ := set $names $world.name $world -}}
{{- range $authorityIndex, $authority := $world.authorities -}}
{{- if empty $authority -}}
{{- fail (printf "%s.authorities[%d] is required" $location $authorityIndex) -}}
{{- end -}}
{{- $normalized := lower $authority -}}
{{- if or (gt (len $normalized) 253) (not (regexMatch "^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$" $normalized)) (contains ".." $normalized) -}}
{{- fail (printf "%s.authorities[%d] %q must be a DNS hostname without a port" $location $authorityIndex $authority) -}}
{{- end -}}
{{- range $label := splitList "." $normalized -}}
{{- if or (gt (len $label) 63) (not (regexMatch "^[a-z0-9]([-a-z0-9]*[a-z0-9])?$" $label)) -}}
{{- fail (printf "%s.authorities[%d] %q must contain valid DNS labels" $location $authorityIndex $authority) -}}
{{- end -}}
{{- end -}}
{{- if hasKey $authorities $normalized -}}
{{- fail (printf "%s.authorities[%d] %q is duplicated after normalization" $location $authorityIndex $authority) -}}
{{- end -}}
{{- $_ := set $authorities $normalized true -}}
{{- end -}}
{{- if empty $bucket.url -}}
{{- fail (printf "%s.bucket.url is required (or set worldDefaults.bucketPrefix / global.bucketPrefix)" $location) -}}
{{- end -}}
{{- include "demarkus-knowledge-server.validateBucketURL" (dict "url" $bucket.url "field" (printf "%s.bucket.url" $location)) -}}
{{- if hasKey $buckets $bucket.url -}}
{{- fail (printf "%s.bucket.url %q is duplicated" $location $bucket.url) -}}
{{- end -}}
{{- $_ := set $buckets $bucket.url true -}}
{{- if empty $bucket.worldID -}}
{{- fail (printf "%s.worldID is required" $location) -}}
{{- end -}}
{{- if not (regexMatch "^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$" $bucket.worldID) -}}
{{- fail (printf "%s.worldID %q must be a canonical lowercase UUID with RFC 4122 variant" $location $bucket.worldID) -}}
{{- end -}}
{{- if hasKey $worldIDs $bucket.worldID -}}
{{- fail (printf "%s.worldID %q is duplicated" $location $bucket.worldID) -}}
{{- end -}}
{{- $_ := set $worldIDs $bucket.worldID true -}}
{{- /* One namespace for both: the static Secret is merged with the runtime
       one, so a shared name would load the same entries twice. */ -}}
{{- range $field := list "tokenSecret" "staticTokenSecret" -}}
{{- $secretName := (get $world $field).name -}}
{{- if hasKey $tokenSecrets $secretName -}}
{{- fail (printf "%s.%s.name %q is duplicated" $location $field $secretName) -}}
{{- end -}}
{{- $_ := set $tokenSecrets $secretName true -}}
{{- end -}}
{{- /* A world that is never seeded would take the policy body silently. */ -}}
{{- if and $world.initialPolicy $world.readOnly -}}
{{- fail (printf "%s.initialPolicy must not be set on a read-only world, which is never seeded" $location) -}}
{{- end -}}
{{- if empty (default dict $world.writeScope).paths -}}
{{- fail (printf "%s.writeScope.paths is required (or set worldDefaults.writeScope.paths)" $location) -}}
{{- end -}}
{{- $allow := default dict $world.allow -}}
{{- if not (or (eq $world.profile "knowledge") (eq $world.profile "memory")) -}}
{{- fail (printf "%s.profile must be knowledge or memory (got %q)" $location $world.profile) -}}
{{- end -}}
{{- if and (eq $world.profile "memory") (empty $.Values.broker.memory.publicURL) -}}
{{- fail (printf "%s.profile memory needs the memory gateway (broker.memory.publicURL)" $location) -}}
{{- end -}}
{{- if and (eq $world.profile "memory") (not (or $allow.domains $allow.groups $allow.emails)) -}}
{{- fail (printf "%s.allow must name the tenant identity (domains, groups or emails) on a memory world" $location) -}}
{{- end -}}
{{- end -}}
{{- $hubName := include "demarkus-knowledge-server.federationHub" . -}}
{{- if $hubName -}}
{{- $hub := get $names $hubName -}}
{{- if or (empty $hub) (ne $hub.profile "knowledge") -}}
{{- fail (printf "federation hub %q must name a knowledge world in worlds (or global.worlds)" $hubName) -}}
{{- end -}}
{{- if $hub.readOnly -}}
{{- fail (printf "federation hub %q is read-only; the deriver writes its checkpoints there" $hubName) -}}
{{- end -}}
{{- /* The Role names the Lease, so the broker's default cannot fill it. */ -}}
{{- if empty .Values.broker.federation.leaseName -}}
{{- fail "broker.federation.leaseName is required when federation has a hub" -}}
{{- end -}}
{{- end -}}
{{- /* The bootstrap Job generates one <world>-token-values Secret per world;
       a generated name colliding with a tokenSecret.name or another
       chart-owned Secret would hand a consumer the wrong data. */ -}}
{{- if and .Values.tokens.bootstrap.enabled .Values.tokens.emitRawValues -}}
{{- $reserved := dict -}}
{{- if include "demarkus-knowledge-server.provisioningEnabled" . -}}
{{- $_ := set $reserved .Values.provisioning.worldsSecret "provisioning.worldsSecret" -}}
{{- end -}}
{{- $_ := set $reserved (include "demarkus-knowledge-server.brokerConfigSecretName" .) "the broker config Secret" -}}
{{- $_ := set $reserved (include "demarkus-knowledge-server.signingKeySecretName" .) "broker.signingKeySecret" -}}
{{- $_ := set $reserved (include "demarkus-knowledge-server.cookieKeySecretName" .) "broker.cookieKeySecret" -}}
{{- $_ := set $reserved (include "demarkus-knowledge-server.dynamicClientsSecretName" .) "broker.dynamicClientsSecret" -}}
{{- $_ := set $reserved (include "demarkus-knowledge-server.oauthStateSecretName" .) "broker.oauthStateSecret" -}}
{{- if .Values.tls.existingSecret -}}
{{- $_ := set $reserved .Values.tls.existingSecret "tls.existingSecret" -}}
{{- end -}}
{{- range $index, $world := $worlds -}}
{{- $rawName := printf "%s-token-values" $world.name -}}
{{- if hasKey $tokenSecrets $rawName -}}
{{- fail (printf "worlds[%d]: generated raw Secret name %q collides with a tokenSecret.name or staticTokenSecret.name" $index $rawName) -}}
{{- end -}}
{{- if hasKey $reserved $rawName -}}
{{- fail (printf "worlds[%d]: generated raw Secret name %q collides with %s" $index $rawName (get $reserved $rawName)) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
Admin token paths/operations, rendered as a quoted, comma-joined TOML array
body (e.g. `"/**"` or `"publish"`). Used by the token-bootstrap Job to build
the admin entry in tokens.toml.
*/}}
{{- define "demarkus-knowledge-server.tokenAdminPaths" -}}
{{- range $i, $p := .Values.tokens.admin.paths }}{{ if $i }}, {{ end }}{{ $p | quote }}{{- end -}}
{{- end -}}
{{- define "demarkus-knowledge-server.tokenAdminOps" -}}
{{- range $i, $o := .Values.tokens.admin.operations }}{{ if $i }}, {{ end }}{{ $o | quote }}{{- end -}}
{{- end -}}

{{/* Broker state Secrets in the release namespace. */}}
{{- define "demarkus-knowledge-server.brokerConfigSecretName" -}}
{{- printf "%s-broker-config" (include "demarkus-knowledge-server.fullname" . | trunc 49 | trimSuffix "-") -}}
{{- end -}}

{{- define "demarkus-knowledge-server.cookieKeySecretName" -}}
{{- default (printf "%s-cookie-key" (include "demarkus-knowledge-server.fullname" .)) .Values.broker.cookieKeySecret -}}
{{- end -}}

{{- define "demarkus-knowledge-server.signingKeySecretName" -}}
{{- default (printf "%s-signing-key" (include "demarkus-knowledge-server.fullname" .)) .Values.broker.signingKeySecret -}}
{{- end -}}

{{- define "demarkus-knowledge-server.dynamicClientsSecretName" -}}
{{- default (printf "%s-dynamic-clients" (include "demarkus-knowledge-server.fullname" .)) .Values.broker.dynamicClientsSecret -}}
{{- end -}}

{{- define "demarkus-knowledge-server.oauthStateSecretName" -}}
{{- default (printf "%s-oauth-state" (include "demarkus-knowledge-server.fullname" .)) .Values.broker.oauthStateSecret -}}
{{- end -}}

{{/* Env var carrying webClients[i]'s secret; rendered into the config and the pod alike. */}}
{{- define "demarkus-knowledge-server.webClientSecretEnv" -}}
{{- printf "WEB_CLIENT_SECRET_%d" (int .) -}}
{{- end -}}

{{/* The binary's default registry Secret, kept so an upgrade finds its tenants. */}}
{{- define "demarkus-knowledge-server.registrySecretName" -}}
{{- default "demarkus-memory-broker-registry" .Values.provisioning.registrySecret -}}
{{- end -}}

{{/* Non-empty when provisioning.mode is allowlisted or open. */}}
{{- define "demarkus-knowledge-server.provisioningEnabled" -}}
{{- $mode := default "static" .Values.provisioning.mode -}}
{{- if or (eq $mode "allowlisted") (eq $mode "open") -}}true{{- end -}}
{{- end -}}

{{/* The federation hub: broker.federation.hub, else the one world marked hub; empty is off. */}}
{{- define "demarkus-knowledge-server.federationHub" -}}
{{- $marked := list -}}
{{- range include "demarkus-knowledge-server.worlds" . | fromYamlArray -}}
{{- if .hub -}}{{- $marked = append $marked .name -}}{{- end -}}
{{- end -}}
{{- if gt (len $marked) 1 -}}
{{- fail (printf "worlds %v are all marked hub; at most one world is the federation hub" $marked) -}}
{{- end -}}
{{- default (first $marked | default "") .Values.broker.federation.hub -}}
{{- end -}}

{{/*
Cookie key rendered into the config, or empty to let the broker generate
and persist one in cookieKeySecret: broker.cookieKey, else the key an
earlier chart rendered into the live config Secret (found only by `helm
upgrade`; `helm template` renders empty, so the output is deterministic).
Empty with existingCookieKeyRef, which the broker reads from the env.
*/}}
{{- define "demarkus-knowledge-server.resolveCookieKey" -}}
{{- if .Values.broker.cookieKey -}}
{{- .Values.broker.cookieKey -}}
{{- else if not .Values.broker.existingCookieKeyRef.name -}}
{{- $existing := lookup "v1" "Secret" .Release.Namespace (include "demarkus-knowledge-server.brokerConfigSecretName" .) -}}
{{- if and $existing (index (default dict $existing.data) "cookie-key") -}}
{{- index $existing.data "cookie-key" | b64dec -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/* Hostname of a URL, lowercased; empty for an empty URL. */}}
{{- define "demarkus-knowledge-server.urlHost" -}}
{{- if . -}}
{{- (urlParse .).hostname | lower -}}
{{- end -}}
{{- end -}}

{{/* The knowledge gateway's URL: broker.mcp.publicURL, else the issuer. */}}
{{- define "demarkus-knowledge-server.knowledgeGatewayURL" -}}
{{- default .Values.broker.publicURL .Values.broker.mcp.publicURL -}}
{{- end -}}

{{/* One allow predicate as the broker config's three lists; takes the allow dict. */}}
{{- define "demarkus-knowledge-server.allowLists" -}}
{{- $allow := default (dict) . -}}
allow:
  domains: {{ default (list) (get $allow "domains") | toJson }}
  groups: {{ default (list) (get $allow "groups") | toJson }}
  emails: {{ default (list) (get $allow "emails") | toJson }}
{{- end -}}

{{/*
A gs:// bucket URL that GCS would accept: the world buckets and the broker's
state bucket share it. Takes a dict with url and field, the value's name.
*/}}
{{- define "demarkus-knowledge-server.validateBucketURL" -}}
{{- $url := .url -}}
{{- $field := .field -}}
{{- $bucketName := trimPrefix "gs://" $url -}}
{{- $maximumBucketLength := 63 -}}
{{- if contains "." $bucketName -}}
{{- $maximumBucketLength = 222 -}}
{{- end -}}
{{- if or (not (hasPrefix "gs://" $url)) (lt (len $bucketName) 3) (gt (len $bucketName) $maximumBucketLength) (not (regexMatch "^[a-z0-9]([a-z0-9._-]*[a-z0-9])?$" $bucketName)) -}}
{{- fail (printf "%s %q must be an exact lowercase gs://bucket URL" $field $url) -}}
{{- end -}}
{{- range $component := splitList "." $bucketName -}}
{{- if or (gt (len $component) 63) (not (regexMatch "^[a-z0-9]([-_a-z0-9]*[a-z0-9])?$" $component)) -}}
{{- fail (printf "%s %q must contain valid GCS bucket components" $field $url) -}}
{{- end -}}
{{- end -}}
{{- $bucketComponents := splitList "." $bucketName -}}
{{- $dottedIPv4 := eq (len $bucketComponents) 4 -}}
{{- if $dottedIPv4 -}}
{{- range $component := $bucketComponents -}}
{{- if or (not (regexMatch "^(0|[1-9][0-9]{0,2})$" $component)) (gt (atoi $component) 255) -}}
{{- $dottedIPv4 = false -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- if $dottedIPv4 -}}
{{- fail (printf "%s %q must not use an IPv4 address as a bucket name" $field $url) -}}
{{- end -}}
{{- if or (hasPrefix "goog" $bucketName) (contains "google" $bucketName) (contains "g00gle" $bucketName) (contains "go0gle" $bucketName) (contains "g0ogle" $bucketName) -}}
{{- fail (printf "%s %q uses a reserved GCS bucket name" $field $url) -}}
{{- end -}}
{{- end -}}

{{/*
Broker settings the binary checks at load, caught at render so a typo reads
as such instead of a CrashLoopBackOff.
*/}}
{{- define "demarkus-knowledge-server.validateBroker" -}}
{{- $b := .Values.broker -}}
{{- if empty $b.publicURL -}}
{{- fail "broker.publicURL is required" -}}
{{- end -}}
{{- include "demarkus-knowledge-server.validateBucketURL" (dict "url" (default "" $b.stateBucket) "field" "broker.stateBucket") -}}
{{- range (include "demarkus-knowledge-server.worlds" . | fromYamlArray) -}}
{{- if eq .bucket.url $b.stateBucket -}}
{{- fail (printf "broker.stateBucket %q is world %q's bucket; the broker needs its own" $b.stateBucket .name) -}}
{{- end -}}
{{- end -}}
{{- if empty $b.oidc.issuer -}}
{{- fail "broker.oidc.issuer is required" -}}
{{- end -}}
{{- if empty $b.oidc.clientID -}}
{{- fail "broker.oidc.clientID is required" -}}
{{- end -}}
{{- if empty $b.oidc.redirectURL -}}
{{- fail "broker.oidc.redirectURL is required" -}}
{{- end -}}
{{- if and $b.oidc.clientSecret $b.oidc.existingSecretRef.name -}}
{{- fail "broker.oidc.clientSecret and broker.oidc.existingSecretRef.name are mutually exclusive; set exactly one" -}}
{{- end -}}
{{- if and (not $b.oidc.clientSecret) (not $b.oidc.existingSecretRef.name) -}}
{{- fail "broker.oidc.clientSecret or broker.oidc.existingSecretRef.name is required" -}}
{{- end -}}
{{- if and $b.oidc.existingSecretRef.name (not $b.oidc.existingSecretRef.key) -}}
{{- fail "broker.oidc.existingSecretRef.key is required when broker.oidc.existingSecretRef.name is set" -}}
{{- end -}}
{{- if and $b.oidc.brokerSigningKey $b.oidc.existingSigningKeyRef.name -}}
{{- fail "broker.oidc.brokerSigningKey and broker.oidc.existingSigningKeyRef.name are mutually exclusive; set at most one" -}}
{{- end -}}
{{- if and $b.oidc.existingSigningKeyRef.name (not $b.oidc.existingSigningKeyRef.key) -}}
{{- fail "broker.oidc.existingSigningKeyRef.key is required when broker.oidc.existingSigningKeyRef.name is set" -}}
{{- end -}}
{{- if and $b.cookieKey $b.existingCookieKeyRef.name -}}
{{- fail "broker.cookieKey and broker.existingCookieKeyRef.name are mutually exclusive; set at most one" -}}
{{- end -}}
{{- if and $b.existingCookieKeyRef.name (not $b.existingCookieKeyRef.key) -}}
{{- fail "broker.existingCookieKeyRef.key is required when broker.existingCookieKeyRef.name is set" -}}
{{- end -}}
{{- $knowledgeHost := include "demarkus-knowledge-server.urlHost" (include "demarkus-knowledge-server.knowledgeGatewayURL" .) -}}
{{- if and $b.memory.publicURL (eq (include "demarkus-knowledge-server.urlHost" $b.memory.publicURL) $knowledgeHost) -}}
{{- fail "broker.memory.publicURL must be on a different host than the knowledge gateway; the mux selects the gateway by Host" -}}
{{- end -}}
{{- range $i, $wc := $b.webClients -}}
{{- $ref := default dict $wc.existingSecretRef -}}
{{- $modes := 0 -}}
{{- if $wc.clientSecret }}{{ $modes = add1 $modes }}{{ end -}}
{{- if $wc.clientSecretHash }}{{ $modes = add1 $modes }}{{ end -}}
{{- if $ref.name }}{{ $modes = add1 $modes }}{{ end -}}
{{- if ne (int $modes) 1 -}}
{{- fail (printf "broker.webClients[%d]: set exactly one of clientSecret, clientSecretHash, existingSecretRef.name" $i) -}}
{{- end -}}
{{- end -}}
{{- range $i, $w := $b.remoteWorlds -}}
{{- if empty $w.name -}}
{{- fail (printf "broker.remoteWorlds[%d].name is required" $i) -}}
{{- end -}}
{{- if empty $w.internalAddress -}}
{{- fail (printf "broker.remoteWorlds[%d].internalAddress is required" $i) -}}
{{- end -}}
{{- end -}}
{{- $p := .Values.provisioning -}}
{{- $mode := default "static" $p.mode -}}
{{- if not (or (eq $mode "static") (eq $mode "allowlisted") (eq $mode "open")) -}}
{{- fail (printf "provisioning.mode must be static, allowlisted or open (got %q)" $mode) -}}
{{- end -}}
{{- if include "demarkus-knowledge-server.provisioningEnabled" . -}}
{{- if empty $b.memory.publicURL -}}
{{- fail "provisioning needs the memory gateway (broker.memory.publicURL)" -}}
{{- end -}}
{{- if and (eq $mode "open") (le (int $p.maxTenants) 0) -}}
{{- fail "provisioning.maxTenants must be positive in open mode" -}}
{{- end -}}
{{- if empty $p.bucketProject -}}
{{- fail "provisioning.bucketProject is required when provisioning is enabled" -}}
{{- end -}}
{{- if empty (include "demarkus-knowledge-server.bucketPrefix" .) -}}
{{- fail "worldDefaults.bucketPrefix (or global.bucketPrefix) is required when provisioning is enabled: tenant buckets are gs://<prefix><slug>" -}}
{{- end -}}
{{- if empty $p.worldsSecret -}}
{{- fail "provisioning.worldsSecret is required when provisioning is enabled" -}}
{{- end -}}
{{- end -}}
{{- if .Values.ingress.enabled -}}
{{- if empty .Values.ingress.host -}}
{{- fail "ingress.host is required when ingress.enabled is true" -}}
{{- end -}}
{{- if and .Values.ingress.tls.existingSecret .Values.ingress.tls.certManager.enabled -}}
{{- fail "ingress.tls.existingSecret and ingress.tls.certManager.enabled are mutually exclusive; set exactly one" -}}
{{- end -}}
{{- if and $b.memory.publicURL (empty .Values.ingress.memory.host) -}}
{{- fail "ingress.memory.host is required when the memory gateway is configured and the Ingress is enabled" -}}
{{- end -}}
{{- /* The mux picks a gateway by Host, so each Ingress host must be the
       hostname its gateway URL carries or requests land on the wrong one. */ -}}
{{- if and $b.memory.publicURL (ne (lower .Values.ingress.memory.host) (include "demarkus-knowledge-server.urlHost" $b.memory.publicURL)) -}}
{{- fail "ingress.memory.host must be the hostname of broker.memory.publicURL; the mux selects the memory gateway by Host" -}}
{{- end -}}
{{- if not (has $knowledgeHost (list (lower .Values.ingress.host) (lower (default "" .Values.ingress.mcp.host)))) -}}
{{- fail "the knowledge gateway's hostname (broker.mcp.publicURL, else broker.publicURL) must be ingress.host or ingress.mcp.host" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/* Distinct lowercase Ingress hosts in a stable order: management, MCP, memory. */}}
{{- define "demarkus-knowledge-server.ingressHosts" -}}
{{- $hosts := list (lower .Values.ingress.host) -}}
{{- range $host := list .Values.ingress.mcp.host .Values.ingress.memory.host -}}
{{- if and $host (not (has (lower $host) $hosts)) -}}
{{- $hosts = append $hosts (lower $host) -}}
{{- end -}}
{{- end -}}
{{- toJson $hosts -}}
{{- end -}}

{{/* The Ingress TLS Secret name, empty when the controller terminates TLS itself. */}}
{{- define "demarkus-knowledge-server.ingressTLSSecretName" -}}
{{- if .Values.ingress.tls.existingSecret -}}
{{- .Values.ingress.tls.existingSecret -}}
{{- else if .Values.ingress.tls.certManager.enabled -}}
{{- printf "%s-ingress-tls" (include "demarkus-knowledge-server.fullname" . | trunc 51 | trimSuffix "-") -}}
{{- end -}}
{{- end -}}
