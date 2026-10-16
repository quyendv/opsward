{{- define "opsward.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "opsward.fullname" -}}
{{- if contains (include "opsward.name" .) .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name (include "opsward.name" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "opsward.selectorLabels" -}}
app.kubernetes.io/name: {{ include "opsward.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "opsward.labels" -}}
{{ include "opsward.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{- end -}}

{{/* The configuration with the chart's defaults under it, after checking what the chart
needs of it. */}}
{{- define "opsward.config" -}}
{{- $cfg := deepCopy .Values.config -}}
{{- $defaults := dict "server" (dict "http" (dict "address" "0.0.0.0:8080")) -}}
{{- if eq (dig "storage" "type" "sqlite" .Values.config) "sqlite" -}}
{{- $_ := set $defaults "storage" (dict "path" "/var/lib/opsward/opsward.db") -}}
{{- end -}}
{{- $cfg = mergeOverwrite $defaults $cfg -}}
{{- if ne (dig "identity" "mode" "" $cfg) "remote" -}}
{{- fail "config.identity.mode: the chart runs Opsward in remote mode" -}}
{{- end -}}
{{- if not (hasSuffix ":8080" (dig "server" "http" "address" "" $cfg)) -}}
{{- fail "config.server.http.address: the chart serves port 8080; use an address ending in :8080" -}}
{{- end -}}
{{- if and (gt (int .Values.replicaCount) 1) (ne (dig "storage" "type" "sqlite" $cfg) "postgres") -}}
{{- fail "replicaCount: more than one replica needs storage.type postgres" -}}
{{- end -}}
{{- range .Values.localProfiles -}}
{{- if not (regexMatch "^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$" .) -}}
{{- fail (printf "localProfiles: %q is not a DNS-1123 label" .) -}}
{{- end -}}
{{- end -}}
{{- toYaml $cfg -}}
{{- end -}}
