{{/* The five backend templates the shared library expects. No content
     volume: documents, versions and catalog rows all live in Postgres. */}}

{{- define "demarkus-server.backendArgs" -}}
- -store
- postgres
{{- end -}}

{{- define "demarkus-server.backendEnv" -}}
# DSN via Secret so credentials never appear in the pod spec.
- name: DEMARKUS_PG_DSN
  valueFrom:
    secretKeyRef:
      name: {{ .Values.server.postgres.dsnSecret.name | quote }}
      key: {{ .Values.server.postgres.dsnSecret.key | default "uri" | quote }}
{{- end -}}

{{- define "demarkus-server.backendVolumeMounts" -}}
{{- end -}}

{{- define "demarkus-server.backendVolumeClaims" -}}
{{- end -}}

{{/* The DSN has no sensible default, so refuse to render without it. */}}
{{- define "demarkus-server.backendGuard" -}}
{{- if not .Values.server.postgres.dsnSecret.name -}}
{{- fail "server.postgres.dsnSecret.name is required: it names the Secret holding the Postgres DSN" -}}
{{- end -}}
{{- end -}}
