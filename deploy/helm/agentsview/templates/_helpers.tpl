{{/* Chart name, truncated to the 63-character label limit. */}}
{{- define "agentsview.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/* Fully qualified app name. */}}
{{- define "agentsview.fullname" -}}
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

{{- define "agentsview.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "agentsview.selectorLabels" -}}
app.kubernetes.io/name: {{ include "agentsview.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "agentsview.labels" -}}
helm.sh/chart: {{ include "agentsview.chart" . }}
{{ include "agentsview.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "agentsview.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "agentsview.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{- define "agentsview.image" -}}
{{- if .Values.image.digest }}
{{- printf "%s@%s" .Values.image.repository .Values.image.digest }}
{{- else }}
{{- printf "%s:%s" .Values.image.repository .Values.image.tag }}
{{- end }}
{{- end }}

{{- define "agentsview.pvcName" -}}
{{- default (printf "%s-data" (include "agentsview.fullname" .)) .Values.persistence.existingClaim }}
{{- end }}

{{/* Path of the PostgreSQL CA bundle inside the containers. */}}
{{- define "agentsview.pgCAPath" -}}
/etc/agentsview/postgres-ca/ca.crt
{{- end }}

{{/*
PostgreSQL URL without credentials. The driver reads PGUSER and PGPASSWORD
from the environment, so passwords never appear in config.toml or values.
*/}}
{{- define "agentsview.pgURL" -}}
{{- $pg := .Values.postgres -}}
{{- $url := printf "postgres://%s:%d/%s?sslmode=%s" $pg.host (int $pg.port) $pg.database $pg.sslMode -}}
{{- if $pg.caSecret.name -}}
{{- $url = printf "%s&sslrootcert=%s" $url (include "agentsview.pgCAPath" .) -}}
{{- end -}}
{{- $url -}}
{{- end }}

{{/* Fail rendering on combinations that cannot work. */}}
{{- define "agentsview.validate" -}}
{{- $v := .Values -}}
{{- if not (has $v.mode (list "pg-serve" "hosted")) -}}
{{- fail (printf "mode must be pg-serve or hosted, got %q" $v.mode) -}}
{{- end -}}
{{- if not $v.image.repository -}}
{{- fail "image.repository is required. Build the AgentsView Dockerfile from a known commit and push it to a registry the cluster can pull from; see README.md." -}}
{{- end -}}
{{- if and (not $v.image.tag) (not $v.image.digest) -}}
{{- fail "image.tag or image.digest is required. Pin an exact build; never use latest." -}}
{{- end -}}
{{- if eq (toString $v.image.tag) "latest" -}}
{{- fail "image.tag must not be latest. Pin an exact build or release tag." -}}
{{- end -}}
{{- if not $v.postgres.host -}}
{{- fail "postgres.host is required." -}}
{{- end -}}
{{- if not $v.postgres.credentials.existingSecret -}}
{{- fail "postgres.credentials.existingSecret is required; the chart never takes a database password as a value." -}}
{{- end -}}
{{- if and (has $v.postgres.sslMode (list "verify-ca" "verify-full")) (not $v.postgres.caSecret.name) -}}
{{- fail (printf "postgres.sslMode %s needs postgres.caSecret.name." $v.postgres.sslMode) -}}
{{- end -}}
{{- if and $v.auth.required (not $v.auth.existingSecret) -}}
{{- fail "auth.existingSecret is required when auth.required is true. Without it the server generates a token that changes on every restart." -}}
{{- end -}}
{{- if and (not $v.auth.required) (not $v.server.publicURL) -}}
{{- fail "auth.required=false is meant for a server behind an authenticating proxy; set server.publicURL to the proxy's browser URL." -}}
{{- end -}}
{{- if eq $v.mode "hosted" -}}
{{- if not $v.hosted.tenant -}}
{{- fail "hosted.tenant is required in hosted mode." -}}
{{- end -}}
{{- if not $v.auth.required -}}
{{- fail "hosted mode requires auth.required=true. The server refuses raw derivation without authentication, and raw-sync device routes must stay reachable without a proxy login." -}}
{{- end -}}
{{- if not $v.cursorSecret.existingSecret -}}
{{- fail "cursorSecret.existingSecret is required in hosted mode; the cursor secret must stay stable across restarts." -}}
{{- end -}}
{{- if not $v.persistence.enabled -}}
{{- fail "hosted mode requires persistence.enabled=true; the raw custody repository lives in the data directory." -}}
{{- end -}}
{{- if $v.hosted.rawDerivation -}}
{{- if not $v.runtimeClassName -}}
{{- fail "hosted.rawDerivation needs runtimeClassName, such as kata-clh-runtime-rs. The parser sandbox fails closed under runc on nodes that restrict unprivileged user namespaces; see README.md." -}}
{{- end -}}
{{- if and $v.hosted.requireKataRuntime (not (contains "kata" $v.runtimeClassName)) -}}
{{- fail (printf "runtimeClassName %q is not a Kata runtime class. The parser sandbox is verified only under Kata; gVisor fails its data limit. Set hosted.requireKataRuntime=false only for a runtime class you have tested." $v.runtimeClassName) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end }}
