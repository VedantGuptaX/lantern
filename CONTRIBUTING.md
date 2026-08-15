# Contributing to Lantern

Thanks for looking. This document covers how the project is built and the
handful of rules that keep it coherent.

## Quick start

```bash
git clone https://github.com/VedantGuptaX/lantern.git
cd lantern
make all      # fmt, vet, test, build
make demo     # discover → synth, end to end
```

Go 1.24+. No other dependencies — Lantern builds entirely against the standard
library, deliberately (see [Constraints](#constraints)).

## Where to help

The most valuable open work, roughly in order:

1. **The P1 dashboard generator.** Generate Grafana dashboards from a service
   spec via the [Grafana Foundation SDK](https://github.com/grafana/grafana-foundation-sdk).
   Start with the `http` template — RED panels, SLO burn-rate panels, a runtime
   section, a Kubernetes section. This is the highest-value and highest-risk
   piece of the whole project: a mediocre generated dashboard is worse than
   none, so it needs real workloads to test against.
2. **`GrafanaFolder` / team RBAC emission** — the "give developers access" half.
3. **Log ↔ trace correlation.** `signals.logs.correlateTraceID` is parsed and
   currently ignored.
4. **More `serviceKind` templates** — `worker`, `cron`, `database`.
5. **An Odigos emitter.** Odigos solves instrumentation orchestration well;
   emitting its CRs instead of our own is composition, not defeat.

Open an issue before starting anything large, so we don't duplicate work.

## Architecture rules

Three rules, all enforced by tests. Breaking them breaks the project's
guarantees, so they are not negotiable without a design discussion first.

### 1. `pkg/compile` is a pure function

`Compile(spec, stack, facts) → objects` performs **no I/O**: no cluster access,
no network, no filesystem, no clock, no randomness. Anything impure is resolved
by the caller and arrives in `Facts`.

This is what gives us:

- the CLI and the future operator sharing one implementation, so `synth` output
  can never drift from what the operator applies
- golden-file tests as a real contract
- `lantern diff` as compile-then-compare, with no second code path

`pkg/compile/boundary_test.go` parses the package's imports and fails the build
on `os`, `time`, `net`, `client-go`, and friends. If you need one of those, you
need `Facts` instead.

### 2. Output is byte-deterministic

Same input, byte-identical output, every run. Go randomises map iteration
order, so **never range over a map to produce output** — sort the keys, or use
the ordered `yamlx.Map`. `TestDeterminism` compiles the same input 50 times and
fails on any difference.

This is not fussiness: non-deterministic output means every GitOps sync shows a
spurious diff, and nobody trusts the tool again.

### 3. Every guess is flagged, and every generated thing is overridable

`lantern discover` infers a lot. Every inference carries its reasoning and a
confidence level, and low-confidence guesses are marked `REVIEW` in the output.

Similarly, anything the compiler generates must be overridable. A developer who
cannot override the generator abandons the tool and hand-writes JSON — which is
exactly the situation the project exists to fix.

Two things discovery must never do: invent an SLO, or invent a team. An
objective is a commitment, and an unowned alert is an unanswered alert.

## Constraints

**Standard library only.** `pkg/yamlx` is a purpose-built YAML subset parser
and deterministic ordered emitter rather than a dependency. It rejects anchors,
aliases, tags and folded scalars loudly rather than mishandling them silently.
If you hit a YAML construct it cannot parse, extend it or reject it — do not
make it guess.

**No vendored upstream CRD types.** We emit into schemas owned by the
OpenTelemetry Operator, Prometheus Operator, and grafana-operator. Vendoring
their Go types would pull three independently-versioned dependency trees into
the compiler and couple our releases to theirs. The golden files are the
contract instead: they pin emitted YAML byte-for-byte, which is exactly what
breaks when an upstream schema moves.

## Testing

```bash
go test ./...                                  # everything
go test ./pkg/compile -run TestGolden -update  # re-baseline golden files
```

Four things the suite guards:

| Test | Guards |
|---|---|
| `TestGolden` | Every emitted byte, across fixtures in `testdata/golden/` |
| `TestDeterminism` | 50 compiles of the same input produce identical output |
| `TestCompilerPurityBoundary` | `pkg/compile` imports nothing impure |
| `TestGoldenOutputIsParseable` | Emitted YAML parses back and has `apiVersion`/`kind`/`metadata.name` |

**Golden file changes need review.** A golden diff is either an intended
behaviour change or an upstream schema break. Run `make golden`, read the diff,
and say which it is in your PR description.

**Test the arithmetic, not the YAML.** A burn-rate threshold off by 10x is
invisible in a YAML review and either never fires or pages constantly, so
`pkg/emit/prom/rules_test.go` checks the numbers directly. Anything with
consequences that are hard to eyeball deserves the same treatment.

## Pull requests

- One logical change per PR.
- `make all` must pass. `gofmt` is not optional.
- New behaviour needs a test. New generated output needs a golden fixture.
- Explain *why* in the description. The code says what.
- Comments should explain reasoning and consequences, not restate the code.
  The existing comments are the house style — when a decision has a failure
  mode, name the failure mode.

## Licensing

Lantern is [Apache 2.0](LICENSE). By contributing, you agree your
contributions are licensed under the same terms, per section 5 of the license.
No separate CLA.

If you add a dependency on, or generate config for, another project, add it to
[NOTICE](NOTICE).

## Code of conduct

Be decent to people. Assume good faith, critique the work rather than the
person, and take the outcome the project needs over the outcome that wins the
argument.
