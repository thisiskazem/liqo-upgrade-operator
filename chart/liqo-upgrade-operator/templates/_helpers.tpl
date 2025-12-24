{{/*
Expand the name of the chart.
*/}}
{{- define "liqo-upgrade-operator.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "liqo-upgrade-operator.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- printf "%s" $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "liqo-upgrade-operator.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "liqo-upgrade-operator.labels" -}}
helm.sh/chart: {{ include "liqo-upgrade-operator.chart" . }}
{{ include "liqo-upgrade-operator.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "liqo-upgrade-operator.selectorLabels" -}}
app.kubernetes.io/name: {{ include "liqo-upgrade-operator.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
control-plane: controller-manager
{{- end }}

{{/*
Create the name of the service account to use
*/}}
{{- define "liqo-upgrade-operator.serviceAccountName" -}}
{{- printf "%s-manager" (include "liqo-upgrade-operator.fullname" .) }}
{{- end }}

{{/*
Operator namespace
*/}}
{{- define "liqo-upgrade-operator.namespace" -}}
{{- .Values.namespace }}
{{- end }}

