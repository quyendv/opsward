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

{{/* values is the configuration as given, without sections left empty (null). */}}
{{- define "opsward.values" -}}
{{- $cfg := dict -}}
{{- range $k, $v := .Values.config -}}
{{- if not (kindIs "invalid" $v) -}}{{- $_ := set $cfg $k $v -}}{{- end -}}
{{- end -}}
{{- toYaml $cfg -}}
{{- end -}}

{{/* The configuration with the chart's defaults under it, after checking what the chart
needs of it. */}}
{{- define "opsward.config" -}}
{{- $cfg := fromYaml (include "opsward.values" .) -}}
{{- $defaults := dict "server" (dict "http" (dict "address" "0.0.0.0:8080")) -}}
{{- if eq (dig "storage" "type" "sqlite" $cfg) "sqlite" -}}
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
{{- $seen := dict -}}
{{- range .Values.localProfiles -}}
{{- if not (regexMatch "^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$" .) -}}
{{- fail (printf "localProfiles: %q is not a DNS-1123 label" .) -}}
{{- end -}}
{{- /* Volumes are named kubeconfig-<profile>, at most 63 characters. */ -}}
{{- if gt (len .) 52 -}}
{{- fail (printf "localProfiles: %q is longer than 52 characters" .) -}}
{{- end -}}
{{- if hasKey $seen . -}}
{{- fail (printf "localProfiles: %q appears twice" .) -}}
{{- end -}}
{{- $_ := set $seen . true -}}
{{- end -}}
{{- /* The root filesystem is read-only: files Opsward writes go on the data volume. */ -}}
{{- if eq (dig "storage" "type" "sqlite" $cfg) "sqlite" -}}
{{- include "opsward.writable" (list "config.storage.path" (dig "storage" "path" "" $cfg)) -}}
{{- end -}}
{{- if eq (dig "audit" "stream" "type" "" $cfg) "file" -}}
{{- include "opsward.writable" (list "config.audit.stream.path" (dig "audit" "stream" "path" "" $cfg)) -}}
{{- end -}}
{{- if and .Values.ingress.enabled (not .Values.ingress.tls) (not .Values.ingress.allowPlainHTTP) -}}
{{- fail "ingress: without tls, bearer tokens cross the network in clear; set ingress.tls, or ingress.allowPlainHTTP if TLS ends in front of the Ingress" -}}
{{- end -}}
{{- toYaml $cfg -}}
{{- end -}}

{{- define "opsward.writable" -}}
{{- $field := index . 0 -}}{{- $path := index . 1 -}}
{{- if not (hasPrefix "/var/lib/opsward/" $path) -}}
{{- fail (printf "%s: %q is not under /var/lib/opsward, the only writable path" $field $path) -}}
{{- end -}}
{{- end -}}

{{/* sqlite reports whether the configuration stores in SQLite, on the chart's volume. */}}
{{- define "opsward.sqlite" -}}
{{- if eq (dig "storage" "type" "sqlite" (fromYaml (include "opsward.values" .))) "sqlite" -}}true{{- end -}}
{{- end -}}

{{/* restartHash hashes the sections a running Opsward cannot reload
(config.RestartRequired), so that changing one rolls the pod; the rest reloads in place. */}}
{{- define "opsward.restartHash" -}}
{{- $cfg := fromYaml (include "opsward.config" .) -}}
{{- $providers := deepCopy (default dict $cfg.providers) -}}
{{- $clusters := dict -}}
{{- range $name, $c := (dig "kubernetes" "clusters" dict $providers) -}}
{{- $_ := set $clusters $name (pick $c "metrics" "logs") -}}
{{- end -}}
{{- $_ := unset $providers "kubernetes" -}}
{{- $identity := omit (default dict $cfg.identity) "groups" -}}
{{- dict "identity" $identity "server" $cfg.server "storage" $cfg.storage "audit" $cfg.audit "approvals" $cfg.approvals "providers" $providers "clusters" $clusters | toJson | sha256sum -}}
{{- end -}}

{{/* rbac checks rbac.rendered: RBAC objects only, binding only the release's profile
ServiceAccounts. It is pasted output of opsward rbac render; anything else in it is a
mistake, or more than was reviewed. */}}
{{- define "opsward.rbac" -}}
{{- $allowed := list -}}
{{- range .Values.localProfiles -}}{{- $allowed = append $allowed (printf "opsward-%s" .) -}}{{- end -}}
{{- range (regexSplit "(?m)^---[ \t]*$" .Values.rbac.rendered -1) -}}
{{- $doc := fromYaml . -}}
{{- if hasKey $doc "Error" -}}
{{- fail (printf "rbac.rendered: %s" $doc.Error) -}}
{{- end -}}
{{- if $doc -}}
{{- if or (ne (default "" $doc.apiVersion) "rbac.authorization.k8s.io/v1") (not (has $doc.kind (list "Role" "RoleBinding" "ClusterRole" "ClusterRoleBinding"))) -}}
{{- fail (printf "rbac.rendered: %s is not an RBAC object" (default "an object without a kind" $doc.kind)) -}}
{{- end -}}
{{- range (default list $doc.subjects) -}}
{{- if or (ne .kind "ServiceAccount") (ne (default "" .namespace) $.Release.Namespace) (not (has .name $allowed)) -}}
{{- fail (printf "rbac.rendered: %s %s binds %s %s/%s; only the ServiceAccounts of localProfiles in %s may be bound" $doc.kind $doc.metadata.name .kind (default "" .namespace) .name $.Release.Namespace) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
