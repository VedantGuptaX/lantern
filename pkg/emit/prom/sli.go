package prom

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	api "github.com/VedantGuptaX/lantern/api/v1alpha1"
)

// SLI holds the numerator/denominator of an error-ratio SLI. Every SLO type
// reduces to `bad events / total events`, which is what makes burn-rate
// alerting mechanical.
type SLI struct {
	Error string
	Total string
	// Experimental marks SLIs built on semantic conventions that upstream has
	// not stabilised, so the compiler can warn instead of silently emitting
	// queries against metrics that may be renamed.
	Experimental bool
}

// metricFamily maps a service kind to its semantic-convention metric base name
// after OTel -> Prometheus translation (dots to underscores, unit suffix
// appended).
type metricFamily struct {
	counter      string // ..._count
	bucket       string // ..._bucket
	errorMatcher string
	experimental bool
}

func familyFor(kind api.ServiceKind) (metricFamily, error) {
	switch kind {
	case api.KindHTTP:
		// http.server.request.duration is STABLE in OTel semconv.
		return metricFamily{
			counter:      "http_server_request_duration_seconds_count",
			bucket:       "http_server_request_duration_seconds_bucket",
			errorMatcher: `http_response_status_code=~"5.."`,
		}, nil
	case api.KindGRPC:
		return metricFamily{
			counter:      "rpc_server_duration_seconds_count",
			bucket:       "rpc_server_duration_seconds_bucket",
			errorMatcher: `rpc_grpc_status_code!="0"`,
			experimental: true,
		}, nil
	case api.KindWorker:
		return metricFamily{
			counter:      "messaging_process_duration_seconds_count",
			bucket:       "messaging_process_duration_seconds_bucket",
			errorMatcher: `error_type!=""`,
			experimental: true,
		}, nil
	case api.KindDatabase:
		return metricFamily{
			counter:      "db_client_operation_duration_seconds_count",
			bucket:       "db_client_operation_duration_seconds_bucket",
			errorMatcher: `error_type!=""`,
			experimental: true,
		}, nil
	default:
		return metricFamily{}, fmt.Errorf("serviceKind %q has no built-in SLI template; use type: custom with errorQuery and totalQuery", kind)
	}
}

// BuildSLI renders the error-ratio expressions for one SLO.
//
// `{{.window}}` is left as a placeholder so a single SLI definition can be
// instantiated at every burn-rate window without re-deriving the query.
func BuildSLI(slo api.SLO, kind api.ServiceKind, selector string) (SLI, error) {
	switch slo.Type {
	case api.SLOCustom:
		if strings.TrimSpace(slo.ErrorQuery) == "" || strings.TrimSpace(slo.TotalQuery) == "" {
			return SLI{}, fmt.Errorf("slo %q: type custom requires both errorQuery and totalQuery", slo.Name)
		}
		return SLI{Error: slo.ErrorQuery, Total: slo.TotalQuery}, nil

	case api.SLOAvailability:
		f, err := familyFor(kind)
		if err != nil {
			return SLI{}, err
		}
		errSel := joinSelector(selector, f.errorMatcher)
		return SLI{
			Error:        fmt.Sprintf("sum(rate(%s{%s}[{{.window}}]))", f.counter, errSel),
			Total:        fmt.Sprintf("sum(rate(%s{%s}[{{.window}}]))", f.counter, selector),
			Experimental: f.experimental,
		}, nil

	case api.SLOLatency:
		f, err := familyFor(kind)
		if err != nil {
			return SLI{}, err
		}
		if slo.Threshold == "" {
			return SLI{}, fmt.Errorf("slo %q: type latency requires a threshold (e.g. 300ms)", slo.Name)
		}
		le, err := bucketBound(slo.Threshold)
		if err != nil {
			return SLI{}, fmt.Errorf("slo %q: %w", slo.Name, err)
		}
		total := fmt.Sprintf("sum(rate(%s{%s}[{{.window}}]))", f.counter, selector)
		fast := fmt.Sprintf("sum(rate(%s{%s}[{{.window}}]))", f.bucket, joinSelector(selector, fmt.Sprintf(`le="%s"`, le)))
		// Requests slower than the threshold are the "bad" events.
		return SLI{
			Error:        fmt.Sprintf("(\n  %s\n  -\n  %s\n)", total, fast),
			Total:        total,
			Experimental: f.experimental,
		}, nil

	default:
		return SLI{}, fmt.Errorf("slo %q: unknown type %q", slo.Name, slo.Type)
	}
}

// Instantiate substitutes a concrete rate window into an SLI expression.
func (s SLI) Instantiate(window string) string {
	ratio := fmt.Sprintf("%s\n/\n%s", s.Error, s.Total)
	return strings.ReplaceAll(ratio, "{{.window}}", window)
}

func joinSelector(base, extra string) string {
	if base == "" {
		return extra
	}
	if extra == "" {
		return base
	}
	return base + ", " + extra
}

// bucketBound converts a latency threshold to a Prometheus `le` bucket bound
// in seconds, matching how OTel histograms are exported.
func bucketBound(threshold string) (string, error) {
	d, err := time.ParseDuration(threshold)
	if err != nil {
		return "", fmt.Errorf("invalid threshold %q: %w", threshold, err)
	}
	if d <= 0 {
		return "", fmt.Errorf("threshold %q must be positive", threshold)
	}
	secs := d.Seconds()
	// Prometheus renders bucket bounds via strconv float formatting; match it
	// exactly or the selector silently matches nothing.
	return strconv.FormatFloat(secs, 'g', -1, 64), nil
}

// ParseWindow accepts Prometheus-style durations including `d`, which Go's
// time.ParseDuration rejects.
func ParseWindow(w string) (time.Duration, error) {
	if w == "" {
		return 30 * 24 * time.Hour, nil
	}
	if strings.HasSuffix(w, "d") {
		n, err := strconv.Atoi(strings.TrimSuffix(w, "d"))
		if err != nil {
			return 0, fmt.Errorf("invalid window %q", w)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	if strings.HasSuffix(w, "w") {
		n, err := strconv.Atoi(strings.TrimSuffix(w, "w"))
		if err != nil {
			return 0, fmt.Errorf("invalid window %q", w)
		}
		return time.Duration(n) * 7 * 24 * time.Hour, nil
	}
	return time.ParseDuration(w)
}

// HasSLITemplate reports whether a service kind has a built-in SLI template.
//
// The compiler uses this to decide whether stack-level default SLOs can be
// applied. An explicit SLO on an unsupported kind is still an error — the user
// asked for it — but a default silently inherited from the stack must not fail
// the build.
func HasSLITemplate(kind api.ServiceKind) bool {
	_, err := familyFor(kind)
	return err == nil
}
