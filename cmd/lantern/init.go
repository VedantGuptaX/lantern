package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// initAnswers is the resolved set of choices from `lantern init`'s prompts.
// Each field maps directly onto one or more Helm subchart toggles -- see
// renderValues.
type initAnswers struct {
	Metrics    bool // kube-prometheus-stack (Prometheus, Alertmanager, Grafana, node-exporter)
	Logs       bool // loki + logsCollector
	Traces     bool // tempo
	SDKAgent   bool // opentelemetry-operator; only asked/meaningful if Traces
	GPU        bool // gpuMonitoring wanted at all; only asked if Metrics
	DCGMExists bool // dcgm-exporter already running; only asked if GPU
	GPUReady   bool // GPU nodes already schedule real GPU workloads (driver+device plugin proven); only asked if GPU && !DCGMExists
}

// installExporter reports whether these answers want Lantern to install
// dcgm-exporter itself (the gpuMonitoring.installExporter / dcgm-exporter.enabled
// pair in values.yaml), rather than assuming a bring-your-own install.
//
// This is deliberately narrow: it's only true when the user said there's no
// dcgm-exporter yet AND that the GPU nodes already run real GPU workloads
// (driver + device plugin proven working). `lantern init` never touches a
// live cluster (see CLAUDE.md), so it cannot verify that claim itself --
// `scripts/preflight-check.sh --gpu-install-exporter` is the actual gate,
// against the real cluster, before anyone runs `helm install`.
func (a initAnswers) installExporter() bool {
	return a.GPU && !a.DCGMExists && a.GPUReady
}

// gpuMonitoringWanted reports whether gpuMonitoring.enabled should end up
// true. A "yes" to the top-level GPU question isn't enough by itself: with
// no dcgm-exporter running and no GPU nodes ready to host one, there is
// nothing for the ServiceMonitor/PrometheusRule this turns on to scrape or
// evaluate -- same reasoning as the existing "metrics off skips GPU
// entirely" rule below, just discovered one layer deeper.
func (a initAnswers) gpuMonitoringWanted() bool {
	return a.GPU && (a.DCGMExists || a.GPUReady)
}

// grafanaOnly reports whether the answers want a Grafana UI without the rest
// of kube-prometheus-stack (Prometheus, Alertmanager, kube-state-metrics).
//
// BUG THIS PREVENTS, found by actually rendering "logs=yes, metrics=no":
// Grafana ships ONLY inside the kube-prometheus-stack subchart in this
// chart -- answering "no" to the metrics question (worded "Prometheus +
// Grafana", which is exactly the trap) disabled that whole subchart, which
// means it also silently threw away the one place you'd ever look at the
// logs/traces you just asked for. The install "succeeds" and produces a
// fully working Loki/Tempo pipeline with no UI attached to it at all.
//
// kube-prometheus-stack's own values expose prometheus.enabled,
// alertmanager.enabled and kubeStateMetrics.enabled as toggles independent
// of grafana.enabled -- a real, deliberate feature of that subchart, not a
// workaround -- so "Grafana only" is achievable without dragging in
// footprint nobody asked for.
func (a initAnswers) grafanaOnly() bool {
	return !a.Metrics && (a.Logs || a.Traces)
}

// grafanaNeeded reports whether kube-prometheus-stack must be enabled at
// all -- for the full bundle, or for grafanaOnly's trimmed-down version of
// it.
func (a initAnswers) grafanaNeeded() bool {
	return a.Metrics || a.grafanaOnly()
}

// collectorNeeded reports whether the OTel Collector subchart is needed at
// all. It's the thing every other signal actually routes through: logs and
// traces both get pushed to it via OTLP, and so does SDK/agent
// instrumentation's own export path. Plain Prometheus scrape-based metrics
// never touch it.
func (a initAnswers) collectorNeeded() bool {
	return a.Logs || a.Traces || a.SDKAgent
}

// operatorNeeded reports whether the OpenTelemetry Operator subchart must be
// installed.
//
// It is NOT just "did the user ask for SDK/agent injection". Both
// templates/collector.yaml and templates/logs-collector.yaml emit
// OpenTelemetryCollector *custom resources*, which need the operator twice
// over: it owns the CRD those resources are validated against, and it's the
// controller that reconciles them into a real Deployment/DaemonSet. Nothing
// runs without it.
//
// Getting this wrong fails silently, which is why it's worth its own
// function and its own tests: both templates gate their CR on
// lantern.otelCollectorCRDReady (`.Capabilities.APIVersions.Has ...`), so on
// a cluster with no operator the CRD doesn't exist, the guard is false, and
// the CRs are simply skipped -- no error at install time, no warning. The
// user gets Loki and/or Tempo running with nothing shipping to them and
// dashboards that stay empty forever. Found by rendering every init
// combination and checking which objects actually came out, not by reading
// the templates.
func (a initAnswers) operatorNeeded() bool {
	return a.collectorNeeded()
}

// promptYesNo asks a yes/no question with a default, reading from r one line
// at a time and writing the prompt (and any re-prompt on bad input) to w.
// Empty input accepts the default -- this is what lets a user hammer enter
// through the whole flow and get the "everything" quickstart back.
func promptYesNo(r *bufio.Reader, w io.Writer, question string, def bool) (bool, error) {
	suffix := "[Y/n]"
	if !def {
		suffix = "[y/N]"
	}
	for {
		fmt.Fprintf(w, "%s %s ", question, suffix)
		line, err := r.ReadString('\n')
		if err != nil && line == "" {
			return false, err
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "":
			return def, nil
		case "y", "yes":
			return true, nil
		case "n", "no":
			return false, nil
		}
		fmt.Fprintln(w, "please answer y or n")
	}
}

// runInitPrompts asks the questions and returns the answers. Separated from
// terminal I/O so it's testable with a canned reader/writer, matching this
// package's existing style (reorderArgs/render are pure, main just wires
// them to os.Args/os.Stdin).
func runInitPrompts(r io.Reader, w io.Writer) (initAnswers, error) {
	br := bufio.NewReader(r)
	var a initAnswers
	var err error

	fmt.Fprintln(w, "lantern init — a few questions, then I'll write a values file with only what you asked for.")
	fmt.Fprintln(w, "Press enter to accept the default (capital letter) at any prompt.")
	fmt.Fprintln(w)

	if a.Metrics, err = promptYesNo(br, w, "Metrics (Prometheus + Grafana)?", true); err != nil {
		return a, err
	}
	if a.Logs, err = promptYesNo(br, w, "Logs (Loki)?", true); err != nil {
		return a, err
	}
	if a.Traces, err = promptYesNo(br, w, "Traces?", true); err != nil {
		return a, err
	}
	if a.Traces {
		if a.SDKAgent, err = promptYesNo(br, w,
			"  Real SDK/agent instrumentation too? (deeper spans -- DB queries, route\n"+
				"  detail -- but restarts each instrumented service's pods, one at a time,\n"+
				"  and needs a known runtime. Answering no keeps eBPF only: zero per-service\n"+
				"  config, no pod restarts, shallower spans.)",
			false); err != nil {
			return a, err
		}
	}
	// Only asked when Metrics is on, matching the SDK-agent sub-question's
	// pattern above.
	//
	// BUG THIS PREVENTS, found by rendering "metrics=no, gpu=yes": GPU
	// monitoring is templates/gpu-monitoring.yaml emitting a bare
	// ServiceMonitor + PrometheusRule, unconditionally -- there's no Grafana-
	// only-style trimmed path for it the way logs/traces got, because those
	// CRDs are owned by kube-prometheus-stack's own nested "crds" subchart,
	// which Helm skips entirely whenever the parent's enabled condition is
	// false. On a genuinely fresh cluster with no pre-existing Prometheus
	// Operator CRDs, that combination fails outright at apply time ("no
	// matches for kind ServiceMonitor"). Unlike Grafana, there's no
	// standalone value to preserve here either -- a ServiceMonitor/
	// PrometheusRule pair is meaningless without a real Prometheus to scrape
	// and evaluate it, so this isn't a case for a trimmed sub-toggle path;
	// the question just shouldn't be asked.
	if a.Metrics {
		if a.GPU, err = promptYesNo(br, w, "GPU node monitoring (DCGM)? Skip if you have no GPU nodes.", false); err != nil {
			return a, err
		}
		if a.GPU {
			if a.DCGMExists, err = promptYesNo(br, w,
				"  Is dcgm-exporter already running on those GPU nodes (via the NVIDIA\n"+
					"  GPU Operator or its own install)?", true); err != nil {
				return a, err
			}
			if !a.DCGMExists {
				if a.GPUReady, err = promptYesNo(br, w,
					"  Do those GPU nodes already run real GPU workloads successfully --\n"+
						"  i.e. something has already been scheduled against the nvidia.com/gpu\n"+
						"  resource, so the driver and device plugin are proven working, just no\n"+
						"  dcgm-exporter yet? If so I can install ONLY dcgm-exporter (not the\n"+
						"  full GPU Operator, no driver changes). Answering no leaves GPU\n"+
						"  monitoring off entirely -- there'd be nothing safe to install and\n"+
						"  nothing running to point a ServiceMonitor at.",
					false); err != nil {
					return a, err
				}
			}
		}
	}

	return a, nil
}

// traceLokiDataSources returns the Grafana additionalDataSources entries
// (pre-rendered as YAML list-item text) for whichever of Tempo/Loki are
// actually enabled, in the exact shape values-quickstart.yaml itself uses --
// same UIDs (tempo-main, loki-main) so anything else in the chart that
// references those UIDs (per-service dashboards, the Investigate-style
// dashboards) keeps working unmodified.
func traceLokiDataSources(a initAnswers) []string {
	var out []string
	if a.Traces {
		out = append(out, "      - name: Tempo\n        type: tempo\n        uid: tempo-main\n"+
			"        url: http://lantern-tempo.observability:3200\n        access: proxy\n")
	}
	if a.Logs {
		out = append(out, "      - name: Loki\n        type: loki\n        uid: loki-main\n"+
			"        url: http://lantern-loki.observability:3100\n        access: proxy\n")
	}
	return out
}

// renderValues writes the Helm values overlay for the given answers. It's
// deliberately small: just the enable/disable toggles, meant to be layered
// on top of values-quickstart.yaml (which already carries the resource
// sizing and install-ordering fixes found running this chart against a real
// cluster) rather than duplicating that tuning here.
func renderValues(a initAnswers) string {
	var b strings.Builder
	b.WriteString("# Generated by `lantern init`. Layer this on top of values-quickstart.yaml --\n")
	b.WriteString("# it only carries the enable/disable decisions from your answers, not the\n")
	b.WriteString("# resource sizing or install-ordering fixes already in that file.\n#\n")
	b.WriteString("#   helm install lantern charts/lantern-stack \\\n")
	b.WriteString("#     -f charts/lantern-stack/values-quickstart.yaml \\\n")
	b.WriteString("#     -f <this-file>\n\n")

	fmt.Fprintf(&b, "kube-prometheus-stack:\n  enabled: %v\n", a.grafanaNeeded())
	if a.grafanaNeeded() {
		if a.grafanaOnly() {
			b.WriteString("  # Metrics answered \"no\", but Grafana is the only place this chart lets\n")
			b.WriteString("  # you look at logs/traces, so it stays on -- trimmed to just Grafana,\n")
			b.WriteString("  # without Prometheus/Alertmanager/kube-state-metrics.\n")
			b.WriteString("  prometheus:\n    enabled: false\n")
			b.WriteString("  alertmanager:\n    enabled: false\n")
			b.WriteString("  kubeStateMetrics:\n    enabled: false\n")
			// prometheusOperator deliberately stays on (the chart's own
			// default). It's what owns the ServiceMonitor/PrometheusRule
			// CRDs, and `lantern synth` generates those objects for every
			// service regardless of this answer -- that's a compiler-level
			// decision this file doesn't control. Turning the operator off
			// too would save ~3m CPU / 27Mi and break `kubectl apply` on the
			// very next `lantern synth` output with a missing-CRD error, a
			// strictly worse trade.
		}
		// Everything Grafana-related has to live under ONE "grafana:" key --
		// a values file with that key twice is ambiguous YAML and an earlier
		// draft of this function did exactly that (sidecar overrides in one
		// block, additionalDataSources in a second "grafana:" block right
		// after it), which would have silently dropped one or the other
		// depending on the parser. Built as one block instead.
		b.WriteString("  grafana:\n")
		if a.grafanaOnly() {
			// kube-prometheus-stack's own Grafana datasource ConfigMap adds a
			// default "Prometheus" datasource gated only on grafana.enabled,
			// not on its own prometheus.enabled -- so without this, Grafana
			// would carry a datasource pointed at a Prometheus this
			// combination never installs. Same class of bug as the Tempo/Loki
			// one below, this time inherited from the subchart rather than
			// introduced by values-quickstart.yaml.
			b.WriteString("    sidecar:\n      datasources:\n        defaultDatasourceEnabled: false\n")
		}
		// values-quickstart.yaml wires Tempo and Loki into Grafana as fixed
		// additionalDataSources regardless of whether those subcharts are
		// enabled -- pointing Grafana at services that don't exist if you
		// turned traces/logs off. Helm replaces (doesn't merge) list values
		// across -f files, so this override corrects it to match what's
		// actually installed. Found by actually rendering a metrics-only
		// combination and grepping the output for "tempo"/"loki", not
		// assumed.
		datasources := traceLokiDataSources(a)
		if len(datasources) == 0 {
			b.WriteString("    additionalDataSources: []\n")
		} else {
			b.WriteString("    additionalDataSources:\n")
			for _, ds := range datasources {
				b.WriteString(ds)
			}
		}
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "loki:\n  enabled: %v\n\n", a.Logs)
	fmt.Fprintf(&b, "logsCollector:\n  enabled: %v\n\n", a.Logs)
	fmt.Fprintf(&b, "tempo:\n  enabled: %v\n\n", a.Traces)
	fmt.Fprintf(&b, "collector:\n  enabled: %v\n\n", a.collectorNeeded())
	fmt.Fprintf(&b, "opentelemetry-operator:\n  enabled: %v\n\n", a.operatorNeeded())
	fmt.Fprintf(&b, "gpuMonitoring:\n  enabled: %v\n  installExporter: %v\n", a.gpuMonitoringWanted(), a.installExporter())
	fmt.Fprintf(&b, "dcgm-exporter:\n  enabled: %v\n", a.installExporter())

	if a.Traces && !a.SDKAgent {
		b.WriteString("\n# Traces without SDK/agent instrumentation means eBPF (OBI). This chart\n")
		b.WriteString("# doesn't deploy OBI itself (a standalone Helm release, bring-your-own) --\n")
		b.WriteString("# see docs/getting-signals-into-grafana.md for a verified install command.\n")
	}
	if a.gpuMonitoringWanted() && !a.installExporter() {
		b.WriteString("\n# gpuMonitoring.enabled only adds a ServiceMonitor + PrometheusRule --\n")
		b.WriteString("# dcgm-exporter itself is bring-your-own (the NVIDIA GPU Operator's job, not\n")
		b.WriteString("# this chart's). See README's GPU section before turning this on.\n")
	}
	if a.installExporter() {
		b.WriteString("\n# dcgm-exporter.enabled installs ONLY the exporter, not the GPU Operator --\n")
		b.WriteString("# no driver, no container toolkit. Before running `helm install`:\n")
		b.WriteString("#   1. Run `./scripts/preflight-check.sh --gpu-install-exporter` against the\n")
		b.WriteString("#      real cluster -- it BLOCKs unless a node already advertises nvidia.com/gpu\n")
		b.WriteString("#      as allocatable, and prints candidate labels for the next step.\n")
		b.WriteString("#   2. Set dcgm-exporter.nodeSelector in values.yaml to one of those labels.\n")
		b.WriteString("#      It's empty by default; left empty, the DaemonSet schedules onto every\n")
		b.WriteString("#      node, not just GPU ones, and CrashLoopBackOffs everywhere else.\n")
	}
	if a.GPU && !a.DCGMExists && !a.GPUReady {
		b.WriteString("\n# You said you have GPU nodes, no dcgm-exporter running yet, and those nodes\n")
		b.WriteString("# aren't yet running real GPU workloads -- so there's nothing safe to install\n")
		b.WriteString("# or point a ServiceMonitor at. GPU monitoring is left off (gpuMonitoring.\n")
		b.WriteString("# enabled: false above). Get the NVIDIA driver + device plugin working first\n")
		b.WriteString("# (NVIDIA GPU Operator is the standard path), then re-run `lantern init`.\n")
	}

	return b.String()
}

// resourceEstimate renders the same real, measured-on-a-cluster numbers from
// README's "Resource requirements" table, filtered to what these answers
// actually turn on -- so the whole point of asking ("this saves real
// compute") is visible immediately, not just implied.
func resourceEstimate(a initAnswers) string {
	type row struct {
		name       string
		cpu, mem   int // milli-CPU, Mi -- single-scheduled
		perNodeCPU int // milli-CPU -- 0 if not a DaemonSet
		perNodeMem int
	}
	var rows []row
	if a.Metrics {
		rows = append(rows, row{"kube-prometheus-stack (Prometheus, Alertmanager, Grafana, kube-state-metrics)", 245, 592, 0, 0})
		rows = append(rows, row{"node-exporter", 0, 0, 10, 24})
	} else if a.grafanaOnly() {
		// Measured on a real cluster via `kubectl top pod` against the
		// Grafana container alone: 10m CPU / 320Mi -- the rest of
		// kube-prometheus-stack's usual footprint (Prometheus, Alertmanager,
		// kube-state-metrics) is what this combination trims away.
		rows = append(rows, row{"Grafana only (Prometheus/Alertmanager/kube-state-metrics off)", 10, 320, 0, 0})
	}
	if a.operatorNeeded() {
		rows = append(rows, row{"OTel Operator", 30, 64, 0, 0})
	}
	if a.collectorNeeded() {
		rows = append(rows, row{"OTel Collector", 40, 256, 0, 0})
	}
	if a.Logs {
		rows = append(rows, row{"Loki (SingleBinary)", 10, 128, 0, 0})
		rows = append(rows, row{"logsCollector (pod-log shipping)", 0, 0, 50, 256})
		rows = append(rows, row{"loki-canary", 0, 0, 10, 24})
	}
	if a.Traces {
		rows = append(rows, row{"Tempo", 10, 128, 0, 0})
		if !a.SDKAgent {
			rows = append(rows, row{"OBI (eBPF probe, bring-your-own)", 0, 0, 10, 256})
		}
	}
	if a.installExporter() {
		// Low end of the request range NVIDIA publishes (README's GPU
		// prerequisites table) -- unlike the rows above, not measured
		// against a real cluster this session, since this bastion's cluster
		// has no GPU nodes to measure against.
		rows = append(rows, row{"dcgm-exporter (installExporter -- NVIDIA's published low end, not measured this session)", 0, 0, 10, 128})
	}

	var totalCPU, totalMem, perNodeCPU, perNodeMem int
	var b strings.Builder
	fmt.Fprintln(&b, "\nEstimated footprint (single-scheduled + per-node, from this session's")
	fmt.Fprintln(&b, "own measurements against a real cluster -- see README's resource table):")
	for _, r := range rows {
		if r.cpu > 0 || r.mem > 0 {
			fmt.Fprintf(&b, "  %-70s %4dm  %5dMi\n", r.name, r.cpu, r.mem)
			totalCPU += r.cpu
			totalMem += r.mem
		} else {
			fmt.Fprintf(&b, "  %-70s %4dm  %5dMi  (per node)\n", r.name, r.perNodeCPU, r.perNodeMem)
			perNodeCPU += r.perNodeCPU
			perNodeMem += r.perNodeMem
		}
	}
	fmt.Fprintf(&b, "\n  Total: ~%dm CPU / ~%dMi RAM, plus ~%dm CPU / ~%dMi RAM per node.\n", totalCPU, totalMem, perNodeCPU, perNodeMem)
	return b.String()
}

func runInit(outPath string) error {
	if outPath == "" {
		outPath = "values-init.yaml"
	}
	// Both of these checks happen BEFORE the first prompt, deliberately.
	// Failing after the questions throws away answers the user just typed --
	// found by running `lantern init -o /nonexistent-dir/v.yaml`, which asked
	// all five questions and only then reported it couldn't write the file.
	if _, err := os.Stat(outPath); err == nil {
		return fmt.Errorf("%s already exists; pass -o to write somewhere else", outPath)
	}
	if dir := filepath.Dir(outPath); dir != "" {
		info, err := os.Stat(dir)
		if err != nil {
			return fmt.Errorf("cannot write %s: %w", outPath, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("cannot write %s: %s is not a directory", outPath, dir)
		}
	}

	answers, err := runInitPrompts(os.Stdin, os.Stdout)
	if err != nil {
		return fmt.Errorf("reading answers: %w", err)
	}

	if err := os.WriteFile(outPath, []byte(renderValues(answers)), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", outPath, err)
	}

	fmt.Println(resourceEstimate(answers))
	fmt.Printf("\nWrote %s. Next:\n\n", outPath)
	fmt.Printf("  helm install lantern charts/lantern-stack \\\n")
	fmt.Printf("    -f charts/lantern-stack/values-quickstart.yaml \\\n")
	fmt.Printf("    -f %s\n", outPath)
	return nil
}
