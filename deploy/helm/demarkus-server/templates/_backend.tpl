{{/*
File-store backend. These five templates are the contract the shared
library expects from a backend chart; demarkus-server-pg defines its own.
*/}}

{{- define "demarkus-server.backendArgs" -}}
- -root
- /var/lib/demarkus/content
{{- end -}}

{{- define "demarkus-server.backendEnv" -}}
{{- end -}}

{{- define "demarkus-server.backendVolumeMounts" -}}
- name: content
  mountPath: /var/lib/demarkus/content
{{- end -}}

{{- define "demarkus-server.backendVolumeClaims" -}}
- metadata:
    name: content
    # selectorLabels only: the VCT is immutable, and chart/version labels
    # would churn every bump into a forbidden in-place update.
    labels:
      {{- include "demarkus-server.selectorLabels" . | nindent 6 }}
  spec:
    accessModes:
      - {{ .Values.storage.accessMode }}
    # Explicit, and defaulted so an empty override cannot blank it: k8s
    # would re-default server-side and the drift wedges SSA re-applies.
    volumeMode: {{ .Values.storage.volumeMode | default "Filesystem" }}
    resources:
      requests:
        storage: {{ .Values.storage.size }}
    {{- if .Values.storage.className }}
    storageClassName: {{ .Values.storage.className | quote }}
    {{- end }}
{{- end -}}

{{/* Nothing to validate: storage.* all have defaults. */}}
{{- define "demarkus-server.backendGuard" -}}
{{- end -}}
