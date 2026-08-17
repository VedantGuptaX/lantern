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

{{/*
The in-cluster Service name kube-prometheus-stack actually creates for
Prometheus.

This has to replicate that subchart's own naming EXACTLY, and its fullname
helper truncates to 26 characters -- not Helm's usual 63. For a release
called "lantern" that makes "lantern-kube-prometheus-stack" become
"lantern-kube-prometheus-st", and its prometheus/service.yaml then appends
"-prometheus", giving "lantern-kube-prometheus-st-prometheus".

BUG THIS FIXES, found on a real cluster: this used to hardcode
"{{ .Release.Name }}-kube-prometheus-prometheus" -- dropping the "st" the
truncation leaves behind. That name resolves to nothing, so the collector's
prometheusremotewrite exporter timed out and DROPPED every application
metric it received ("Permanent error: context deadline exceeded",
~55 metrics every 10s, continuously). Nothing downstream reported a
problem: the collector logged at error level inside its own pod and kept
running, Prometheus was healthy, and the chart installed cleanly. The only
visible symptom was that every SLI recording rule, per-service dashboard
panel, and SLO burn-rate alert stayed permanently empty, because the
metric they all select on (http_server_request_duration_seconds) never
reached Prometheus at all.

Mirrors kube-prometheus-stack.fullname's own logic, including the
"release name already contains the chart name" branch.
*/}}
{{- define "lantern.kubePrometheusStackFullname" -}}
{{- $name := "kube-prometheus-stack" -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 26 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 26 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "lantern.prometheusService" -}}
{{ include "lantern.kubePrometheusStackFullname" . }}-prometheus.{{ include "lantern.namespace" . }}:9090
{{- end -}}

{{/*
kube-prometheus-stack.enabled turns on the subchart, but the subchart has
its OWN independent prometheus.enabled underneath it (Grafana, Alertmanager,
and kube-state-metrics can each be toggled separately from Prometheus
itself) -- a real, deliberate feature of that subchart, not a corner case.
Checking only the outer toggle here would repeat exactly the bug just fixed
for traces/logs in observabilitystack.yaml: claim a working Prometheus
endpoint exists when kube-prometheus-stack is on for Grafana alone and its
own prometheus.enabled is false. Grafana without a scraped-metrics backend
is a real combination -- e.g. wanting a UI for Loki/Tempo without also
running Prometheus/Alertmanager/kube-state-metrics.
*/}}
{{- define "lantern.metricsBackendReady" -}}
{{- if and (index .Values "kube-prometheus-stack" "enabled") (index .Values "kube-prometheus-stack" "prometheus" "enabled") -}}
true
{{- end -}}
{{- end -}}

{{- define "lantern.metricsRemoteWrite" -}}
{{- if .Values.backends.metrics.remoteWrite -}}
{{ .Values.backends.metrics.remoteWrite }}
{{- else if (include "lantern.metricsBackendReady" .) -}}
http://{{ include "lantern.prometheusService" . }}/api/v1/write
{{- end -}}
{{- end -}}

{{- define "lantern.metricsQueryURL" -}}
{{- if .Values.backends.metrics.queryURL -}}
{{ .Values.backends.metrics.queryURL }}
{{- else if (include "lantern.metricsBackendReady" .) -}}
http://{{ include "lantern.prometheusService" . }}
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
