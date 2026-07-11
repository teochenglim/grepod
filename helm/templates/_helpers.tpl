{{/*
Chart name, truncated/sanitized for use in resource names.
*/}}
{{- define "grepod.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Fully qualified resource name base. Defaults to the release name so a
namespace normally has exactly one grepod release; override via
fullnameOverride if you need something else.
*/}}
{{- define "grepod.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{/*
"helm.sh/chart" label value.
*/}}
{{- define "grepod.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Common labels.
*/}}
{{- define "grepod.labels" -}}
helm.sh/chart: {{ include "grepod.chart" . }}
{{ include "grepod.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{/*
Selector labels — kept minimal and immutable across upgrades.
*/}}
{{- define "grepod.selectorLabels" -}}
app.kubernetes.io/name: {{ include "grepod.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/*
ServiceAccount name.
*/}}
{{- define "grepod.serviceAccountName" -}}
{{- include "grepod.fullname" . -}}
{{- end -}}
