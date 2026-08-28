{{- define "demarkus-knowledge-server.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "demarkus-knowledge-server.fullname" -}}
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

{{- define "demarkus-knowledge-server.validate" -}}
{{- if lt (int .Values.replicaCount) 2 -}}
{{- fail "replicaCount must be at least 2 for production availability" -}}
{{- end -}}
{{- $udpPort := int .Values.server.udpPort -}}
{{- $healthPort := int .Values.server.healthPort -}}
{{- if or (lt $udpPort 1) (gt $udpPort 65535) -}}
{{- fail "server.udpPort must be between 1 and 65535" -}}
{{- end -}}
{{- if or (lt $healthPort 1) (gt $healthPort 65535) -}}
{{- fail "server.healthPort must be between 1 and 65535" -}}
{{- end -}}
{{- if eq $udpPort $healthPort -}}
{{- fail "server.healthPort must differ from server.udpPort" -}}
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
{{- if and .Values.tls.certManager.enabled (empty .Values.tls.certManager.issuerRef.name) -}}
{{- fail "tls.certManager.issuerRef.name is required when cert-manager is enabled" -}}
{{- end -}}
{{- if .Values.serviceAccount.create -}}
{{- if empty .Values.serviceAccount.workloadIdentity.gsa -}}
{{- fail "serviceAccount.workloadIdentity.gsa is required when serviceAccount.create is true" -}}
{{- end -}}
{{- else if empty .Values.serviceAccount.name -}}
{{- fail "serviceAccount.name is required when serviceAccount.create is false" -}}
{{- end -}}
{{- if .Values.networkPolicy.enabled -}}
{{- if or (empty .Values.networkPolicy.broker.namespace) (empty .Values.networkPolicy.broker.podLabels) -}}
{{- fail "networkPolicy.broker.namespace and podLabels are required when NetworkPolicy is enabled" -}}
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
{{- if and (empty .Values.worlds) (not .Values.dynamicWorlds.enabled) -}}
{{- fail "worlds must contain at least one world (or enable dynamicWorlds)" -}}
{{- end -}}
{{- $names := dict -}}
{{- $authorities := dict -}}
{{- $buckets := dict -}}
{{- $worldIDs := dict -}}
{{- $tokenSecrets := dict -}}
{{- range $index, $configuredWorld := .Values.worlds -}}
{{- $world := default dict $configuredWorld -}}
{{- $bucket := default dict $world.bucket -}}
{{- $tokenSecret := default dict $world.tokenSecret -}}
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
{{- $_ := set $names $world.name true -}}
{{- if empty $world.authorities -}}
{{- fail (printf "%s.authorities must contain at least one authority" $location) -}}
{{- end -}}
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
{{- fail (printf "%s.bucket.url is required" $location) -}}
{{- end -}}
{{- $bucketName := trimPrefix "gs://" $bucket.url -}}
{{- $maximumBucketLength := 63 -}}
{{- if contains "." $bucketName -}}
{{- $maximumBucketLength = 222 -}}
{{- end -}}
{{- if or (not (hasPrefix "gs://" $bucket.url)) (lt (len $bucketName) 3) (gt (len $bucketName) $maximumBucketLength) (not (regexMatch "^[a-z0-9]([a-z0-9._-]*[a-z0-9])?$" $bucketName)) -}}
{{- fail (printf "%s.bucket.url %q must be an exact lowercase gs://bucket URL" $location $bucket.url) -}}
{{- end -}}
{{- range $component := splitList "." $bucketName -}}
{{- if or (gt (len $component) 63) (not (regexMatch "^[a-z0-9]([-_a-z0-9]*[a-z0-9])?$" $component)) -}}
{{- fail (printf "%s.bucket.url %q must contain valid GCS bucket components" $location $bucket.url) -}}
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
{{- fail (printf "%s.bucket.url %q must not use an IPv4 address as a bucket name" $location $bucket.url) -}}
{{- end -}}
{{- if or (hasPrefix "goog" $bucketName) (contains "google" $bucketName) (contains "g00gle" $bucketName) (contains "go0gle" $bucketName) (contains "g0ogle" $bucketName) -}}
{{- fail (printf "%s.bucket.url %q uses a reserved GCS bucket name" $location $bucket.url) -}}
{{- end -}}
{{- if hasKey $buckets $bucket.url -}}
{{- fail (printf "%s.bucket.url %q is duplicated" $location $bucket.url) -}}
{{- end -}}
{{- $_ := set $buckets $bucket.url true -}}
{{- if empty $bucket.worldID -}}
{{- fail (printf "%s.bucket.worldID is required" $location) -}}
{{- end -}}
{{- if not (regexMatch "^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$" $bucket.worldID) -}}
{{- fail (printf "%s.bucket.worldID %q must be a canonical lowercase UUID with RFC 4122 variant" $location $bucket.worldID) -}}
{{- end -}}
{{- if hasKey $worldIDs $bucket.worldID -}}
{{- fail (printf "%s.bucket.worldID %q is duplicated" $location $bucket.worldID) -}}
{{- end -}}
{{- $_ := set $worldIDs $bucket.worldID true -}}
{{- if empty $tokenSecret.name -}}
{{- fail (printf "%s.tokenSecret.name is required" $location) -}}
{{- end -}}
{{- if hasKey $tokenSecrets $tokenSecret.name -}}
{{- fail (printf "%s.tokenSecret.name %q is duplicated" $location $tokenSecret.name) -}}
{{- end -}}
{{- $_ := set $tokenSecrets $tokenSecret.name true -}}
{{- if empty $tokenSecret.key -}}
{{- fail (printf "%s.tokenSecret.key is required" $location) -}}
{{- end -}}
{{- end -}}
{{- end -}}
