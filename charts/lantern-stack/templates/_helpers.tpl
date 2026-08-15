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

{{- define "lantern.collectorEndpoint" -}}
{{- if .Values.collector.enabled -}}
http://{{ .Release.Name }}-collector.{{ include "lantern.namespace" . }}:4318
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
