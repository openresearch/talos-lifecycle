{{- define "otc.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "otc.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{- define "otc.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{ include "otc.selectorLabels" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/* no release name in selectors: they match install.yaml, so Helm can adopt it */}}
{{- define "otc.selectorLabels" -}}
app.kubernetes.io/name: {{ include "otc.name" . }}
{{- end }}

{{- define "otc.image" -}}
{{ .Values.image.repository }}:{{ default .Chart.AppVersion .Values.image.tag }}
{{- end }}

{{- define "otc.affinity" -}}
{{- if .Values.affinity }}
{{- toYaml .Values.affinity }}
{{- else }}
podAntiAffinity:
  requiredDuringSchedulingIgnoredDuringExecution:
  - labelSelector:
      matchLabels:
        {{- include "otc.selectorLabels" . | nindent 8 }}
    topologyKey: kubernetes.io/hostname
{{- end }}
{{- end }}

{{/* arg: dict "root" $ "sa" name "secret" talos-secret-name */}}
{{- define "otc.podCommon" -}}
{{- $ := .root -}}
priorityClassName: {{ $.Values.priorityClassName }}
securityContext:
  runAsNonRoot: true
  seccompProfile:
    type: RuntimeDefault
serviceAccountName: {{ .sa }}
terminationGracePeriodSeconds: 10
{{- with $.Values.imagePullSecrets }}
imagePullSecrets:
  {{- toYaml . | nindent 2 }}
{{- end }}
{{- with $.Values.nodeSelector }}
nodeSelector:
  {{- toYaml . | nindent 2 }}
{{- end }}
{{- with $.Values.tolerations }}
tolerations:
  {{- toYaml . | nindent 2 }}
{{- end }}
volumes:
- name: talos-secrets
  secret:
    secretName: {{ .secret }}
- emptyDir:
    sizeLimit: 64Mi
  name: tmp
{{- end }}

{{- define "otc.containerCommon" -}}
command:
- /manager
image: {{ include "otc.image" . }}
imagePullPolicy: {{ .Values.image.pullPolicy }}
livenessProbe:
  httpGet:
    path: /healthz
    port: 8081
  initialDelaySeconds: 15
  periodSeconds: 20
name: manager
readinessProbe:
  httpGet:
    path: /readyz
    port: 8081
  initialDelaySeconds: 5
  periodSeconds: 10
securityContext:
  allowPrivilegeEscalation: false
  capabilities:
    drop:
    - ALL
  readOnlyRootFilesystem: true
volumeMounts:
- mountPath: /var/run/secrets/talos.dev
  name: talos-secrets
  readOnly: true
- mountPath: /tmp
  name: tmp
{{- end }}

{{/* arg: dict "root" $ "name" service-name "selector" labels-yaml */}}
{{- define "otc.metricsService" -}}
apiVersion: v1
kind: Service
metadata:
  name: {{ .name | trunc 63 | trimSuffix "-" }}
  namespace: {{ .root.Release.Namespace }}
  labels:
    {{- include "otc.labels" .root | nindent 4 }}
spec:
  ports:
  - name: https
    port: 8443
    protocol: TCP
    targetPort: 8443
  selector:
    {{- .selector | nindent 4 }}
{{- end }}
