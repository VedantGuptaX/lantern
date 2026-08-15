// Package kube is a minimal typed-enough Kubernetes object model.
//
// P0 deliberately does not vendor the upstream Go types for the
// OpenTelemetry Operator, Prometheus Operator, or grafana-operator. Doing so
// would pull three independently-versioned dependency trees into the compiler
// and couple our release cadence to theirs. The golden-file tests are the real
// contract: they pin the emitted YAML byte-for-byte, which is what actually
// breaks when an upstream schema changes. Swapping in upstream types is a P2
// concern, and only inside pkg/emit.
package kube

import (
	"sort"
	"strings"

	"github.com/VedantGuptaX/lantern/pkg/yamlx"
)

// Object is a single emitted Kubernetes resource.
type Object struct {
	APIVersion string
	Kind       string
	Name       string
	Namespace  string
	Body       *yamlx.Map
}

// New builds an object with a standard metadata block.
func New(apiVersion, kind, name, namespace string, labels map[string]string) *Object {
	meta := yamlx.NewMap("name", yamlx.S(name))
	if namespace != "" {
		meta.Set("namespace", yamlx.S(namespace))
	}
	if len(labels) > 0 {
		meta.Set("labels", SortedStringMap(labels))
	}
	return &Object{
		APIVersion: apiVersion,
		Kind:       kind,
		Name:       name,
		Namespace:  namespace,
		Body: yamlx.NewMap(
			"apiVersion", yamlx.S(apiVersion),
			"kind", yamlx.S(kind),
			"metadata", meta,
		),
	}
}

// Set adds a top-level field such as `spec`.
func (o *Object) Set(key string, n yamlx.Node) *Object {
	o.Body.Set(key, n)
	return o
}

// Ref is the stable identity used for ordering and diffing.
func (o *Object) Ref() string {
	return strings.Join([]string{o.APIVersion, o.Kind, o.Namespace, o.Name}, "/")
}

// YAML renders the object as a single document.
func (o *Object) YAML() string { return yamlx.Encode(o.Body) }

// SortedStringMap renders a Go map with keys in sorted order, so output does
// not depend on Go's randomised map iteration.
func SortedStringMap(m map[string]string) *yamlx.Map {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := yamlx.NewMap()
	for _, k := range keys {
		out.Set(k, yamlx.S(m[k]))
	}
	return out
}

// Sort orders objects deterministically: apply-order by kind, then by
// namespace and name. Kinds that must exist before others reference them come
// first.
func Sort(objs []*Object) {
	rank := map[string]int{
		"Namespace":        0,
		"Instrumentation":  1,
		"ServiceMonitor":   2,
		"PodMonitor":       2,
		"PrometheusRule":   3,
		"GrafanaFolder":    4,
		"GrafanaDashboard": 5,
	}
	sort.SliceStable(objs, func(i, j int) bool {
		ri, oki := rank[objs[i].Kind]
		rj, okj := rank[objs[j].Kind]
		if !oki {
			ri = 50
		}
		if !okj {
			rj = 50
		}
		if ri != rj {
			return ri < rj
		}
		if objs[i].Kind != objs[j].Kind {
			return objs[i].Kind < objs[j].Kind
		}
		if objs[i].Namespace != objs[j].Namespace {
			return objs[i].Namespace < objs[j].Namespace
		}
		return objs[i].Name < objs[j].Name
	})
}

// Stream renders objects as a multi-document YAML stream.
func Stream(objs []*Object) string {
	var b strings.Builder
	for i, o := range objs {
		if i > 0 {
			b.WriteString("---\n")
		}
		b.WriteString(o.YAML())
	}
	return b.String()
}
