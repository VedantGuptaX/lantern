package compile

import api "github.com/VedantGuptaX/lantern/api/v1alpha1"

// Built-in SLO presets for serviceKind: inference.
//
// serviceKind: inference deliberately has no semantic-convention metric family
// (see pkg/emit/prom/sli.go) because every model server names its metrics
// differently. The preset closes that gap for the servers Lantern knows about:
// given spec.inferenceServer, it supplies the standard SLOs against that
// server's real metric names, so a user gets burn-rate alerts and a populated
// dashboard without hand-writing a single metric name.
//
// What is and isn't a guess: the metric NAMES are curated from each server's
// documented Prometheus output (kept in sync with
// docs/gpu-and-inference-observability.md's cheat sheet). The SLO thresholds
// and objectives are defaults — Lantern cannot verify a histogram's bucket
// boundaries, so the compiler flags every preset application for review rather
// than presenting the thresholds as a commitment.

// inferencePreset is the SLOs for one server plus any server-specific caveat.
type inferencePreset struct {
	slos []api.SLO
	// note is an extra caveat surfaced as a warn diagnostic when the preset is
	// applied (empty = none). Used where a server's metric availability or
	// naming is version- or backend-dependent.
	note string
}

// validInferenceServer reports whether s is a server the preset table knows.
func validInferenceServer(s api.InferenceServer) bool {
	switch s {
	case api.InferenceVLLM, api.InferenceTriton, api.InferenceNIM, api.InferenceTGI:
		return true
	}
	return false
}

func latencySLO(name, metric, threshold string) api.SLO {
	return api.SLO{Name: name, Type: api.SLOLatency, Objective: 99.0, Threshold: threshold, Window: "7d", Metric: metric}
}

func saturationSLO(name, metric, threshold string) api.SLO {
	return api.SLO{Name: name, Type: api.SLOSaturation, Objective: 99.0, Threshold: threshold, Window: "7d", Metric: metric}
}

// inferencePresetFor returns the built-in preset for a server, and whether one
// exists. An unknown or empty server returns ok=false, leaving the caller on
// the normal (no built-in SLOs) inference path.
func inferencePresetFor(server api.InferenceServer) (inferencePreset, bool) {
	switch server {
	case api.InferenceVLLM:
		return inferencePreset{slos: []api.SLO{
			latencySLO("ttft", "vllm:time_to_first_token_seconds", "500ms"),
			latencySLO("inter-token-latency", "vllm:time_per_output_token_seconds", "50ms"),
			saturationSLO("queue-depth", "vllm:num_requests_waiting", "10"),
		}}, true

	case api.InferenceNIM:
		// NIM exposes vLLM-compatible metrics when backed by vLLM. When backed
		// by TensorRT-LLM the names differ, hence the caveat.
		return inferencePreset{slos: []api.SLO{
			latencySLO("ttft", "vllm:time_to_first_token_seconds", "500ms"),
			latencySLO("inter-token-latency", "vllm:time_per_output_token_seconds", "50ms"),
			saturationSLO("queue-depth", "vllm:num_requests_waiting", "10"),
		}, note: "NIM wraps vLLM or TensorRT-LLM depending on configuration; this preset assumes the vLLM-compatible metric names — verify against your NIM deployment's /metrics if it uses the TensorRT-LLM backend"}, true

	case api.InferenceTriton:
		// Triton has no built-in notion of tokens and exports per-request
		// duration only in microseconds, which would not line up with a
		// seconds-based latency threshold — so the preset ships queue-depth
		// only and leaves token-latency SLOs to the user.
		return inferencePreset{slos: []api.SLO{
			saturationSLO("queue-depth", "nv_inference_pending_request_count", "10"),
		}, note: "Triton exposes no time-to-first-token or inter-token latency by default (only per-request duration, in microseconds), so the preset generates queue-depth only; add type: latency SLOs by hand if your backend exports token metrics"}, true

	case api.InferenceTGI:
		return inferencePreset{slos: []api.SLO{
			latencySLO("inter-token-latency", "tgi_request_mean_time_per_token_duration", "50ms"),
			saturationSLO("queue-depth", "tgi_queue_size", "10"),
		}, note: "TGI metric names vary across versions; verify tgi_request_mean_time_per_token_duration and tgi_queue_size against your deployment's /metrics before trusting these SLOs"}, true
	}
	return inferencePreset{}, false
}
