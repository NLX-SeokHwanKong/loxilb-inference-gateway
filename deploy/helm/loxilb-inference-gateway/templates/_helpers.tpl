{{/*
Expand the name of the chart.
*/}}
{{- define "loxilb-inference-gateway.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Fully qualified app name (release-aware, 63 chars).
*/}}
{{- define "loxilb-inference-gateway.fullname" -}}
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

{{- define "loxilb-inference-gateway.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "loxilb-inference-gateway.labels" -}}
helm.sh/chart: {{ include "loxilb-inference-gateway.chart" . }}
{{ include "loxilb-inference-gateway.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "loxilb-inference-gateway.selectorLabels" -}}
app.kubernetes.io/name: {{ include "loxilb-inference-gateway.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/*
Namespace of the Gateway: inference.gateway.namespace, else the release namespace.
*/}}
{{- define "loxilb-inference-gateway.gatewayNamespace" -}}
{{- .Values.inference.gateway.namespace | default .Release.Namespace -}}
{{- end -}}

{{/*
Namespace of a pool. Context: (dict "pool" <pool> "root" $)
*/}}
{{- define "loxilb-inference-gateway.poolNamespace" -}}
{{- .pool.namespace | default .root.Release.Namespace -}}
{{- end -}}

{{/*
The loxilb REST base URL the hooks and tests call: tests.loxilbURL, else the
loxilb-igw management Service, else the first kube-loxilb.loxiURL, else the
discovery Service.
*/}}
{{- define "loxilb-inference-gateway.loxilbURL" -}}
{{- $igw := index .Values "loxilb-igw" | default dict -}}
{{- $klb := index .Values "kube-loxilb" | default dict -}}
{{- $mgmt := $igw.mgmtService | default dict -}}
{{- $disc := $igw.discoveryService | default dict -}}
{{- if .Values.tests.loxilbURL -}}
{{- .Values.tests.loxilbURL | trimSuffix "/" -}}
{{- else if and $igw.enabled $mgmt.enabled -}}
{{- printf "http://%s.%s.svc:%v" ($mgmt.name | default "loxilb-lb-mgmt") .Release.Namespace ($mgmt.port | default 11111) -}}
{{- else if $klb.loxiURL -}}
{{- first $klb.loxiURL | trimSuffix "/" -}}
{{- else -}}
{{- printf "http://%s.%s.svc:%v" ($disc.name | default "loxilb-lb-service") .Release.Namespace (($igw.ports | default dict).api | default 11111) -}}
{{- end -}}
{{- end -}}

{{/*
Annotations of one InferencePool, as YAML. Context: (dict "pool" <pool>)
  modelName  -> loxilb.io/model-name
  epselect   -> loxilb.io/epselect
  sseMode    -> loxilb.io/sse-mode (only when true; absent means false to kube-loxilb)
  auth.mode  -> loxilb.io/api-key-auth (unless none)
  auth.jwtAuthProfile -> loxilb.io/jwt-auth-profile
  annotations -> verbatim, last, so they can override the above
*/}}
{{- define "loxilb-inference-gateway.poolAnnotations" -}}
{{- $p := .pool -}}
{{- $ann := dict -}}
{{- with $p.modelName }}{{ $_ := set $ann "loxilb.io/model-name" (. | toString) }}{{ end -}}
{{- with $p.epselect }}{{ $_ := set $ann "loxilb.io/epselect" (. | toString) }}{{ end -}}
{{- if $p.sseMode }}{{ $_ := set $ann "loxilb.io/sse-mode" "true" }}{{ end -}}
{{- with $p.auth -}}
{{- if and .mode (ne (.mode | toString) "none") }}{{ $_ := set $ann "loxilb.io/api-key-auth" (.mode | toString) }}{{ end -}}
{{- with .jwtAuthProfile }}{{ $_ := set $ann "loxilb.io/jwt-auth-profile" (. | toString) }}{{ end -}}
{{- end -}}
{{- range $k, $v := ($p.annotations | default dict) }}{{ $_ := set $ann $k ($v | toString) }}{{ end -}}
{{- toYaml $ann -}}
{{- end -}}

{{/*
Pools whose auth is set: kube-loxilb does not honour the auth annotations yet.
*/}}
{{- define "loxilb-inference-gateway.authPools" -}}
{{- $root := . -}}
{{- $names := list -}}
{{- range $p := .Values.inference.pools -}}
{{- with $p.auth -}}
{{- if or (and .mode (ne (.mode | toString) "none")) .jwtAuthProfile -}}
{{- $names = append $names (printf "%s/%s" (include "loxilb-inference-gateway.poolNamespace" (dict "pool" $p "root" $root)) $p.name) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- join ", " $names -}}
{{- end -}}

{{/*
What test-pool-rule asserts, as JSON: for every pool, the Service kube-loxilb
creates per Gateway listener ("<pool>-inference", with "-<gateway>-<listener>"
appended when the pool is served on more than one listener) and the pool's
own fields.
*/}}
{{- define "loxilb-inference-gateway.poolTestSpec" -}}
{{- $root := . -}}
{{- $gw := .Values.inference.gateway -}}
{{- $multi := gt (len $gw.listeners) 1 -}}
{{- $out := list -}}
{{- range $p := .Values.inference.pools -}}
{{- $svcs := list -}}
{{- range $l := $gw.listeners -}}
{{- $name := printf "%s-inference" $p.name -}}
{{- if $multi }}{{ $name = printf "%s-%s-%s" $name $gw.name $l.name }}{{ end -}}
{{- $svcs = append $svcs (dict "name" $name "port" $l.port) -}}
{{- end -}}
{{- $route := $p.route | default dict -}}
{{- $spec := dict "name" $p.name "namespace" (include "loxilb-inference-gateway.poolNamespace" (dict "pool" $p "root" $root)) "selector" ($p.selector | default dict) "targetPort" $p.targetPort "modelName" ($p.modelName | default "" | toString) "epselect" ($p.epselect | default "" | toString) "hostnames" ($route.hostnames | default list) "matches" ($route.matches | default list) "services" $svcs -}}
{{- $out = append $out $spec -}}
{{- end -}}
{{- toJson $out -}}
{{- end -}}
