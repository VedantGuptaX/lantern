package yamlx

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Node is an ordered YAML value. Ordering is explicit rather than derived from
// a Go map, so emitted output is byte-stable across runs and across Go
// versions. Determinism is a hard requirement of the compiler contract: golden
// tests and `lantern diff` both depend on it.
type Node interface {
	writeYAML(b *strings.Builder, indent int)
}

// ---------- scalars ----------

type scalarNode struct {
	text  string
	block bool
	// plain marks a scalar whose text is already a valid unquoted YAML token
	// — a number, a boolean, or a PromQL expression. Without this flag the
	// quoting analysis below would render I(1) as the string "1", which
	// changes the type of every numeric field we emit.
	plain bool
}

// S is a string scalar. Its text is quoted whenever YAML would otherwise
// reinterpret it as a number, boolean, or null.
func S(s string) Node { return scalarNode{text: s} }

// I is an integer scalar.
func I(i int) Node { return scalarNode{text: strconv.Itoa(i), plain: true} }

// F is a float scalar rendered without a trailing `.0` for integral values.
func F(f float64) Node {
	return scalarNode{text: strconv.FormatFloat(f, 'f', -1, 64), plain: true}
}

// B is a boolean scalar.
func B(v bool) Node { return scalarNode{text: strconv.FormatBool(v), plain: true} }

// Lit is a literal block scalar (`|`), used for embedded PromQL and JSON.
func Lit(s string) Node { return scalarNode{text: s, block: true} }

// Raw emits text with no quoting analysis. Use only for values already known
// to be safe plain scalars, such as a numeric PromQL threshold.
func Raw(s string) Node { return scalarNode{text: s, plain: true} }

func (s scalarNode) writeYAML(b *strings.Builder, indent int) {
	b.WriteString(s.render(indent))
}

func (s scalarNode) render(indent int) string {
	if s.plain && !s.block && !strings.Contains(s.text, "\n") {
		return s.text
	}
	if s.block || strings.Contains(s.text, "\n") {
		pad := strings.Repeat(" ", indent+2)
		var sb strings.Builder
		sb.WriteString("|-\n")
		lines := strings.Split(strings.TrimRight(s.text, "\n"), "\n")
		for i, l := range lines {
			sb.WriteString(pad)
			sb.WriteString(l)
			if i < len(lines)-1 {
				sb.WriteString("\n")
			}
		}
		return sb.String()
	}
	return quoteIfNeeded(s.text)
}

var plainNumeric = regexp.MustCompile(`^-?(\d+\.?\d*|\.\d+)([eE][-+]?\d+)?$`)

var reserved = map[string]bool{
	"true": true, "false": true, "null": true, "~": true, "yes": true,
	"no": true, "on": true, "off": true, "True": true, "False": true,
	"Null": true, "Yes": true, "No": true, "On": true, "Off": true,
}

func quoteIfNeeded(s string) string {
	if s == "" {
		return `""`
	}
	if reserved[s] || plainNumeric.MatchString(s) {
		return strconv.Quote(s)
	}
	if s != strings.TrimSpace(s) {
		return strconv.Quote(s)
	}
	switch s[0] {
	case '-', '?', ':', ',', '[', ']', '{', '}', '#', '&', '*', '!', '|', '>', '\'', '"', '%', '@', '`':
		return strconv.Quote(s)
	}
	if strings.Contains(s, ": ") || strings.Contains(s, " #") || strings.HasSuffix(s, ":") {
		return strconv.Quote(s)
	}
	return s
}

// ---------- mapping ----------

// Map is an insertion-ordered mapping.
type Map struct {
	keys   []string
	values map[string]Node
}

// NewMap builds an ordered mapping from alternating key/Node pairs.
func NewMap(kv ...any) *Map {
	m := &Map{values: map[string]Node{}}
	for i := 0; i+1 < len(kv); i += 2 {
		k, ok := kv[i].(string)
		if !ok {
			panic(fmt.Sprintf("yamlx.NewMap: key %d is not a string", i))
		}
		m.Set(k, kv[i+1].(Node))
	}
	return m
}

// Set appends or replaces a key, preserving first-insertion order.
func (m *Map) Set(k string, v Node) *Map {
	if _, exists := m.values[k]; !exists {
		m.keys = append(m.keys, k)
	}
	m.values[k] = v
	return m
}

// SetIf sets the key only when cond holds, keeping call sites free of ifs.
func (m *Map) SetIf(cond bool, k string, v Node) *Map {
	if cond {
		m.Set(k, v)
	}
	return m
}

// Get returns a nested node by key, or nil.
func (m *Map) Get(k string) Node { return m.values[k] }

// Len reports the number of keys.
func (m *Map) Len() int { return len(m.keys) }

func (m *Map) writeYAML(b *strings.Builder, indent int) {
	if len(m.keys) == 0 {
		b.WriteString("{}")
		return
	}
	pad := strings.Repeat(" ", indent)
	for i, k := range m.keys {
		if i > 0 {
			b.WriteString("\n")
			b.WriteString(pad)
		}
		b.WriteString(quoteIfNeeded(k))
		b.WriteString(":")
		writeChild(b, m.values[k], indent)
	}
}

// ---------- sequence ----------

// Seq is an ordered sequence.
type Seq struct{ items []Node }

// NewSeq builds a sequence.
func NewSeq(items ...Node) *Seq { return &Seq{items: items} }

// Add appends an item.
func (s *Seq) Add(n Node) *Seq { s.items = append(s.items, n); return s }

// Len reports the number of items.
func (s *Seq) Len() int { return len(s.items) }

// Strings builds a sequence of string scalars.
func Strings(vals ...string) *Seq {
	s := &Seq{}
	for _, v := range vals {
		s.Add(S(v))
	}
	return s
}

func (s *Seq) writeYAML(b *strings.Builder, indent int) {
	if len(s.items) == 0 {
		b.WriteString("[]")
		return
	}
	pad := strings.Repeat(" ", indent)
	for i, item := range s.items {
		if i > 0 {
			b.WriteString("\n")
			b.WriteString(pad)
		}
		b.WriteString("- ")
		switch v := item.(type) {
		case scalarNode:
			b.WriteString(v.render(indent + 2))
		default:
			item.writeYAML(b, indent+2)
		}
	}
}

// writeChild emits the value part of `key:`, choosing inline or block layout.
func writeChild(b *strings.Builder, n Node, indent int) {
	switch v := n.(type) {
	case scalarNode:
		b.WriteString(" ")
		b.WriteString(v.render(indent))
	case *Map:
		if v.Len() == 0 {
			b.WriteString(" {}")
			return
		}
		b.WriteString("\n")
		b.WriteString(strings.Repeat(" ", indent+2))
		v.writeYAML(b, indent+2)
	case *Seq:
		if v.Len() == 0 {
			b.WriteString(" []")
			return
		}
		b.WriteString("\n")
		b.WriteString(strings.Repeat(" ", indent+2))
		v.writeYAML(b, indent+2)
	case nil:
		b.WriteString(" null")
	default:
		b.WriteString(" ")
		n.writeYAML(b, indent)
	}
}

// Encode renders a node as a single YAML document.
func Encode(n Node) string {
	var b strings.Builder
	n.writeYAML(&b, 0)
	b.WriteString("\n")
	return b.String()
}
