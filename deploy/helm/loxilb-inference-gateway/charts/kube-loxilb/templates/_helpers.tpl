{{- define "kube-loxilb.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "kube-loxilb.fullname" -}}
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

{{- define "kube-loxilb.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "kube-loxilb.labels" -}}
helm.sh/chart: {{ include "kube-loxilb.chart" . }}
{{ include "kube-loxilb.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "kube-loxilb.selectorLabels" -}}
app.kubernetes.io/name: {{ include "kube-loxilb.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "kube-loxilb.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "kube-loxilb.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{- define "kube-loxilb.image" -}}
{{- printf "%s:%s" .Values.image.repository (.Values.image.tag | default .Chart.AppVersion | toString) -}}
{{- end -}}

{{/*
name=cidr[,name=cidr] from a map, keys sorted.
*/}}
{{- define "kube-loxilb.poolDefs" -}}
{{- $defs := list -}}
{{- range $k, $v := . }}{{ $defs = append $defs (printf "%s=%s" $k $v) }}{{ end -}}
{{- join "," $defs -}}
{{- end -}}

{{/*
Container args.
*/}}
{{- define "kube-loxilb.args" -}}
{{- with .Values.loxiURL }}
- {{ printf "--loxiURL=%s" (join "," .) | quote }}
{{- end }}
{{- with .Values.cidrPools }}
- {{ printf "--cidrPools=%s" (include "kube-loxilb.poolDefs" .) | quote }}
{{- end }}
{{- with .Values.cidr6Pools }}
- {{ printf "--cidr6Pools=%s" (include "kube-loxilb.poolDefs" .) | quote }}
{{- end }}
{{- with .Values.setRoles }}
- {{ printf "--setRoles=%s" (. | toString) | quote }}
{{- end }}
{{- if .Values.gatewayAPI }}
- --gatewayAPI
{{- end }}
{{- if .Values.inferenceExtension }}
- --inferenceExtension
{{- end }}
{{- if not (kindIs "invalid" .Values.setLBMode) }}
- {{ printf "--setLBMode=%v" .Values.setLBMode | quote }}
{{- end }}
{{- if not (kindIs "invalid" .Values.verbosity) }}
- {{ printf "--v=%v" .Values.verbosity | quote }}
{{- end }}
{{- range .Values.extraArgs }}
- {{ . | quote }}
{{- end }}
{{- end -}}

{{/*
The URL the wait-for-loxilb initContainer polls.
*/}}
{{- define "kube-loxilb.waitURL" -}}
{{- $w := .Values.waitForLoxilb -}}
{{- if $w.url -}}
{{- $w.url -}}
{{- else if .Values.loxiURL -}}
{{- printf "%s/netlox/v1/version" (first .Values.loxiURL | trimSuffix "/") -}}
{{- else -}}
{{- printf "http://%s.%s.svc:%v/netlox/v1/version" $w.serviceName .Release.Namespace $w.port -}}
{{- end -}}
{{- end -}}
