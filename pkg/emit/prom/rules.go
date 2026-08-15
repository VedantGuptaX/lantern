package prom

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	api "github.com/VedantGuptaX/lantern/api/v1alpha1"
	"github.com/VedantGuptaX/lantern/pkg/kube"
	"github.com/VedantGuptaX/lantern/pkg/yamlx"
)

// BurnWindow is one multiwindow-multiburn alert pair.
//
// Factors and window pairs follow the Google SRE workbook's
// "Multiwindow, Multi-Burn-Rate Alerts" table for a 30-day period. The short
// window suppresses alerts for spikes that have already stopped; the long
// window establishes that the budget is genuinely being consumed.
type BurnWindow struct {
	Severity string
	Short    string
	Long     string
	Factor   float64
	// BudgetConsumed is how much of the 30d budget this pair implies is gone
	// by the time it fires; carried into the annotation so an on-call engineer
	// understands the urgency without looking up the table.
	BudgetConsumed string
}

// DefaultBurnWindows is the standard 30-day ladder.
var DefaultBurnWindows = []BurnWindow{
	{Severity: "critical", Short: "5m", Long: "1h", Factor: 14.4, BudgetConsumed: "2%"},
	{Severity: "critical", Short: "30m", Long: "6h", Factor: 6, BudgetConsumed: "5%"},
	{Severity: "warning", Short: "2h", Long: "1d", Factor: 3, BudgetConsumed: "10%"},
	{Severity: "warning", Short: "6h", Long: "3d", Factor: 1, BudgetConsumed: "10%"},
}

// rateWindows are every window an SLI recording rule is materialised at. They
// are the union of the short and long windows above; recording them once and
// referencing them from alerts keeps alert evaluation cheap.
var rateWindows = []string{"5m", "30m", "1h", "2h", "6h", "1d", "3d"}

// RuleInput is everything the rule generator needs, pre-resolved.
type RuleInput struct {
	Service     string
	Namespace   string
	Team        string
	ServiceKind api.ServiceKind
	Selector    string
	SLOs        []api.SLO
	Labels      map[string]string
	// RuleNamespace is where the PrometheusRule object is written, which may
	// differ from the workload namespace.
	RuleNamespace string
}

// Diagnostic is a non-fatal compiler message.
type Diagnostic struct {
	Level   string // warn | info
	Message string
}

// BuildRules emits one PrometheusRule containing, per SLO:
//   - SLI error-ratio recording rules at every burn window
//   - metadata recording rules (objective, error budget, burn rate)
//   - multiwindow-multiburn alerts
//
// Recording rules are generated rather than hand-written because the burn-rate
// arithmetic is the single easiest thing to get subtly wrong by hand, and a
// subtly wrong burn-rate alert is worse than no alert at all.
func BuildRules(in RuleInput) (*kube.Object, []Diagnostic, error) {
	var diags []Diagnostic
	groups := yamlx.NewSeq()

	for _, slo := range in.SLOs {
		sli, err := BuildSLI(slo, in.ServiceKind, in.Selector)
		if err != nil {
			return nil, nil, err
		}
		if sli.Experimental {
			diags = append(diags, Diagnostic{
				Level: "warn",
				Message: fmt.Sprintf(
					"slo %q builds on experimental OpenTelemetry semantic conventions for serviceKind %q; "+
						"the underlying metric may be renamed upstream. Pin your semconv version.",
					slo.Name, in.ServiceKind),
			})
		}

		window := slo.Window
		if window == "" {
			window = "30d"
		}
		if _, err := ParseWindow(window); err != nil {
			return nil, nil, fmt.Errorf("slo %q: %w", slo.Name, err)
		}

		objective := slo.Objective
		if objective <= 0 || objective >= 100 {
			return nil, nil, fmt.Errorf("slo %q: objective must be between 0 and 100 exclusive, got %v", slo.Name, objective)
		}
		errorBudget := (100 - objective) / 100

		sloID := fmt.Sprintf("%s-%s", in.Service, slo.Name)
		// Prometheus label names must match [a-zA-Z_][a-zA-Z0-9_]*, so the
		// Kubernetes-style `lantern.dev/team` label used on objects cannot be
		// reused here — Prometheus rejects the whole rule file at load time.
		common := map[string]string{
			"lantern_service": in.Service,
			"lantern_slo":     slo.Name,
			"lantern_id":      sloID,
			"team":            in.Team,
		}
		for k := range common {
			if !validPromLabel(k) {
				return nil, nil, fmt.Errorf("internal: %q is not a valid Prometheus label name", k)
			}
		}

		groups.Add(sliGroup(sloID, sli, common))
		groups.Add(metadataGroup(sloID, objective, errorBudget, window, common))
		groups.Add(alertGroup(sloID, slo, in, objective, errorBudget, window, common, &diags))
	}

	ns := in.RuleNamespace
	if ns == "" {
		ns = in.Namespace
	}

	obj := kube.New(
		"monitoring.coreos.com/v1", "PrometheusRule",
		in.Service+"-slo", ns, in.Labels,
	).Set("spec", yamlx.NewMap("groups", groups))

	return obj, diags, nil
}

func sliGroup(sloID string, sli SLI, common map[string]string) *yamlx.Map {
	rules := yamlx.NewSeq()
	for _, w := range rateWindows {
		rules.Add(yamlx.NewMap(
			"record", yamlx.S("lantern:sli_error:ratio_rate"+w),
			"expr", yamlx.Lit(sli.Instantiate(w)),
			"labels", labelsWith(common, map[string]string{"lantern_window": w}),
		))
	}
	return yamlx.NewMap(
		"name", yamlx.S("lantern-sli-recordings-"+sloID),
		"rules", rules,
	)
}

func metadataGroup(sloID string, objective, errorBudget float64, window string, common map[string]string) *yamlx.Map {
	days, _ := ParseWindow(window)
	rules := yamlx.NewSeq(
		yamlx.NewMap(
			"record", yamlx.S("lantern:objective:ratio"),
			"expr", yamlx.Raw(trimFloat(objective/100)),
			"labels", labelsWith(common, nil),
		),
		yamlx.NewMap(
			"record", yamlx.S("lantern:error_budget:ratio"),
			"expr", yamlx.Raw(trimFloat(errorBudget)),
			"labels", labelsWith(common, nil),
		),
		yamlx.NewMap(
			"record", yamlx.S("lantern:time_period:days"),
			"expr", yamlx.Raw(trimFloat(days.Hours()/24)),
			"labels", labelsWith(common, nil),
		),
		yamlx.NewMap(
			"record", yamlx.S("lantern:current_burn_rate:ratio"),
			"expr", yamlx.Lit(fmt.Sprintf(
				"lantern:sli_error:ratio_rate5m{lantern_id=\"%s\"}\n/ on(lantern_id) group_left\nlantern:error_budget:ratio{lantern_id=\"%s\"}",
				sloID, sloID)),
			"labels", labelsWith(common, nil),
		),
		yamlx.NewMap(
			"record", yamlx.S("lantern:period_error_budget_remaining:ratio"),
			"expr", yamlx.Lit(fmt.Sprintf(
				"1 - (\n  lantern:sli_error:ratio_rate3d{lantern_id=\"%s\"}\n  / on(lantern_id) group_left\n  lantern:error_budget:ratio{lantern_id=\"%s\"}\n)",
				sloID, sloID)),
			"labels", labelsWith(common, nil),
		),
	)
	return yamlx.NewMap(
		"name", yamlx.S("lantern-slo-meta-recordings-"+sloID),
		"rules", rules,
	)
}

func alertGroup(
	sloID string,
	slo api.SLO,
	in RuleInput,
	objective, errorBudget float64,
	window string,
	common map[string]string,
	diags *[]Diagnostic,
) *yamlx.Map {
	rules := yamlx.NewSeq()

	// Group the ladder by severity so each severity produces one alert with an
	// OR of its window pairs, which is what makes the alert count manageable.
	bySeverity := map[string][]BurnWindow{}
	var order []string
	for _, bw := range DefaultBurnWindows {
		if _, seen := bySeverity[bw.Severity]; !seen {
			order = append(order, bw.Severity)
		}
		bySeverity[bw.Severity] = append(bySeverity[bw.Severity], bw)
	}

	for _, sev := range order {
		var clauses []string
		for _, bw := range bySeverity[sev] {
			threshold := trimFloat(bw.Factor * errorBudget)
			clauses = append(clauses, fmt.Sprintf(
				"(\n  lantern:sli_error:ratio_rate%s{lantern_id=\"%s\"} > (%s)\n  and\n  lantern:sli_error:ratio_rate%s{lantern_id=\"%s\"} > (%s)\n)",
				bw.Short, sloID, threshold, bw.Long, sloID, threshold,
			))
		}

		alertName := fmt.Sprintf("%sSLOBurn%s", camel(in.Service)+camel(slo.Name), camel(sev))
		summary := fmt.Sprintf("%s is burning its %s error budget too fast", in.Service, slo.Name)
		desc := fmt.Sprintf(
			"SLO %q (objective %s%% over %s) is consuming error budget at a rate that exhausts it before the window ends. Burn-rate windows: %s.",
			slo.Name, trimFloat(objective), window, windowSummary(bySeverity[sev]),
		)

		rules.Add(yamlx.NewMap(
			"alert", yamlx.S(alertName),
			"expr", yamlx.Lit(strings.Join(clauses, "\nor\n")),
			"labels", labelsWith(common, map[string]string{
				"severity": sev,
				"team":     in.Team,
			}),
			"annotations", yamlx.NewMap(
				"summary", yamlx.S(summary),
				"description", yamlx.S(desc),
				"runbook_url", yamlx.S(fmt.Sprintf("https://runbooks.internal/slo/%s", sloID)),
			),
		))
	}

	if in.Team == "" {
		*diags = append(*diags, Diagnostic{
			Level:   "warn",
			Message: "no team set: generated alerts carry an empty team label and Alertmanager will not be able to route them",
		})
	}

	return yamlx.NewMap(
		"name", yamlx.S("lantern-slo-alerts-"+sloID),
		"rules", rules,
	)
}

func windowSummary(bws []BurnWindow) string {
	var parts []string
	for _, bw := range bws {
		parts = append(parts, fmt.Sprintf("%s/%s at %sx", bw.Short, bw.Long, trimFloat(bw.Factor)))
	}
	return strings.Join(parts, ", ")
}

func labelsWith(base, extra map[string]string) *yamlx.Map {
	merged := map[string]string{}
	for k, v := range base {
		if v != "" {
			merged[k] = v
		}
	}
	for k, v := range extra {
		if v != "" {
			merged[k] = v
		}
	}
	return kube.SortedStringMap(merged)
}

// trimFloat renders a float without binary-representation noise.
//
// 14.4 * 0.001 is 0.014400000000000003 in IEEE-754. Emitting that into a
// PromQL threshold is technically correct and completely unreadable, and it
// makes golden diffs impossible to eyeball. Twelve significant digits is far
// more precision than any burn-rate threshold needs.
func trimFloat(f float64) string {
	s := strconv.FormatFloat(f, 'g', 12, 64)
	// 'g' may produce exponent form for small values; PromQL accepts it, but
	// decimal form reads better at the magnitudes we generate.
	if v, err := strconv.ParseFloat(s, 64); err == nil {
		if d := strconv.FormatFloat(v, 'f', -1, 64); len(d) <= 12 {
			return d
		}
	}
	return s
}

var promLabelRe = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

func validPromLabel(s string) bool { return promLabelRe.MatchString(s) }

func camel(s string) string {
	var b strings.Builder
	up := true
	for _, r := range s {
		if r == '-' || r == '_' || r == '.' || r == ' ' || r == '/' {
			up = true
			continue
		}
		if up {
			b.WriteString(strings.ToUpper(string(r)))
			up = false
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
