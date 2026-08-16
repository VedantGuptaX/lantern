// Package discover turns existing Kubernetes workloads into draft
// ServiceObservability specs.
//
// This is what makes "all my microservices" possible without writing a spec
// per service by hand. It reads workloads from manifest files or from
// `kubectl get -o yaml`, so it works against a live cluster and against a
// GitOps repository with the same code path — and, unlike a controller, it
// needs no cluster credentials to test.
//
// Everything here is inference, and inference is wrong sometimes. Every field
// the discoverer guesses carries a confidence level, and the CLI reports what
// it was unsure about so a human reviews the draft before applying it.
package discover

import (
	"fmt"
	"sort"
	"strings"

	"github.com/VedantGuptaX/lantern/pkg/yamlx"
)

// Workload is the subset of a Kubernetes workload that discovery reads.
type Workload struct {
	Kind        string
	Name        string
	Namespace   string
	Labels      map[string]string
	Annotations map[string]string
	Selector    map[string]string
	Containers  []Container
}

// Container is one container in a pod template.
type Container struct {
	Name    string
	Image   string
	Command []string
	Args    []string
	Env     map[string]string
	Ports   []Port
	// GPULimit is the container's nvidia.com/gpu resource limit, if any
	// (e.g. "1"). Empty when no GPU is requested.
	GPULimit string
}

// Port is a named container port.
type Port struct {
	Name string
	Port int
}

// workloadKinds are the kinds discovery understands. Anything else is skipped
// rather than guessed at.
var workloadKinds = map[string]bool{
	"Deployment":  true,
	"StatefulSet": true,
	"DaemonSet":   true,
	"CronJob":     true,
}

// ParseWorkloads extracts workloads from a YAML stream. It accepts plain
// manifests, multi-document streams, and the `kind: List` wrapper that
// `kubectl get -o yaml` produces.
func ParseWorkloads(data []byte) ([]Workload, error) {
	docs, err := yamlx.ParseAll(data)
	if err != nil {
		return nil, err
	}

	var out []Workload
	for _, doc := range docs {
		m, ok := doc.(map[string]any)
		if !ok {
			continue
		}
		kind, _ := m["kind"].(string)

		if kind == "List" {
			items, _ := m["items"].([]any)
			for _, it := range items {
				im, ok := it.(map[string]any)
				if !ok {
					continue
				}
				if w, ok := parseOne(im); ok {
					out = append(out, w)
				}
			}
			continue
		}

		if w, ok := parseOne(m); ok {
			out = append(out, w)
		}
	}

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func parseOne(m map[string]any) (Workload, bool) {
	kind, _ := m["kind"].(string)
	if !workloadKinds[kind] {
		return Workload{}, false
	}

	meta, _ := m["metadata"].(map[string]any)
	if meta == nil {
		return Workload{}, false
	}
	name, _ := meta["name"].(string)
	if name == "" {
		return Workload{}, false
	}
	ns, _ := meta["namespace"].(string)
	if ns == "" {
		ns = "default"
	}

	w := Workload{
		Kind:        kind,
		Name:        name,
		Namespace:   ns,
		Labels:      stringMap(meta["labels"]),
		Annotations: stringMap(meta["annotations"]),
	}

	spec, _ := m["spec"].(map[string]any)
	if spec == nil {
		return w, true
	}

	// CronJob nests its pod template two levels deeper than the others.
	podSpecOwner := spec
	if kind == "CronJob" {
		if jt, ok := spec["jobTemplate"].(map[string]any); ok {
			if js, ok := jt["spec"].(map[string]any); ok {
				podSpecOwner = js
			}
		}
	}

	if sel, ok := spec["selector"].(map[string]any); ok {
		w.Selector = stringMap(sel["matchLabels"])
	}

	template, _ := podSpecOwner["template"].(map[string]any)
	if template == nil {
		return w, true
	}
	if tmeta, ok := template["metadata"].(map[string]any); ok {
		// Pod-template annotations matter: an existing inject-* annotation is
		// the strongest possible signal about instrumentation.
		for k, v := range stringMap(tmeta["annotations"]) {
			if w.Annotations == nil {
				w.Annotations = map[string]string{}
			}
			w.Annotations[k] = v
		}
		if len(w.Selector) == 0 {
			w.Selector = stringMap(tmeta["labels"])
		}
	}

	tspec, _ := template["spec"].(map[string]any)
	if tspec == nil {
		return w, true
	}
	containers, _ := tspec["containers"].([]any)
	for _, c := range containers {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		w.Containers = append(w.Containers, parseContainer(cm))
	}

	return w, true
}

func parseContainer(m map[string]any) Container {
	c := Container{
		Env: map[string]string{},
	}
	c.Name, _ = m["name"].(string)
	c.Image, _ = m["image"].(string)
	c.Command = stringSlice(m["command"])
	c.Args = stringSlice(m["args"])

	if envs, ok := m["env"].([]any); ok {
		for _, e := range envs {
			em, ok := e.(map[string]any)
			if !ok {
				continue
			}
			k, _ := em["name"].(string)
			if k == "" {
				continue
			}
			// valueFrom entries have no literal value; record presence only,
			// which is all the detection heuristics need.
			v, _ := em["value"].(string)
			c.Env[k] = v
		}
	}

	if ports, ok := m["ports"].([]any); ok {
		for _, p := range ports {
			pm, ok := p.(map[string]any)
			if !ok {
				continue
			}
			name, _ := pm["name"].(string)
			num := 0
			switch v := pm["containerPort"].(type) {
			case int64:
				num = int(v)
			case float64:
				num = int(v)
			}
			c.Ports = append(c.Ports, Port{Name: name, Port: num})
		}
	}

	// nvidia.com/gpu is checked in both limits and requests: the device
	// plugin only actually schedules a GPU via limits (Kubernetes requires
	// requests==limits for extended resources), but reading both means a
	// hand-written manifest that only sets requests is still recognised.
	if res, ok := m["resources"].(map[string]any); ok {
		for _, section := range []string{"limits", "requests"} {
			rm, ok := res[section].(map[string]any)
			if !ok {
				continue
			}
			if v, ok := rm["nvidia.com/gpu"]; ok {
				c.GPULimit = fmt.Sprint(v)
				break
			}
		}
	}
	return c
}

// GPURequested reports whether any container in the pod template requests an
// NVIDIA GPU via the nvidia.com/gpu extended resource.
func (w Workload) GPURequested() bool {
	for _, c := range w.Containers {
		if c.GPULimit != "" {
			return true
		}
	}
	return false
}

// Images returns every container image, for runtime detection.
func (w Workload) Images() []string {
	out := make([]string, 0, len(w.Containers))
	for _, c := range w.Containers {
		out = append(out, c.Image)
	}
	return out
}

// Commands returns each container's command line, for runtime detection.
func (w Workload) Commands() []string {
	var out []string
	for _, c := range w.Containers {
		parts := append(append([]string{}, c.Command...), c.Args...)
		if len(parts) > 0 {
			out = append(out, strings.Join(parts, " "))
		}
	}
	return out
}

// Env merges environment variables across containers.
func (w Workload) Env() map[string]string {
	out := map[string]string{}
	for _, c := range w.Containers {
		for k, v := range c.Env {
			out[k] = v
		}
	}
	return out
}

// Ports merges container ports across containers.
func (w Workload) Ports() []Port {
	var out []Port
	for _, c := range w.Containers {
		out = append(out, c.Ports...)
	}
	return out
}

func (w Workload) String() string {
	return fmt.Sprintf("%s %s/%s", w.Kind, w.Namespace, w.Name)
}

func stringMap(v any) map[string]string {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, val := range m {
		if s, ok := val.(string); ok {
			out[k] = s
		}
	}
	return out
}

func stringSlice(v any) []string {
	items, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, it := range items {
		if s, ok := it.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
