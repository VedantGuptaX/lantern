.PHONY: all build test fmt vet lint golden synth clean

BIN := bin/lantern

all: fmt vet test build

build:
	@mkdir -p bin
	go build -o $(BIN) ./cmd/lantern

test:
	go test ./...

# Re-baseline the golden files. Review the diff before committing: a golden
# change is either an intended behaviour change or an upstream schema break.
golden:
	go test ./pkg/compile -run TestGolden -update
	@git diff --stat -- testdata/golden 2>/dev/null || true

fmt:
	gofmt -w .

vet:
	go vet ./...

# The purity boundary is a test, not a separate linter, so it runs in CI for
# free alongside everything else.
lint: vet
	go test ./pkg/compile -run TestCompilerPurityBoundary -v

synth: build
	@$(BIN) synth -stack examples/stack.yaml examples/checkout-api.yaml

clean:
	rm -rf bin

# End-to-end: draft specs from the example cluster, then compile them.
demo: build
	@$(BIN) discover -quiet -team unassigned examples/cluster/workloads.yaml > /tmp/lantern-services.yaml
	@$(BIN) synth -stack examples/stack.yaml /tmp/lantern-services.yaml

chart-check:
	@helm lint charts/lantern-stack
	@helm template lantern charts/lantern-stack -f charts/lantern-stack/values-quickstart.yaml >/dev/null
	@echo "chart OK"

# --- cluster install targets -------------------------------------------
# preflight is a real prerequisite here, not a suggestion: if
# scripts/preflight-check.sh exits non-zero, make stops before helm ever runs.

NS ?= observability

preflight:
	./scripts/preflight-check.sh -n $(NS) -r lantern

# The OpenTelemetryCollector CRD (kube-prometheus-stack's CRDs are fine —
# they live in the chart's crds/ directory, which Helm guarantees runs before
# anything else) ships as a regular template in the opentelemetry-operator
# subchart, not Helm's special crds/ directory. On a genuinely fresh cluster
# that CRD doesn't exist yet when `helm install` first runs, so the chart
# skips creating collector.yaml/logs-collector.yaml's OpenTelemetryCollector
# resources rather than failing the release (see
# lantern.otelCollectorCRDReady in _helpers.tpl). The second `helm upgrade`
# with the identical flags picks them up now that the CRD is registered —
# always safe to run, a no-op everywhere else. Found on a real first-ever
# install against an empty cluster, not a hypothetical.
install-quickstart: preflight
	helm dependency update charts/lantern-stack
	helm lint charts/lantern-stack
	helm install lantern charts/lantern-stack \
		-n $(NS) --create-namespace \
		-f charts/lantern-stack/values-quickstart.yaml
	helm upgrade lantern charts/lantern-stack \
		-n $(NS) \
		-f charts/lantern-stack/values-quickstart.yaml

install-byo: preflight
	helm dependency update charts/lantern-stack
	helm lint charts/lantern-stack
	helm install lantern charts/lantern-stack -n $(NS) --create-namespace
	helm upgrade lantern charts/lantern-stack -n $(NS)
