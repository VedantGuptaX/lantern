{{/*
Endpoint resolution.

Each helper prefers an explicitly configured backend address. If none is set
and the matching subchart is enabled, it falls back to that subchart's
in-cluster service. This is what lets the same chart serve both the
bring-your-own and quickstart paths without two sets of templates.

If neither is available the helper returns empty, and NOTES.txt tells the user
which setting is missing rather than silently rendering a broken stack.
*/}}

{{- define "lantern.namespace" -}}
{{- default .Release.Namespace .Values.namespace -}}
{{- end -}}

{{/*
The OpenTelemetryCollector CR this chart creates is named
"<release>-collector" (see templates/collector.yaml), and the OTel Operator
always appends its own "-collector" suffix to whatever Service it generates
for a CR -- regardless of what the CR's own name already contains. That
means the real, live Service is "<release>-collector-collector", not
"<release>-collector". Verified against a real cluster: `kubectl get svc -n
observability` shows "lantern-collector-collector" and
"lantern-logs-collector-collector", both with the suffix doubled. A
single-suffix endpoint here silently breaks every mode: agent/sdk
Instrumentation CR's exporter -- the injected agent just can't reach a
Service that doesn't exist, with no error at apply time. eBPF mode (OBI)
doesn't go through this path, which is why this went unnoticed until
agent-mode instrumentation was actually tried for real.
*/}}
{{- define "lantern.collectorEndpoint" -}}
{{- if .Values.collector.enabled -}}
http://{{ .Release.Name }}-collector-collector.{{ include "lantern.namespace" . }}:4318
{{- end -}}
{{- end -}}

{{- define "lantern.tracesEndpoint" -}}
{{- if .Values.backends.traces.otlpEndpoint -}}
{{ .Values.backends.traces.otlpEndpoint }}
{{- else if (index .Values "tempo" "enabled") -}}
{{ .Release.Name }}-tempo.{{ include "lantern.namespace" . }}:4317
{{- end -}}
{{- end -}}

{{- define "lantern.metricsRemoteWrite" -}}
{{- if .Values.backends.metrics.remoteWrite -}}
{{ .Values.backends.metrics.remoteWrite }}
{{- else if (index .Values "kube-prometheus-stack" "enabled") -}}
http://{{ .Release.Name }}-kube-prometheus-prometheus.{{ include "lantern.namespace" . }}:9090/api/v1/write
{{- end -}}
{{- end -}}

{{- define "lantern.metricsQueryURL" -}}
{{- if .Values.backends.metrics.queryURL -}}
{{ .Values.backends.metrics.queryURL }}
{{- else if (index .Values "kube-prometheus-stack" "enabled") -}}
http://{{ .Release.Name }}-kube-prometheus-prometheus.{{ include "lantern.namespace" . }}:9090
{{- end -}}
{{- end -}}

{{- define "lantern.logsEndpoint" -}}
{{- if .Values.backends.logs.endpoint -}}
{{ .Values.backends.logs.endpoint }}
{{- else if (index .Values "loki" "enabled") -}}
http://{{ .Release.Name }}-loki.{{ include "lantern.namespace" . }}:3100
{{- end -}}
{{- end -}}

{{- define "lantern.labels" -}}
app.kubernetes.io/name: lantern-stack
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{/*
True once the OpenTelemetryCollector CRD is registered on the API server.

Why this matters: the opentelemetry-operator subchart ships that CRD outside
Helm's special crds/ directory (in conf/crds/ instead — a template, not a
pre-install-guaranteed object), so on a genuinely fresh cluster it lands in
the same apply batch as any OpenTelemetryCollector custom resource this
chart also creates. Helm builds and REST-maps the entire release manifest
against the API server's CURRENT schema before applying anything, so a CR of
a kind that doesn't exist yet fails the whole install outright — "ensure
CRDs are installed first" — even though the very same release would have
installed that CRD moments later. Found on a real first-ever `helm install`
against an empty cluster, not a hypothetical.

.Capabilities.APIVersions reflects the server's state as of the START of
this helm operation, before this release's own crds/ objects are applied, so
this check reads the same "not there yet" on both a first install and a
`helm template` dry run. Gating collector.yaml / logs-collector.yaml's CR on
this makes a first install skip creating them gracefully (see NOTES.txt for
the follow-up instruction) instead of hard-failing the entire release.
`make install-quickstart` runs the follow-up automatically.
*/}}
{{- define "lantern.otelCollectorCRDReady" -}}
{{- if .Capabilities.APIVersions.Has "opentelemetry.io/v1beta1/OpenTelemetryCollector" -}}
true
{{- end -}}
{{- end -}}
