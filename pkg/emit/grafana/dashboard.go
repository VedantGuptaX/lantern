// Package grafana generates per-service Grafana dashboards.
//
// This targets the sidecar-based provisioning model that
// charts/lantern-stack/values-quickstart.yaml already runs: kube-prometheus-
// stack's bundled Grafana, with `sidecar.dashboards.enabled: true` and
// `searchNamespace: ALL`. A dashboard is emitted as a ConfigMap labeled
// `grafana_dashboard: "1"`; the sidecar picks it up with no further action.
//
// This is deliberately NOT the grafana-operator GrafanaDashboard/GrafanaFolder
// CRD path that ObservabilityStack's `backends.dashboards.folderStrategy` and
// `instanceLabels` fields anticipate (see api/v1alpha1/types.go and
// pkg/kube/object.go's Sort rank table, both of which already reserve room
// for it). That path needs grafana-operator installed and a Grafana CR
// pointing at the target instance — real infrastructure this project doesn't
// ship yet. The ConfigMap approach works with what's already deployed and
// tested today; `folderStrategy`/`instanceRef`/`instanceLabels` are read by
// nothing here and stay reserved for that future upgrade (P1, per plan.md).
// One concrete consequence: every dashboard lands in Grafana's default
// "General" folder — there is no per-team folder yet.
package grafana

import (
	"encoding/json"
	"fmt"
	"strings"

	api "github.com/VedantGuptaX/lantern/api/v1alpha1"
	"github.com/VedantGuptaX/lantern/pkg/kube"
	"github.com/VedantGuptaX/lantern/pkg/yamlx"
)

// Diagnostic mirrors the shape used by the other emitters.
type Diagnostic struct {
	Level   string
	Message string
}

// Input is the pre-resolved input for one service's dashboard.
type Input struct {
	Service   string
	Namespace string
	Team      string

	// SLOs drive the burn-rate and error-budget panels, built from the exact
	// recording rules pkg/emit/prom.BuildRules already emits — the numbers on
	// the dashboard are guaranteed to match what actually drives alerts,
	// because they're the same series.
	SLOs []api.SLO

	// ExtraPanels and Patches are the escape hatches from
	// ServiceObservability's spec.dashboard — see api/v1alpha1/types.go's
	// Panel and Patch doc comments.
	ExtraPanels []api.Panel
	Patches     []api.Patch

	// Datasource UIDs. Empty disables the corresponding panel group rather
	// than guessing a UID that might not exist — same "don't guess silently"
	// rule the ServiceMonitor port fallback now follows.
	PrometheusUID string
	LokiUID       string
	TempoUID      string

	LogsEnabled   bool
	TracesEnabled bool

	// Labels are applied to the ConfigMap object itself (lantern.dev/*), not
	// to the dashboard JSON.
	Labels map[string]string
}

// Build renders one service's dashboard as a sidecar-discovered ConfigMap.
// Returns (nil, diags, nil) if there is nothing meaningful to show — no SLOs
// and no configured datasources — rather than emitting an empty shell.
func Build(in Input) (*kube.Object, []Diagnostic, error) {
	var diags []Diagnostic

	if in.PrometheusUID == "" {
		diags = append(diags, Diagnostic{
			Level:   "info",
			Message: "backends.metrics.datasource is not set; SLO panels will be skipped on the generated dashboard",
		})
	}
	if in.LogsEnabled && in.LokiUID == "" {
		diags = append(diags, Diagnostic{
			Level:   "info",
			Message: "backends.logs.datasource is not set; the logs panel will be skipped on the generated dashboard",
		})
	}
	if in.TracesEnabled && in.TempoUID == "" {
		diags = append(diags, Diagnostic{
			Level:   "info",
			Message: "backends.traces.datasource is not set; the traces panel will be skipped on the generated dashboard",
		})
	}

	panels := newPanelBuilder()

	if in.PrometheusUID != "" {
		for _, slo := range in.SLOs {
			sloID := fmt.Sprintf("%s-%s", in.Service, slo.Name)
			panels.addRow(
				burnRatePanel(slo.Name, sloID, in.PrometheusUID),
				errorBudgetPanel(slo.Name, sloID, in.PrometheusUID),
			)
		}
	}
	if len(in.SLOs) == 0 || in.PrometheusUID == "" {
		if len(in.SLOs) == 0 {
			diags = append(diags, Diagnostic{
				Level:   "info",
				Message: "no SLOs declared; the dashboard has no burn-rate panels",
			})
		}
	}

	if in.LogsEnabled && in.LokiUID != "" {
		panels.addFull(logsPanel(in.Namespace, in.Service, in.LokiUID))
	}
	if in.TracesEnabled && in.TempoUID != "" {
		panels.addFull(tracesPanel(in.Service, in.TempoUID))
	}

	for _, p := range in.ExtraPanels {
		panels.addFull(extraPanel(p, in.PrometheusUID))
	}

	if len(panels.panels) == 0 {
		diags = append(diags, Diagnostic{
			Level:   "info",
			Message: "no dashboard emitted: no SLOs, no configured datasources, and no extraPanels — nothing to show",
		})
		return nil, diags, nil
	}

	dash := dashboardJSON{
		UID:           dashboardUID(in.Service),
		Title:         fmt.Sprintf("Lantern: %s", in.Service),
		Tags:          dashboardTags(in.Team),
		Timezone:      "browser",
		SchemaVersion: 39,
		Version:       1,
		Refresh:       "30s",
		Time:          timeRange{From: "now-6h", To: "now"},
		Panels:        panels.panels,
	}

	doc, err := toGenericJSON(dash)
	if err != nil {
		return nil, diags, fmt.Errorf("service %q: encoding dashboard: %w", in.Service, err)
	}

	for _, p := range in.Patches {
		doc, err = applyPatch(doc, p)
		if err != nil {
			return nil, diags, fmt.Errorf("service %q: dashboard patch %q %q: %w", in.Service, p.Op, p.Path, err)
		}
	}

	body, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, diags, fmt.Errorf("service %q: encoding patched dashboard: %w", in.Service, err)
	}

	cmLabels := map[string]string{"grafana_dashboard": "1"}
	for k, v := range in.Labels {
		cmLabels[k] = v
	}
	obj := kube.New("v1", "ConfigMap", "lantern-dashboard-"+in.Service, in.Namespace, cmLabels)
	data := yamlx.NewMap(in.Service+".json", yamlx.Lit(string(body)))
	obj.Set("data", data)

	return obj, diags, nil
}

// ---------------------------------------------------------------------------
// Dashboard JSON model. Structs, not map[string]any, so field order (and
// therefore output) is deterministic — the same discipline pkg/emit/prom and
// pkg/emit/otel already follow.

type dashboardJSON struct {
	UID           string      `json:"uid"`
	Title         string      `json:"title"`
	Tags          []string    `json:"tags"`
	Timezone      string      `json:"timezone"`
	SchemaVersion int         `json:"schemaVersion"`
	Version       int         `json:"version"`
	Refresh       string      `json:"refresh"`
	Time          timeRange   `json:"time"`
	Panels        []panelJSON `json:"panels"`
}

type timeRange struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type panelJSON struct {
	ID          int          `json:"id"`
	Title       string       `json:"title"`
	Description string       `json:"description,omitempty"`
	Type        string       `json:"type"`
	GridPos     gridPos      `json:"gridPos"`
	Datasource  dsRef        `json:"datasource"`
	Targets     []targetJSON `json:"targets,omitempty"`
	FieldConfig *fieldConfig `json:"fieldConfig,omitempty"`
}

type gridPos struct {
	H int `json:"h"`
	W int `json:"w"`
	X int `json:"x"`
	Y int `json:"y"`
}

type dsRef struct {
	Type string `json:"type"`
	UID  string `json:"uid"`
}

type targetJSON struct {
	Datasource   dsRef  `json:"datasource"`
	RefID        string `json:"refId"`
	Expr         string `json:"expr,omitempty"`
	LegendFormat string `json:"legendFormat,omitempty"`
	// Query/QueryType are used by Tempo's TraceQL target shape instead of
	// Expr/LegendFormat.
	Query     string `json:"query,omitempty"`
	QueryType string `json:"queryType,omitempty"`
}

type fieldConfig struct {
	Defaults fieldDefaults `json:"defaults"`
}

type fieldDefaults struct {
	Unit string `json:"unit,omitempty"`
}

// ---------------------------------------------------------------------------
// Panel builders. A 24-column grid, one row per SLO (timeseries + stat side
// by side), full-width rows for logs/traces/extra panels below.

type panelBuilder struct {
	panels []panelJSON
	nextID int
	y      int
}

func newPanelBuilder() *panelBuilder { return &panelBuilder{nextID: 1} }

func (b *panelBuilder) addRow(left, right panelJSON) {
	left.ID = b.nextID
	left.GridPos = gridPos{H: 8, W: 16, X: 0, Y: b.y}
	b.nextID++

	right.ID = b.nextID
	right.GridPos = gridPos{H: 8, W: 8, X: 16, Y: b.y}
	b.nextID++

	b.panels = append(b.panels, left, right)
	b.y += 8
}

func (b *panelBuilder) addFull(p panelJSON) {
	p.ID = b.nextID
	p.GridPos = gridPos{H: 10, W: 24, X: 0, Y: b.y}
	b.nextID++
	b.panels = append(b.panels, p)
	b.y += 10
}

func burnRatePanel(sloName, sloID, promUID string) panelJSON {
	ds := dsRef{Type: "prometheus", UID: promUID}
	return panelJSON{
		Title:       fmt.Sprintf("%s: SLI error ratio by window", sloName),
		Description: "Recorded by the same rules that drive burn-rate alerts (lantern:sli_error:ratio_rate*, lantern:current_burn_rate:ratio) — this panel and the alert can never disagree.",
		Type:        "timeseries",
		Datasource:  ds,
		Targets: []targetJSON{
			{
				Datasource:   ds,
				RefID:        "A",
				Expr:         fmt.Sprintf(`{__name__=~"lantern:sli_error:ratio_rate.+", lantern_id=%q}`, sloID),
				LegendFormat: "{{lantern_window}}",
			},
			{
				Datasource:   ds,
				RefID:        "B",
				Expr:         fmt.Sprintf(`lantern:current_burn_rate:ratio{lantern_id=%q}`, sloID),
				LegendFormat: "current burn rate",
			},
		},
	}
}

func errorBudgetPanel(sloName, sloID, promUID string) panelJSON {
	ds := dsRef{Type: "prometheus", UID: promUID}
	return panelJSON{
		Title:      fmt.Sprintf("%s: error budget remaining", sloName),
		Type:       "stat",
		Datasource: ds,
		Targets: []targetJSON{
			{
				Datasource: ds,
				RefID:      "A",
				Expr:       fmt.Sprintf(`lantern:period_error_budget_remaining:ratio{lantern_id=%q}`, sloID),
			},
		},
		FieldConfig: &fieldConfig{Defaults: fieldDefaults{Unit: "percentunit"}},
	}
}

func logsPanel(namespace, service, lokiUID string) panelJSON {
	ds := dsRef{Type: "loki", UID: lokiUID}
	return panelJSON{
		Title:       "Logs",
		Description: fmt.Sprintf("Requires Lantern's log-collection DaemonSet (logsCollector.enabled) to actually ship pod logs into Loki — see docs/getting-signals-into-grafana.md. Filtered by namespace=%q, deployment=%q.", namespace, service),
		Type:        "logs",
		Datasource:  ds,
		Targets: []targetJSON{
			{
				Datasource: ds,
				RefID:      "A",
				Expr:       fmt.Sprintf(`{namespace=%q, deployment=%q}`, namespace, service),
			},
		},
	}
}

func tracesPanel(service, tempoUID string) panelJSON {
	ds := dsRef{Type: "tempo", UID: tempoUID}
	return panelJSON{
		Title:       "Traces",
		Description: "Requires a live trace source pushing to the collector — an SDK agent, or a separately-installed eBPF probe (OBI/Beyla) for mode: ebpf. Lantern does not deploy the eBPF probe itself; see docs/getting-signals-into-grafana.md. The service name in this query assumes it matches the resolved resource.service.name; adjust if your instrumentation names it differently.",
		Type:        "traces",
		Datasource:  ds,
		Targets: []targetJSON{
			{
				Datasource: ds,
				RefID:      "A",
				QueryType:  "traceql",
				Query:      fmt.Sprintf(`{resource.service.name=%q}`, service),
			},
		},
	}
}

func extraPanel(p api.Panel, promUID string) panelJSON {
	viz := p.Viz
	if viz == "" {
		viz = "timeseries"
	}
	ds := dsRef{Type: "prometheus", UID: promUID}
	pj := panelJSON{
		Title:      p.Title,
		Type:       viz,
		Datasource: ds,
		Targets: []targetJSON{
			{
				Datasource:   ds,
				RefID:        "A",
				Expr:         p.Query,
				LegendFormat: p.Legend,
			},
		},
	}
	if p.Unit != "" {
		pj.FieldConfig = &fieldConfig{Defaults: fieldDefaults{Unit: p.Unit}}
	}
	return pj
}

func dashboardUID(service string) string {
	uid := "lantern-" + service
	if len(uid) > 40 {
		uid = uid[:40]
	}
	return uid
}

func dashboardTags(team string) []string {
	tags := []string{"lantern"}
	if team != "" {
		tags = append(tags, "team:"+team)
	}
	return tags
}

// ---------------------------------------------------------------------------
// RFC 6902 JSON Patch — the mandatory escape hatch declared by
// api.Patch. Supports add, replace, remove, test; move and copy are
// rejected loudly rather than silently ignored, since they're not
// implemented.

func toGenericJSON(v any) (any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func applyPatch(doc any, p api.Patch) (any, error) {
	ptr, err := parsePointer(p.Path)
	if err != nil {
		return nil, err
	}
	switch p.Op {
	case "add":
		return setAtPointer(doc, ptr, p.Value, true)
	case "replace":
		return setAtPointer(doc, ptr, p.Value, false)
	case "remove":
		return removeAtPointer(doc, ptr)
	case "test":
		got, err := getAtPointer(doc, ptr)
		if err != nil {
			return nil, err
		}
		gotJSON, _ := json.Marshal(got)
		wantJSON, _ := json.Marshal(p.Value)
		if string(gotJSON) != string(wantJSON) {
			return nil, fmt.Errorf("test failed: value at %q is %s, want %s", p.Path, gotJSON, wantJSON)
		}
		return doc, nil
	default:
		return nil, fmt.Errorf("unsupported patch op %q (only add, replace, remove, test are implemented)", p.Op)
	}
}

// pointer is a parsed RFC 6901 JSON Pointer.
type pointer []string

func parsePointer(path string) (pointer, error) {
	if path == "" {
		return nil, nil
	}
	if !strings.HasPrefix(path, "/") {
		return nil, fmt.Errorf("path %q must start with /", path)
	}
	parts := strings.Split(path[1:], "/")
	for i, p := range parts {
		p = strings.ReplaceAll(p, "~1", "/")
		p = strings.ReplaceAll(p, "~0", "~")
		parts[i] = p
	}
	return pointer(parts), nil
}

func getAtPointer(doc any, ptr pointer) (any, error) {
	cur := doc
	for _, seg := range ptr {
		switch node := cur.(type) {
		case map[string]any:
			v, ok := node[seg]
			if !ok {
				return nil, fmt.Errorf("no such key %q", seg)
			}
			cur = v
		case []any:
			idx, err := arrayIndex(seg, len(node), false)
			if err != nil {
				return nil, err
			}
			cur = node[idx]
		default:
			return nil, fmt.Errorf("cannot descend into %T at %q", cur, seg)
		}
	}
	return cur, nil
}

// setAtPointer sets the value at ptr, returning a new document root (the
// document is walked and rebuilt rather than mutated in place, since Go maps
// nested inside `any` are reference types but this keeps the recursion
// simple and side-effect-free).
func setAtPointer(doc any, ptr pointer, value any, insert bool) (any, error) {
	if len(ptr) == 0 {
		return value, nil
	}
	return setRecursive(doc, ptr, value, insert)
}

func setRecursive(node any, ptr pointer, value any, insert bool) (any, error) {
	seg := ptr[0]
	rest := ptr[1:]

	switch n := node.(type) {
	case map[string]any:
		if len(rest) == 0 {
			if !insert {
				if _, ok := n[seg]; !ok {
					return nil, fmt.Errorf("no such key %q to replace", seg)
				}
			}
			n[seg] = value
			return n, nil
		}
		child, ok := n[seg]
		if !ok {
			return nil, fmt.Errorf("no such key %q", seg)
		}
		updated, err := setRecursive(child, rest, value, insert)
		if err != nil {
			return nil, err
		}
		n[seg] = updated
		return n, nil

	case []any:
		if seg == "-" {
			if len(rest) != 0 {
				return nil, fmt.Errorf("cannot descend past array append marker \"-\"")
			}
			if !insert {
				return nil, fmt.Errorf("\"-\" is only valid for add, not replace")
			}
			return append(n, value), nil
		}
		// RFC 6902 §4.1: for "add", the target index may equal the array's
		// current length — that means "append", the same as the "-" marker
		// above. It's only legal one level down from here (i.e. this is the
		// final segment and we're inserting), not for a plain replace or for
		// descending further into the array.
		allowEqualLength := insert && len(rest) == 0
		idx, err := arrayIndex(seg, len(n), allowEqualLength)
		if err != nil {
			return nil, err
		}
		if len(rest) == 0 {
			if insert {
				n = append(n, nil)
				copy(n[idx+1:], n[idx:])
				n[idx] = value
				return n, nil
			}
			n[idx] = value
			return n, nil
		}
		updated, err := setRecursive(n[idx], rest, value, insert)
		if err != nil {
			return nil, err
		}
		n[idx] = updated
		return n, nil

	default:
		return nil, fmt.Errorf("cannot descend into %T at %q", node, seg)
	}
}

func removeAtPointer(doc any, ptr pointer) (any, error) {
	if len(ptr) == 0 {
		return nil, fmt.Errorf("cannot remove the document root")
	}
	return removeRecursive(doc, ptr)
}

func removeRecursive(node any, ptr pointer) (any, error) {
	seg := ptr[0]
	rest := ptr[1:]

	switch n := node.(type) {
	case map[string]any:
		if len(rest) == 0 {
			if _, ok := n[seg]; !ok {
				return nil, fmt.Errorf("no such key %q to remove", seg)
			}
			delete(n, seg)
			return n, nil
		}
		child, ok := n[seg]
		if !ok {
			return nil, fmt.Errorf("no such key %q", seg)
		}
		updated, err := removeRecursive(child, rest)
		if err != nil {
			return nil, err
		}
		n[seg] = updated
		return n, nil

	case []any:
		idx, err := arrayIndex(seg, len(n), false)
		if err != nil {
			return nil, err
		}
		if len(rest) == 0 {
			return append(n[:idx], n[idx+1:]...), nil
		}
		updated, err := removeRecursive(n[idx], rest)
		if err != nil {
			return nil, err
		}
		n[idx] = updated
		return n, nil

	default:
		return nil, fmt.Errorf("cannot descend into %T at %q", node, seg)
	}
}

// arrayIndex parses seg as a non-negative array index. allowEqualLength
// permits idx == length (RFC 6902 §4.1: an "add" target index equal to the
// array's current length means append, same as the "-" marker) — callers
// doing a get/replace/remove must pass false, since indexing past the last
// element is never valid for those.
func arrayIndex(seg string, length int, allowEqualLength bool) (int, error) {
	var idx int
	if _, err := fmt.Sscanf(seg, "%d", &idx); err != nil {
		return 0, fmt.Errorf("array index %q is not a number", seg)
	}
	max := length
	if allowEqualLength {
		max = length + 1
	}
	if idx < 0 || idx >= max {
		return 0, fmt.Errorf("array index %d out of range (length %d)", idx, length)
	}
	return idx, nil
}
