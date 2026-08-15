package yamlx

import (
	"reflect"
	"testing"
)

func TestParseNestedStructures(t *testing.T) {
	src := []byte(`
name: checkout-api
enabled: true
replicas: 3
rate: 0.1
nested:
  a: 1
  b:
    c: deep
list:
  - one
  - two
maps:
  - name: first
    value: 1
  - name: second
    value: 2
flowMap: { x: 1, y: two }
flowSeq: [a, b, c]
empty: {}
quoted: "300ms"
colonInValue: "key: value"
`)

	got, err := Parse(src)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	m := got.(map[string]any)

	checks := map[string]any{
		"name":         "checkout-api",
		"enabled":      true,
		"replicas":     int64(3),
		"rate":         0.1,
		"quoted":       "300ms",
		"colonInValue": "key: value",
	}
	for k, want := range checks {
		if !reflect.DeepEqual(m[k], want) {
			t.Errorf("%s = %#v, want %#v", k, m[k], want)
		}
	}

	nested := m["nested"].(map[string]any)
	if nested["b"].(map[string]any)["c"] != "deep" {
		t.Errorf("nested.b.c = %#v", nested["b"])
	}

	if !reflect.DeepEqual(m["list"], []any{"one", "two"}) {
		t.Errorf("list = %#v", m["list"])
	}

	maps := m["maps"].([]any)
	if len(maps) != 2 || maps[0].(map[string]any)["name"] != "first" || maps[1].(map[string]any)["value"] != int64(2) {
		t.Errorf("maps = %#v", maps)
	}

	if !reflect.DeepEqual(m["flowMap"], map[string]any{"x": int64(1), "y": "two"}) {
		t.Errorf("flowMap = %#v", m["flowMap"])
	}
	if !reflect.DeepEqual(m["flowSeq"], []any{"a", "b", "c"}) {
		t.Errorf("flowSeq = %#v", m["flowSeq"])
	}
	if !reflect.DeepEqual(m["empty"], map[string]any{}) {
		t.Errorf("empty = %#v", m["empty"])
	}
}

func TestCommentsAreStrippedButNotInsideQuotes(t *testing.T) {
	src := []byte(`
# a leading comment
a: 1  # trailing comment
b: "value # not a comment"
c: url#fragment
`)
	m, err := Parse(src)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	mm := m.(map[string]any)
	if mm["a"] != int64(1) {
		t.Errorf("a = %#v", mm["a"])
	}
	if mm["b"] != "value # not a comment" {
		t.Errorf("b = %#v", mm["b"])
	}
	if mm["c"] != "url#fragment" {
		t.Errorf("c = %#v (a # without a preceding space is not a comment)", mm["c"])
	}
}

func TestBlockScalar(t *testing.T) {
	src := []byte("expr: |\n  sum(rate(x[5m]))\n  /\n  sum(rate(y[5m]))\nnext: after\n")
	m, err := Parse(src)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	mm := m.(map[string]any)
	want := "sum(rate(x[5m]))\n/\nsum(rate(y[5m]))\n"
	if mm["expr"] != want {
		t.Errorf("expr = %q, want %q", mm["expr"], want)
	}
	if mm["next"] != "after" {
		t.Errorf("parsing did not resume after the block scalar: %#v", mm["next"])
	}
}

func TestMultiDocument(t *testing.T) {
	docs, err := ParseAll([]byte("a: 1\n---\nb: 2\n---\nc: 3\n"))
	if err != nil {
		t.Fatalf("ParseAll: %v", err)
	}
	if len(docs) != 3 {
		t.Fatalf("got %d documents, want 3", len(docs))
	}
}

func TestUnsupportedFeaturesAreRejectedLoudly(t *testing.T) {
	// Silently mishandling an anchor would corrupt a user's config. Failing is
	// the correct behaviour for an intentionally partial parser.
	if _, err := Parse([]byte("a: &anchor 1\nb: *anchor\n")); err == nil {
		t.Error("anchors must be rejected, not silently mishandled")
	}
	if _, err := Parse([]byte("folded: >\n  some text\n")); err == nil {
		t.Error("folded scalars must be rejected")
	}
	if _, err := Parse([]byte("dup: 1\ndup: 2\n")); err == nil {
		t.Error("duplicate keys must be rejected")
	}
}

func TestUnknownFieldsAreRejected(t *testing.T) {
	var v struct {
		A int `json:"a"`
	}
	if err := Unmarshal([]byte("a: 1\ntypoed: 2\n"), &v); err == nil {
		t.Error("an unknown field must fail rather than being silently dropped")
	}
}

// ---------------------------------------------------------------------------

func TestEmitOrderingIsStable(t *testing.T) {
	build := func() string {
		m := NewMap(
			"apiVersion", S("v1"),
			"kind", S("ConfigMap"),
			"metadata", NewMap("name", S("x"), "namespace", S("y")),
			"list", NewSeq(S("a"), S("b")),
			"nestedList", NewSeq(NewMap("k", S("v"), "n", I(1))),
			"emptyMap", NewMap(),
			"emptySeq", NewSeq(),
		)
		return Encode(m)
	}
	first := build()
	for i := 0; i < 100; i++ {
		if build() != first {
			t.Fatal("emission is not deterministic")
		}
	}

	want := `apiVersion: v1
kind: ConfigMap
metadata:
  name: x
  namespace: y
list:
  - a
  - b
nestedList:
  - k: v
    n: 1
emptyMap: {}
emptySeq: []
`
	if first != want {
		t.Errorf("emitted:\n%s\nwant:\n%s", first, want)
	}
}

func TestQuotingProtectsAmbiguousScalars(t *testing.T) {
	cases := map[string]string{
		"plain":   "plain",
		"true":    `"true"`,
		"123":     `"123"`,
		"1.5":     `"1.5"`,
		"":        `""`,
		"a: b":    `"a: b"`,
		"-lead":   `"-lead"`,
		"yes":     `"yes"`,
		"has #ok": `"has #ok"`,
		"300ms":   "300ms",
	}
	for in, want := range cases {
		got := Encode(NewMap("k", S(in)))
		wantLine := "k: " + want + "\n"
		if got != wantLine {
			t.Errorf("S(%q) emitted %q, want %q", in, got, wantLine)
		}
	}
}

// TestRoundTrip proves the emitter and parser agree, which is what makes the
// golden files trustworthy as a contract.
func TestRoundTrip(t *testing.T) {
	src := Encode(NewMap(
		"apiVersion", S("monitoring.coreos.com/v1"),
		"kind", S("PrometheusRule"),
		"spec", NewMap("groups", NewSeq(
			NewMap("name", S("g1"), "rules", NewSeq(
				NewMap("record", S("r"), "expr", Lit("sum(rate(x[5m]))\n/\nsum(rate(y[5m]))")),
			)),
		)),
	))

	parsed, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("round trip failed to parse:\n%s\nerror: %v", src, err)
	}
	m := parsed.(map[string]any)
	if m["kind"] != "PrometheusRule" {
		t.Errorf("kind = %#v", m["kind"])
	}
	groups := m["spec"].(map[string]any)["groups"].([]any)
	rules := groups[0].(map[string]any)["rules"].([]any)
	expr := rules[0].(map[string]any)["expr"].(string)
	if expr != "sum(rate(x[5m]))\n/\nsum(rate(y[5m]))" {
		t.Errorf("expr round-tripped as %q", expr)
	}
}
