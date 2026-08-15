// Package v1alpha1 defines the Lantern API contract.
//
// These types are the stable interface. The SDKs synthesize them, the CLI
// consumes them, and the operator (P2) will reconcile them. Everything below
// the compiler is an implementation detail that may change; this package may
// not.
package v1alpha1

const (
	// GroupVersion is the API group/version of both Lantern kinds.
	GroupVersion = "lantern.dev/v1alpha1"

	KindServiceObservability = "ServiceObservability"
	KindObservabilityStack   = "ObservabilityStack"

	// ManagedLabel marks every object the compiler emits, so `lantern diff`
	// and future pruning can identify what it owns.
	ManagedLabel = "lantern.dev/managed"
	// ServiceLabel links an emitted object back to its source spec.
	ServiceLabel = "lantern.dev/service"
	// TeamLabel carries ownership onto alerts for Alertmanager routing.
	TeamLabel = "lantern.dev/team"
)

// ---------------------------------------------------------------------------
// ServiceObservability — namespaced, owned by the developer
// ---------------------------------------------------------------------------

type ServiceObservability struct {
	APIVersion string                   `json:"apiVersion"`
	Kind       string                   `json:"kind"`
	Metadata   ObjectMeta               `json:"metadata"`
	Spec       ServiceObservabilitySpec `json:"spec"`
}

type ObjectMeta struct {
	Name        string            `json:"name"`
	Namespace   string            `json:"namespace,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

type ServiceObservabilitySpec struct {
	Target          Target             `json:"target"`
	ServiceKind     ServiceKind        `json:"serviceKind"`
	Team            string             `json:"team"`
	Signals         Signals            `json:"signals,omitempty"`
	SLOs            []SLO              `json:"slos,omitempty"`
	Dashboard       Dashboard          `json:"dashboard,omitempty"`
	Instrumentation InstrumentationCfg `json:"instrumentation,omitempty"`
	Attributes      map[string]string  `json:"attributes,omitempty"`
}

// Target identifies the workload to instrument.
type Target struct {
	Kind     string            `json:"kind,omitempty"` // Deployment | StatefulSet | DaemonSet
	Name     string            `json:"name,omitempty"`
	Selector map[string]string `json:"selector,omitempty"`
	// MetricsPort names the Service port exposing Prometheus metrics.
	MetricsPort string `json:"metricsPort,omitempty"`
}

// ServiceKind selects the dashboard template and the semantic-convention
// metric family the compiler generates queries against.
type ServiceKind string

const (
	KindHTTP     ServiceKind = "http"
	KindGRPC     ServiceKind = "grpc"
	KindWorker   ServiceKind = "worker"
	KindCron     ServiceKind = "cron"
	KindDatabase ServiceKind = "database"
	KindCustom   ServiceKind = "custom"
)

type Signals struct {
	Metrics  *bool       `json:"metrics,omitempty"`
	Traces   TraceCfg    `json:"traces,omitempty"`
	Logs     LogCfg      `json:"logs,omitempty"`
	Profiles *ProfileCfg `json:"profiles,omitempty"`
}

type TraceCfg struct {
	Enabled      *bool    `json:"enabled,omitempty"`
	SamplingRate *float64 `json:"samplingRate,omitempty"`
}

type LogCfg struct {
	Enabled          *bool `json:"enabled,omitempty"`
	CorrelateTraceID *bool `json:"correlateTraceID,omitempty"`
}

type ProfileCfg struct {
	Enabled *bool `json:"enabled,omitempty"`
}

// SLOType selects the SLI expression template.
type SLOType string

const (
	SLOAvailability SLOType = "availability"
	SLOLatency      SLOType = "latency"
	SLOCustom       SLOType = "custom"
)

type SLO struct {
	Name      string  `json:"name"`
	Type      SLOType `json:"type"`
	Objective float64 `json:"objective"`           // e.g. 99.9
	Threshold string  `json:"threshold,omitempty"` // latency only, e.g. "300ms"
	Window    string  `json:"window,omitempty"`    // default 30d
	// Custom SLIs supply their own ratio. Both are required when Type=custom.
	ErrorQuery string `json:"errorQuery,omitempty"`
	TotalQuery string `json:"totalQuery,omitempty"`
}

type Dashboard struct {
	ExtraPanels []Panel `json:"extraPanels,omitempty"`
	Patches     []Patch `json:"patches,omitempty"`
	Disabled    bool    `json:"disabled,omitempty"`
}

type Panel struct {
	Title  string `json:"title"`
	Query  string `json:"query"`
	Viz    string `json:"viz,omitempty"`
	Unit   string `json:"unit,omitempty"`
	Legend string `json:"legend,omitempty"`
}

// Patch is an RFC-6902 operation applied to the generated dashboard. This is
// the mandatory escape hatch: a developer who cannot override the generator
// will abandon the tool and hand-write JSON.
type Patch struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value any    `json:"value,omitempty"`
	From  string `json:"from,omitempty"`
}

// InstrumentationMode selects how telemetry gets into the workload.
type InstrumentationMode string

const (
	ModeAuto    InstrumentationMode = "auto"
	ModeAgent   InstrumentationMode = "agent"
	ModeEBPF    InstrumentationMode = "ebpf"
	ModeSDKOnly InstrumentationMode = "sdk-only"
	ModeNone    InstrumentationMode = "none"
)

type InstrumentationCfg struct {
	Mode    InstrumentationMode `json:"mode,omitempty"`
	Runtime Runtime             `json:"runtime,omitempty"`
}

// Runtime is the detected or declared application runtime.
type Runtime string

const (
	RuntimeJava     Runtime = "java"
	RuntimeNodeJS   Runtime = "nodejs"
	RuntimePython   Runtime = "python"
	RuntimeDotNet   Runtime = "dotnet"
	RuntimeGo       Runtime = "go"
	RuntimeOther    Runtime = "other"
	RuntimeUnknown  Runtime = ""
	RuntimePreInstr Runtime = "already-instrumented"
)

// ---------------------------------------------------------------------------
// ObservabilityStack — cluster-scoped, owned by the platform team
// ---------------------------------------------------------------------------

type ObservabilityStack struct {
	APIVersion string                 `json:"apiVersion"`
	Kind       string                 `json:"kind"`
	Metadata   ObjectMeta             `json:"metadata"`
	Spec       ObservabilityStackSpec `json:"spec"`
}

type ObservabilityStackSpec struct {
	Backends        Backends             `json:"backends"`
	Instrumentation StackInstrumentation `json:"instrumentation,omitempty"`
	Policy          Policy               `json:"policy,omitempty"`
	Defaults        Defaults             `json:"defaults,omitempty"`
}

type Backends struct {
	Metrics    MetricsBackend    `json:"metrics,omitempty"`
	Traces     TracesBackend     `json:"traces,omitempty"`
	Logs       LogsBackend       `json:"logs,omitempty"`
	Dashboards DashboardsBackend `json:"dashboards,omitempty"`
}

type MetricsBackend struct {
	Type        string `json:"type,omitempty"` // prometheus | mimir | victoriametrics
	RemoteWrite string `json:"remoteWrite,omitempty"`
	QueryURL    string `json:"queryURL,omitempty"`
	Operator    string `json:"operator,omitempty"`   // prometheus-operator | none
	Datasource  string `json:"datasource,omitempty"` // Grafana datasource uid
	// RuleNamespace overrides where PrometheusRule objects are written.
	RuleNamespace string `json:"ruleNamespace,omitempty"`
}

type TracesBackend struct {
	Type         string `json:"type,omitempty"`
	OTLPEndpoint string `json:"otlpEndpoint,omitempty"`
	Datasource   string `json:"datasource,omitempty"`
}

type LogsBackend struct {
	Type       string `json:"type,omitempty"`
	Endpoint   string `json:"endpoint,omitempty"`
	Datasource string `json:"datasource,omitempty"`
}

type DashboardsBackend struct {
	Type           string            `json:"type,omitempty"` // grafana | perses
	InstanceRef    InstanceRef       `json:"instanceRef,omitempty"`
	FolderStrategy string            `json:"folderStrategy,omitempty"` // per-team | per-namespace | flat
	InstanceLabels map[string]string `json:"instanceLabels,omitempty"`
}

type InstanceRef struct {
	Name      string `json:"name,omitempty"`
	Namespace string `json:"namespace,omitempty"`
}

type StackInstrumentation struct {
	Provider  string  `json:"provider,omitempty"` // otel-operator | odigos | none
	Namespace string  `json:"namespace,omitempty"`
	Collector string  `json:"collector,omitempty"` // OTLP endpoint agents export to
	EBPF      EBPFCfg `json:"ebpf,omitempty"`
}

type EBPFCfg struct {
	Enabled    bool   `json:"enabled,omitempty"`
	Provider   string `json:"provider,omitempty"` // obi | odigos
	Privileged bool   `json:"privileged,omitempty"`
}

// Policy holds guardrails developers cannot exceed. The compiler enforces
// these; it does not merely warn.
type Policy struct {
	MaxSeriesPerService int      `json:"maxSeriesPerService,omitempty"`
	MinSamplingRate     *float64 `json:"minSamplingRate,omitempty"`
	MaxSamplingRate     *float64 `json:"maxSamplingRate,omitempty"`
	RequireTeamLabel    *bool    `json:"requireTeamLabel,omitempty"`
	MaxRouteCardinality int      `json:"maxRouteCardinality,omitempty"`
	DenyLabels          []string `json:"denyLabels,omitempty"`
}

type Defaults struct {
	SLO       DefaultSLO `json:"slo,omitempty"`
	Retention Retention  `json:"retention,omitempty"`
	// Environment is stamped onto every service as deployment.environment.
	Environment string `json:"environment,omitempty"`
}

type DefaultSLO struct {
	Availability *float64        `json:"availability,omitempty"`
	Latency      *DefaultLatency `json:"latency,omitempty"`
}

type DefaultLatency struct {
	Objective float64 `json:"objective"`
	Threshold string  `json:"threshold"`
}

type Retention struct {
	Traces  string `json:"traces,omitempty"`
	Metrics string `json:"metrics,omitempty"`
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// BoolValue dereferences an optional bool with a default.
func BoolValue(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}

// FloatValue dereferences an optional float with a default.
func FloatValue(p *float64, def float64) float64 {
	if p == nil {
		return def
	}
	return *p
}

// Ptr returns a pointer to v, for building specs in Go.
func Ptr[T any](v T) *T { return &v }
