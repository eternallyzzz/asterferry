{{/* Expand the chart name. */}}
{{- define "asterferry.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/* Create a release-qualified name. */}}
{{- define "asterferry.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := include "asterferry.name" . }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/* Common labels. */}}
{{- define "asterferry.labels" -}}
helm.sh/chart: {{ include "asterferry.name" . }}-{{ .Chart.Version | replace "+" "_" }}
app.kubernetes.io/name: {{ include "asterferry.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/component: {{ .Values.role }}
{{- end }}

{{/* Selector labels. */}}
{{- define "asterferry.selectorLabels" -}}
app.kubernetes.io/name: {{ include "asterferry.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/* Service account name. */}}
{{- define "asterferry.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "asterferry.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/* ConfigMap name. */}}
{{- define "asterferry.configMapName" -}}
{{- if .Values.config.content }}
{{- printf "%s-config" (include "asterferry.fullname" .) }}
{{- else }}
{{- required "config.existingConfigMap is required when config.content is empty" .Values.config.existingConfigMap }}
{{- end }}
{{- end }}

{{/* Secret name. */}}
{{- define "asterferry.secretName" -}}
{{- required "secret.existingSecret must reference a pre-created Secret" .Values.secret.existingSecret }}
{{- end }}

{{/* Management port. */}}
{{- define "asterferry.managementPort" -}}
{{- if gt (int .Values.managementPort) 0 }}
{{- .Values.managementPort }}
{{- else if eq .Values.role "gateway" }}
9090
{{- else }}
9091
{{- end }}
{{- end }}

{{/* Image reference. */}}
{{- define "asterferry.image" -}}
{{- if .Values.image.digest }}
{{- if not (regexMatch "^sha256:[a-f0-9]{64}$" .Values.image.digest) }}
{{- fail "image.digest must match sha256:<64 lowercase hexadecimal characters>" }}
{{- end }}
{{- printf "%s@%s" .Values.image.repository .Values.image.digest }}
{{- else }}
{{- printf "%s:%s" .Values.image.repository (default .Chart.AppVersion .Values.image.tag) }}
{{- end }}
{{- end }}
