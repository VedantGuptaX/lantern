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
		return metricFamily{}, fmt.Errorf(
			"serviceKind %q has no built-in SLI template; use type: custom with errorQuery and totalQuery, "+
				"type: latency with an explicit metric (a duration histogram), or type: saturation (a gauge)", kind)
	}
}

// latencyFamily resolves the counter/bucket pair a type: latency SLO builds
// against. An explicit slo.Metric always wins over the serviceKind's
// built-in family: this is what lets serviceKind: inference (or any kind)
// build a latency SLO against a vendor-specific histogram — time-to-first-token,
// inter-token latency, or anything else — without Lantern hardcoding one
// inference server's naming convention. It is always marked experimental:
// Lantern has no semantic-convention guarantee about a metric it didn't name,
// and cannot verify the histogram's bucket boundaries actually include the
// threshold given.
func latencyFamily(slo api.SLO, kind api.ServiceKind) (metricFamily, error) {
	if strings.TrimSpace(slo.Metric) != "" {
		return metricFamily{
			counter:      slo.Metric + "_count",
			bucket:       slo.Metric + "_bucket",
			experimental: true,
		}, nil
	}
	return familyFor(kind)
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
		f, err := latencyFamily(slo, kind)
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

	case api.SLOSaturation:
		if strings.TrimSpace(slo.Metric) == "" {
			return SLI{}, fmt.Errorf(
				"slo %q: type saturation requires metric (a gauge, e.g. vllm:num_requests_waiting or DCGM_FI_DEV_GPU_UTIL)", slo.Name)
		}
		if strings.TrimSpace(slo.Threshold) == "" {
			return SLI{}, fmt.Errorf(
				"slo %q: type saturation requires a threshold (a plain number the gauge must stay under, e.g. \"10\" — not a duration)", slo.Name)
		}
		n, err := strconv.ParseFloat(slo.Threshold, 64)
		if err != nil {
			return SLI{}, fmt.Errorf("slo %q: threshold %q must be a plain number for type saturation: %w", slo.Name, slo.Threshold, err)
		}
		// sum_over_time((gauge > bool N)[window:]) counts the samples where the
		// gauge breached N: `> bool N` yields 1 at each breaching sample and 0
		// otherwise, so summing gives the breach count. (count_over_time here
		// would be wrong -- it counts every sample, 0s included, making the
		// ratio identically 1 and the SLO permanently "100% bad"; confirmed
		// against a live Prometheus.) Dividing by the total sample count over
		// the same window gives the same bad/total ratio shape every other SLO
		// type produces, so it gets the same burn-rate alerts for free.
		bad := fmt.Sprintf("sum_over_time((%s{%s} > bool %s)[{{.window}}:])", slo.Metric, selector, trimFloat(n))
		total := fmt.Sprintf("count_over_time(%s{%s}[{{.window}}:])", slo.Metric, selector)
		return SLI{Error: bad, Total: total, Experimental: true}, nil

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
