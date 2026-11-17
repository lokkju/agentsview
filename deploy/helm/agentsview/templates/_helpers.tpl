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

{{/*
Server data directory. The volume root at /data is owned by root (fsGroup
only adds group access), and the server requires a data directory owned by
its own uid. The init container creates this subdirectory as uid 1000.
*/}}
{{- define "agentsview.dataDir" -}}
/data/agentsview
{{- end }}

{{/*
Secret holding the running server's database login. pg-serve falls back to
the CNPG application Secret; hosted mode has no default because the server
refuses any role with privileges outside its tenant schema.
*/}}
{{- define "agentsview.pgCredentialsSecret" -}}
{{- $secret := .Values.postgres.credentials.existingSecret -}}
{{- if and (not $secret) (eq .Values.mode "pg-serve") -}}
{{- $secret = "agentsview-db-app" -}}
{{- end -}}
{{- $secret -}}
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

{{/*
Runtime-role grants for hosted mode, printed by NOTES.txt. Keep in sync with
the README and internal/postgres/raw_runtime_privileges.go. RUNTIME_ROLE is a
placeholder; the chart cannot read the role name from its Secret.
*/}}
{{- define "agentsview.runtimeGrants" -}}
{{- $s := .Values.hosted.schema -}}
GRANT CONNECT ON DATABASE {{ .Values.postgres.database }} TO RUNTIME_ROLE;
GRANT USAGE ON SCHEMA {{ $s }} TO RUNTIME_ROLE;
GRANT SELECT ON ALL TABLES IN SCHEMA {{ $s }} TO RUNTIME_ROLE;
GRANT UPDATE (dialogue_text) ON {{ $s }}.messages TO RUNTIME_ROLE;
GRANT INSERT ON {{ $s }}.raw_device_tokens, {{ $s }}.raw_manifest_entries,
  {{ $s }}.raw_manifest_objects, {{ $s }}.messages, {{ $s }}.tool_calls,
  {{ $s }}.tool_result_events, {{ $s }}.usage_events,
  {{ $s }}.session_plan_usage, {{ $s }}.secret_findings,
  {{ $s }}.raw_projection_generations, {{ $s }}.raw_source_contributions,
  {{ $s }}.raw_session_public_aliases, {{ $s }}.raw_embedding_outbox,
  {{ $s }}.raw_manifests TO RUNTIME_ROLE;
GRANT UPDATE (observed_outcome, observed_repeat, sequence_ending)
  ON {{ $s }}.tool_calls TO RUNTIME_ROLE;
GRANT INSERT, UPDATE ON {{ $s }}.raw_objects, {{ $s }}.raw_source_heads,
  {{ $s }}.raw_ingest_jobs, {{ $s }}.raw_source_projections,
  {{ $s }}.raw_session_groups, {{ $s }}.raw_content_revisions,
  {{ $s }}.raw_session_branches, {{ $s }}.raw_corpus_state,
  {{ $s }}.raw_projection_rollouts, {{ $s }}.excluded_sessions TO RUNTIME_ROLE;
GRANT SELECT, INSERT, UPDATE, DELETE ON {{ $s }}.raw_message_dialogue TO RUNTIME_ROLE;
GRANT INSERT, UPDATE, DELETE ON {{ $s }}.raw_upload_sessions, {{ $s }}.sessions,
  {{ $s }}.session_sources, {{ $s }}.pinned_messages, {{ $s }}.raw_curation,
  {{ $s }}.raw_pins TO RUNTIME_ROLE;
GRANT INSERT, DELETE ON {{ $s }}.starred_sessions, {{ $s }}.raw_session_links,
  {{ $s }}.friction_findings TO RUNTIME_ROLE;
GRANT USAGE ON SEQUENCE {{ $s }}.raw_ingest_jobs_id_seq, {{ $s }}.tool_calls_id_seq,
  {{ $s }}.tool_result_events_id_seq, {{ $s }}.usage_events_id_seq,
  {{ $s }}.pinned_messages_id_seq TO RUNTIME_ROLE;
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
{{- if and (has $v.postgres.sslMode (list "verify-ca" "verify-full")) (not $v.postgres.caSecret.name) -}}
{{- fail (printf "postgres.sslMode %s needs postgres.caSecret.name." $v.postgres.sslMode) -}}
{{- end -}}
{{- if and $v.auth.required (not $v.auth.existingSecret) -}}
{{- fail "auth.existingSecret is required when auth.required is true. Without it the server generates a token that changes on every restart." -}}
{{- end -}}
{{- if and (not $v.auth.required) (not $v.server.publicURL) -}}
{{- fail "auth.required=false is meant for a server behind an authenticating proxy; set server.publicURL to the proxy's browser URL." -}}
{{- end -}}
{{- if and $v.networkPolicy.enabled (eq $v.networkPolicy.type "kubernetes") (not $v.networkPolicy.nodeCIDRs) -}}
{{- fail "networkPolicy.type kubernetes needs networkPolicy.nodeCIDRs: kubelet probes come from the node, and without its addresses the policy blocks them. On Cilium, set networkPolicy.type=cilium instead." -}}
{{- end -}}
{{- if eq $v.mode "hosted" -}}
{{- if not $v.postgres.credentials.existingSecret -}}
{{- fail "hosted mode requires postgres.credentials.existingSecret: a Secret for a dedicated runtime role that owns nothing and has no privileges on any other schema. The CNPG application Secret does not qualify: that role owns the database, and the server refuses it. See README.md, \"Create the runtime role\"." -}}
{{- end -}}
{{- if and $v.hosted.provision.enabled (eq $v.postgres.credentials.existingSecret $v.hosted.provision.credentials.existingSecret) -}}
{{- fail (printf "postgres.credentials.existingSecret and hosted.provision.credentials.existingSecret are both %q. The provision Job needs the schema owner and the server needs a separate restricted runtime role; see README.md, \"Create the runtime role\"." $v.postgres.credentials.existingSecret) -}}
{{- end -}}
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
