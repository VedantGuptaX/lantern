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

install-quickstart: preflight
	helm dependency update charts/lantern-stack
	helm lint charts/lantern-stack
	helm install lantern charts/lantern-stack \
		-n $(NS) --create-namespace \
		-f charts/lantern-stack/values-quickstart.yaml

install-byo: preflight
	helm dependency update charts/lantern-stack
	helm lint charts/lantern-stack
	helm install lantern charts/lantern-stack -n $(NS) --create-namespace
