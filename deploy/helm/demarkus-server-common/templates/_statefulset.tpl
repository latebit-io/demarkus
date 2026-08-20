{{/* One definition for all three probes: a CLI fetch of the manifest. */}}
{{- define "demarkus-server.probeCommand" -}}
- /demarkus
- -insecure
- -no-cache
- {{ printf "mark://localhost:%d/.well-known/agent-manifest.md" (int .Values.server.udpPort) | quote }}
{{- end -}}

{{- define "demarkus-server.statefulset" -}}
{{- $tlsSecret := include "demarkus-server.tlsSecretName" . -}}
{{- include "demarkus-server.backendGuard" . -}}
{{- /* A backend swap under a live release would need an immutable
       volumeClaimTemplates change and would move no data; refuse it here so
       every backend inherits the check. */ -}}
{{- $wantClaims := ne (trim (include "demarkus-server.backendVolumeClaims" .)) "" -}}
{{- $existing := lookup "apps/v1" "StatefulSet" .Release.Namespace (include "demarkus-server.fullname" .) -}}
{{- if $existing -}}
{{- $hasClaims := gt (len ($existing.spec.volumeClaimTemplates | default list)) 0 -}}
{{- if ne $hasClaims $wantClaims -}}
{{- fail "this release runs a different storage backend: migrate the content with demarkus-migrate, then uninstall, delete any retained content PVC, and install this chart" -}}
{{- end -}}
{{- end -}}
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: {{ include "demarkus-server.fullname" . }}
  labels:
    {{- include "demarkus-server.labels" . | nindent 4 }}
spec:
  serviceName: {{ include "demarkus-server.fullname" . }}
  replicas: {{ .Values.replicaCount }}
  podManagementPolicy: OrderedReady
  selector:
    matchLabels:
      {{- include "demarkus-server.selectorLabels" . | nindent 6 }}
  template:
    metadata:
      labels:
        {{- include "demarkus-server.selectorLabels" . | nindent 8 }}
        {{- with .Values.podLabels }}
        {{- toYaml . | nindent 8 }}
        {{- end }}
      annotations:
        {{- /* No checksum/tokens: the token Secret is runtime-owned (bootstrap
               Job + broker), not chart-rendered, so there is nothing stable to
               hash here. Token rotation is an explicit operator action. */}}
        {{- with .Values.podAnnotations }}
        {{- toYaml . | nindent 8 }}
        {{- end }}
    spec:
      serviceAccountName: {{ include "demarkus-server.serviceAccountName" . }}
      {{- with .Values.imagePullSecrets }}
      imagePullSecrets:
        {{- toYaml . | nindent 8 }}
      {{- end }}
      securityContext:
        {{- toYaml .Values.podSecurityContext | nindent 8 }}
      containers:
        - name: server
          image: "{{ .Values.image.repository }}:{{ .Values.image.tag | default .Chart.AppVersion }}"
          imagePullPolicy: {{ .Values.image.pullPolicy }}
          securityContext:
            {{- toYaml .Values.securityContext | nindent 12 }}
          args:
            - -port
            - {{ .Values.server.udpPort | quote }}
            {{- with include "demarkus-server.backendArgs" . }}
            {{- . | nindent 12 }}
            {{- end }}
            - -tokens
            - /etc/demarkus/tokens/tokens.toml
            {{- if $tlsSecret }}
            - -tls-cert
            - /etc/demarkus/tls/tls.crt
            - -tls-key
            - /etc/demarkus/tls/tls.key
            {{- end }}
            {{- if .Values.server.readOnly }}
            - -read-only
            {{- end }}
            {{- with .Values.server.extraArgs }}
            {{- toYaml . | nindent 12 }}
            {{- end }}
          env:
            - name: HOME
              value: /home/demarkus
            # JSON by default so log shippers parse fields without grok;
            # override to "text" on a debug pod.
            - name: DEMARKUS_LOG_FORMAT
              value: {{ .Values.server.logFormat | default "json" | quote }}
            {{- with include "demarkus-server.backendEnv" . }}
            {{- . | nindent 12 }}
            {{- end }}
          ports:
            - name: mark
              protocol: UDP
              containerPort: {{ .Values.server.udpPort }}
          {{- if .Values.probes.enabled }}
          # Holds liveness off during startup work (world walk / catalog
          # backfill); see values.yaml.
          startupProbe:
            exec:
              command:
                {{- include "demarkus-server.probeCommand" . | nindent 16 }}
            periodSeconds: {{ .Values.probes.startup.periodSeconds }}
            timeoutSeconds: {{ .Values.probes.startup.timeoutSeconds }}
            failureThreshold: {{ .Values.probes.startup.failureThreshold }}
          livenessProbe:
            exec:
              command:
                {{- include "demarkus-server.probeCommand" . | nindent 16 }}
            initialDelaySeconds: {{ .Values.probes.liveness.initialDelaySeconds }}
            periodSeconds: {{ .Values.probes.liveness.periodSeconds }}
            timeoutSeconds: {{ .Values.probes.liveness.timeoutSeconds }}
            failureThreshold: {{ .Values.probes.liveness.failureThreshold }}
          readinessProbe:
            exec:
              command:
                {{- include "demarkus-server.probeCommand" . | nindent 16 }}
            initialDelaySeconds: {{ .Values.probes.readiness.initialDelaySeconds }}
            periodSeconds: {{ .Values.probes.readiness.periodSeconds }}
            timeoutSeconds: {{ .Values.probes.readiness.timeoutSeconds }}
            failureThreshold: {{ .Values.probes.readiness.failureThreshold }}
          {{- end }}
          volumeMounts:
            {{- with include "demarkus-server.backendVolumeMounts" . }}
            {{- . | nindent 12 }}
            {{- end }}
            - name: tokens
              mountPath: /etc/demarkus/tokens
              readOnly: true
            {{- if $tlsSecret }}
            - name: tls
              mountPath: /etc/demarkus/tls
              readOnly: true
            {{- end }}
            - name: tmp
              mountPath: /tmp
            - name: home
              mountPath: /home/demarkus
          resources:
            {{- toYaml .Values.resources | nindent 12 }}
      volumes:
        - name: tokens
          secret:
            secretName: {{ include "demarkus-server.tokensSecretName" . }}
        {{- if $tlsSecret }}
        - name: tls
          secret:
            secretName: {{ $tlsSecret }}
        {{- end }}
        - name: tmp
          emptyDir: {}
        - name: home
          emptyDir: {}
      {{- with .Values.nodeSelector }}
      nodeSelector:
        {{- toYaml . | nindent 8 }}
      {{- end }}
      {{- with .Values.affinity }}
      affinity:
        {{- toYaml . | nindent 8 }}
      {{- end }}
      {{- with .Values.tolerations }}
      tolerations:
        {{- toYaml . | nindent 8 }}
      {{- end }}
  {{- with include "demarkus-server.backendVolumeClaims" . }}
  volumeClaimTemplates:
    {{- . | nindent 4 }}
  {{- end }}
{{- end -}}
