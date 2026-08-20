{{- define "demarkus-server.tokenBootstrap" -}}
{{- if .Values.tokens.bootstrap.enabled }}
{{- /* The token Secrets are runtime-owned, not desired-state: the broker
       appends to tokens.toml and the admin hash must match the raw value, so
       a reconciler must never template them. See the chart README. */}}
{{- $sa := printf "%s-token-bootstrap" (include "demarkus-server.fullname" .) -}}
apiVersion: v1
kind: ServiceAccount
metadata:
  name: {{ $sa }}
  namespace: {{ .Release.Namespace }}
  labels:
    {{- include "demarkus-server.labels" . | nindent 4 }}
  annotations:
    helm.sh/hook: pre-install,pre-upgrade
    helm.sh/hook-weight: "-10"
    helm.sh/hook-delete-policy: before-hook-creation,hook-succeeded
    argocd.argoproj.io/hook: PreSync
    argocd.argoproj.io/sync-wave: "-10"
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: {{ $sa }}
  namespace: {{ .Release.Namespace }}
  labels:
    {{- include "demarkus-server.labels" . | nindent 4 }}
  annotations:
    helm.sh/hook: pre-install,pre-upgrade
    helm.sh/hook-weight: "-10"
    helm.sh/hook-delete-policy: before-hook-creation,hook-succeeded
    argocd.argoproj.io/hook: PreSync
    argocd.argoproj.io/sync-wave: "-10"
rules:
  # Bootstrap-only: read to detect an existing (runtime-owned) Secret and skip;
  # create + annotate to write the initial pair. No update/delete — the Job
  # never overwrites what the broker owns.
  - apiGroups: [""]
    resources: ["secrets"]
    verbs: ["get", "create", "patch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: {{ $sa }}
  namespace: {{ .Release.Namespace }}
  labels:
    {{- include "demarkus-server.labels" . | nindent 4 }}
  annotations:
    helm.sh/hook: pre-install,pre-upgrade
    helm.sh/hook-weight: "-10"
    helm.sh/hook-delete-policy: before-hook-creation,hook-succeeded
    argocd.argoproj.io/hook: PreSync
    argocd.argoproj.io/sync-wave: "-10"
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: {{ $sa }}
subjects:
  - kind: ServiceAccount
    name: {{ $sa }}
    namespace: {{ .Release.Namespace }}
---
apiVersion: batch/v1
kind: Job
metadata:
  name: {{ $sa }}
  namespace: {{ .Release.Namespace }}
  labels:
    {{- include "demarkus-server.labels" . | nindent 4 }}
  annotations:
    helm.sh/hook: pre-install,pre-upgrade
    helm.sh/hook-weight: "-5"
    helm.sh/hook-delete-policy: before-hook-creation,hook-succeeded
    argocd.argoproj.io/hook: PreSync
    argocd.argoproj.io/sync-wave: "-5"
spec:
  backoffLimit: 2
  template:
    metadata:
      labels:
        {{- include "demarkus-server.selectorLabels" . | nindent 8 }}
    spec:
      serviceAccountName: {{ $sa }}
      restartPolicy: Never
      securityContext:
        runAsNonRoot: true
        runAsUser: 65532
        # fsGroup so the non-root user can write the scratch emptyDir below
        # (kubectl wants a writable cache dir under a read-only root fs).
        fsGroup: 65532
      containers:
        - name: bootstrap
          image: {{ .Values.tokens.bootstrap.image | quote }}
          imagePullPolicy: {{ .Values.tokens.bootstrap.imagePullPolicy | default "IfNotPresent" }}
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities:
              drop: ["ALL"]
          volumeMounts:
            - name: scratch
              mountPath: /tmp
          env:
            # HOME on the writable scratch volume → kubectl's cache/discovery
            # writes succeed under readOnlyRootFilesystem.
            - name: HOME
              value: /tmp
            - name: NS
              value: {{ .Release.Namespace | quote }}
            - name: TOKENS_SECRET
              value: {{ include "demarkus-server.tokensSecretName" . | quote }}
            - name: VALUES_SECRET
              value: {{ include "demarkus-server.tokenValuesSecretName" . | quote }}
            - name: LABEL
              value: {{ .Values.tokens.admin.label | quote }}
            # Explicit token (optional); empty ⇒ generate a fresh 64-char token.
            - name: EXPLICIT_TOKEN
              value: {{ .Values.tokens.admin.token | quote }}
            - name: EMIT_RAW
              value: {{ .Values.tokens.emitRawValues | quote }}
          command: ["/bin/sh", "-eu", "-c"]
          args:
            - |
              if kubectl -n "$NS" get secret "$TOKENS_SECRET" >/dev/null 2>&1; then
                echo "tokens secret $TOKENS_SECRET exists; handing off to runtime owners (broker). no-op."
                exit 0
              fi
              TOKEN="$EXPLICIT_TOKEN"
              if [ -z "$TOKEN" ]; then
                TOKEN=$(tr -dc 'A-Za-z0-9' </dev/urandom | head -c 64)
              fi
              HASH="sha256-$(printf %s "$TOKEN" | sha256sum | cut -d' ' -f1)"
              TOML=$(printf '[tokens.%s]\nhash = "%s"\npaths = [{{ include "demarkus-server.tokenAdminPaths" . }}]\noperations = [{{ include "demarkus-server.tokenAdminOps" . }}]\n' "$LABEL" "$HASH")
              kubectl -n "$NS" create secret generic "$TOKENS_SECRET" --from-literal=tokens.toml="$TOML"
              kubectl -n "$NS" patch secret "$TOKENS_SECRET" -p '{"metadata":{"annotations":{"helm.sh/resource-policy":"keep"}}}'
              if [ "$EMIT_RAW" = "true" ]; then
                kubectl -n "$NS" create secret generic "$VALUES_SECRET" --from-literal="$LABEL=$TOKEN"
                kubectl -n "$NS" patch secret "$VALUES_SECRET" -p '{"metadata":{"annotations":{"helm.sh/resource-policy":"keep","demarkus.io/raw-tokens":"true"}}}'
              fi
              echo "bootstrapped $TOKENS_SECRET (emit-raw=$EMIT_RAW), hash $HASH."
      volumes:
        - name: scratch
          emptyDir: {}
{{- end }}
{{- end -}}
