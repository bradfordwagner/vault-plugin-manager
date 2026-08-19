{{- define "vault-plugin-manager.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "vault-plugin-manager.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name (include "vault-plugin-manager.name" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "vault-plugin-manager.labels" -}}
app.kubernetes.io/name: {{ include "vault-plugin-manager.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{- end -}}

{{- define "vault-plugin-manager.selectorLabels" -}}
app.kubernetes.io/name: {{ include "vault-plugin-manager.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "vault-plugin-manager.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "vault-plugin-manager.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{- define "vault-plugin-manager.configMapNamespace" -}}
{{- default .Release.Namespace .Values.configMap.namespace -}}
{{- end -}}
