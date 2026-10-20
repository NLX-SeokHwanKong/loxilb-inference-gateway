{{- define "loxilb-igw.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "loxilb-igw.fullname" -}}
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

{{- define "loxilb-igw.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "loxilb-igw.labels" -}}
helm.sh/chart: {{ include "loxilb-igw.chart" . }}
{{ include "loxilb-igw.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "loxilb-igw.selectorLabels" -}}
app.kubernetes.io/name: {{ include "loxilb-igw.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "loxilb-igw.image" -}}
{{- printf "%s:%s" .Values.image.repository (.Values.image.tag | default .Chart.AppVersion | toString) -}}
{{- end -}}

{{/*
Container args: every loxilb flag the chart knows, in one place.
*/}}
{{- define "loxilb-igw.args" -}}
{{- $l := .Values.loxilb -}}
{{- if $l.egrHooks }}
- --egr-hooks
{{- end }}
{{- with $l.blacklist }}
- {{ printf "--blacklist=%s" . | quote }}
{{- end }}
{{- with $l.configPath }}
- {{ printf "--config-path=%s" . | quote }}
{{- end }}
{{- with $l.audit.dir }}
- {{ printf "--audit-dir=%s" . | quote }}
{{- end }}
{{- if $l.audit.required }}
- --audit-required
{{- end }}
{{- with $l.mgmtProfile }}
- {{ printf "--mgmt-profile=%s" . | quote }}
{{- end }}
{{- if .Values.tls.enabled }}
- --tls
- {{ printf "--tls-port=%v" .Values.tls.port | quote }}
{{- end }}
{{- range $l.extraArgs }}
- {{ . | quote }}
{{- end }}
{{- end -}}
