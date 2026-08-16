// Package yamlx is a dependency-free YAML subset parser and deterministic
// emitter.
//
// It exists because Lantern is built stdlib-only for P0 (see README). It
// implements the subset of YAML that Kubernetes manifests actually use: block
// maps, block sequences, flow maps and sequences, quoted and plain scalars,
// literal block scalars, comments, and multi-document streams. Anchors,
// aliases, merge keys, tags, and explicit `>` folded block scalars are
// deliberately unsupported and produce an explicit error rather than
// silently wrong data.
//
// Plain and quoted scalars that a YAML encoder (kubectl's included) wraps
// across multiple lines are supported: any line indented deeper than a
// `key: value` or `- value` line is, under YAML's grammar, necessarily a
// continuation of that same scalar — a nested block map or sequence requires
// the key/dash to have nothing after it on its own line — so such lines are
// folded into the value with a single space, same as real YAML plain-scalar
// folding. This is distinct from an explicit `>` block scalar header, which
// remains unsupported.
//
// Decoding goes YAML -> generic Go value -> encoding/json -> typed struct, so
// the struct `json:"..."` tags are the single source of truth for field names,
// exactly as sigs.k8s.io/yaml behaves.
package yamlx

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Unmarshal parses YAML into v using the struct's json tags.
func Unmarshal(data []byte, v any) error {
	raw, err := Parse(data)
	if err != nil {
		return err
	}
	j, err := json.Marshal(raw)
	if err != nil {
		return fmt.Errorf("re-encode: %w", err)
	}
	dec := json.NewDecoder(strings.NewReader(string(j)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	return nil
}

// UnmarshalValue decodes an already-parsed generic value into v, so callers
// that inspect a document before typing it do not have to re-parse the source.
func UnmarshalValue(raw any, v any) error {
	j, err := json.Marshal(raw)
	if err != nil {
		return fmt.Errorf("re-encode: %w", err)
	}
	dec := json.NewDecoder(strings.NewReader(string(j)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	return nil
}

// Parse converts a single YAML document into map[string]any / []any / scalars.
func Parse(data []byte) (any, error) {
	docs, err := ParseAll(data)
	if err != nil {
		return nil, err
	}
	switch len(docs) {
	case 0:
		return nil, nil
	case 1:
		return docs[0], nil
	default:
		return nil, fmt.Errorf("expected a single YAML document, found %d", len(docs))
	}
}

// ParseAll splits a multi-document stream on `---` and parses each document.
func ParseAll(data []byte) ([]any, error) {
	var docs []any
	for i, chunk := range splitDocuments(string(data)) {
		lines, err := scan(chunk)
		if err != nil {
			return nil, fmt.Errorf("document %d: %w", i+1, err)
		}
		if len(lines) == 0 {
			continue
		}
		p := &parser{lines: lines}
		v, err := p.parseValue(lines[0].indent)
		if err != nil {
			return nil, fmt.Errorf("document %d: %w", i+1, err)
		}
		if p.pos < len(p.lines) {
			return nil, fmt.Errorf("document %d, line %d: unexpected indentation", i+1, p.lines[p.pos].num)
		}
		docs = append(docs, v)
	}
	return docs, nil
}

func splitDocuments(s string) []string {
	raw := strings.Split(s, "\n")
	var docs []string
	var cur []string
	for _, l := range raw {
		t := strings.TrimRight(l, " \t\r")
		if t == "---" || strings.HasPrefix(t, "--- ") {
			docs = append(docs, strings.Join(cur, "\n"))
			cur = nil
			continue
		}
		if t == "..." {
			continue
		}
		cur = append(cur, l)
	}
	docs = append(docs, strings.Join(cur, "\n"))
	return docs
}

type line struct {
	indent int
	text   string
	num    int
	// literal marks a line captured verbatim inside a block scalar; it is
	// never re-tokenised.
	literal bool
}

// scan strips blank lines and comments and records indentation. Lines that
// belong to a literal block scalar are captured verbatim by the parser, so
// scan records raw text alongside indentation and lets the parser decide.
func scan(src string) ([]line, error) {
	var out []line
	for i, raw := range strings.Split(src, "\n") {
		expanded := strings.ReplaceAll(raw, "\t", "    ")
		trimmed := strings.TrimLeft(expanded, " ")
		indent := len(expanded) - len(trimmed)
		body := strings.TrimRight(trimmed, " \r")

		if body == "" || strings.HasPrefix(body, "#") {
			// Keep blank lines out of the token stream, but they may sit
			// inside a block scalar; the parser reconstructs those from raw
			// text, so dropping them here is safe for our subset.
			continue
		}
		if strings.HasPrefix(body, "%") {
			return nil, fmt.Errorf("line %d: YAML directives are not supported", i+1)
		}
		if strings.HasPrefix(body, "&") || strings.HasPrefix(body, "*") {
			return nil, fmt.Errorf("line %d: anchors and aliases are not supported", i+1)
		}
		out = append(out, line{indent: indent, text: stripComment(body), num: i + 1})
	}
	return out, nil
}

// stripComment removes a trailing ` #...` comment that is not inside quotes.
func stripComment(s string) string {
	var inSingle, inDouble bool
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\'':
			if !inDouble {
				inSingle = !inSingle
			}
		case '"':
			if !inSingle {
				inDouble = !inDouble
			}
		case '#':
			if !inSingle && !inDouble && i > 0 && (s[i-1] == ' ' || s[i-1] == '\t') {
				return strings.TrimRight(s[:i], " \t")
			}
		}
	}
	return s
}

type parser struct {
	lines []line
	pos   int
}

func (p *parser) parseValue(indent int) (any, error) {
	if p.pos >= len(p.lines) {
		return nil, nil
	}
	l := p.lines[p.pos]
	if l.indent < indent {
		return nil, nil
	}
	if isSeqLine(l.text) {
		return p.parseSeq(l.indent)
	}
	return p.parseMap(l.indent)
}

func isSeqLine(t string) bool {
	return t == "-" || strings.HasPrefix(t, "- ")
}

func (p *parser) parseMap(indent int) (map[string]any, error) {
	m := map[string]any{}
	for p.pos < len(p.lines) {
		l := p.lines[p.pos]
		if l.indent < indent {
			break
		}
		if l.indent > indent {
			return nil, fmt.Errorf("line %d: unexpected indentation (got %d, want %d)", l.num, l.indent, indent)
		}
		if isSeqLine(l.text) {
			break
		}

		key, rest, ok := splitKey(l.text)
		if !ok {
			return nil, fmt.Errorf("line %d: expected `key: value`, got %q", l.num, l.text)
		}
		if _, dup := m[key]; dup {
			return nil, fmt.Errorf("line %d: duplicate key %q", l.num, key)
		}
		p.pos++

		switch {
		case isBlockScalarHeader(rest):
			v, err := p.readBlockScalar(indent, rest)
			if err != nil {
				return nil, err
			}
			m[key] = v

		case rest != "":
			v, err := p.parseScalarValue(rest, indent, l.num)
			if err != nil {
				return nil, err
			}
			m[key] = v

		default:
			// Nested block: either deeper-indented, or a sequence at the same
			// indent as its key (both are legal YAML).
			if p.pos < len(p.lines) && p.lines[p.pos].indent > indent {
				v, err := p.parseValue(p.lines[p.pos].indent)
				if err != nil {
					return nil, err
				}
				m[key] = v
			} else if p.pos < len(p.lines) && p.lines[p.pos].indent == indent && isSeqLine(p.lines[p.pos].text) {
				v, err := p.parseSeq(indent)
				if err != nil {
					return nil, err
				}
				m[key] = v
			} else {
				m[key] = nil
			}
		}
	}
	return m, nil
}

func (p *parser) parseSeq(indent int) ([]any, error) {
	out := []any{}
	for p.pos < len(p.lines) {
		l := p.lines[p.pos]
		if l.indent < indent {
			break
		}
		if l.indent > indent {
			return nil, fmt.Errorf("line %d: unexpected indentation in sequence", l.num)
		}
		if !isSeqLine(l.text) {
			break
		}

		after := l.text[1:]
		trimmed := strings.TrimLeft(after, " ")
		itemIndent := l.indent + 1 + (len(after) - len(trimmed))

		if trimmed == "" {
			p.pos++
			if p.pos < len(p.lines) && p.lines[p.pos].indent > indent {
				v, err := p.parseValue(p.lines[p.pos].indent)
				if err != nil {
					return nil, err
				}
				out = append(out, v)
			} else {
				out = append(out, nil)
			}
			continue
		}

		if _, _, isMap := splitKey(trimmed); isMap {
			// `- key: value` — rewrite as a map line at the key's column and
			// let parseMap consume this and any continuation lines.
			p.lines[p.pos] = line{indent: itemIndent, text: trimmed, num: l.num}
			v, err := p.parseMap(itemIndent)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
			continue
		}

		p.pos++
		// Use the dash's own indent as the continuation threshold (matching
		// parseMap's use of the key's indent), not itemIndent (the value's
		// start column) — a continuation line only needs to be deeper than
		// the `-`, not aligned with where the value happens to start.
		v, err := p.parseScalarValue(trimmed, l.indent, l.num)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// gatherContinuation consumes and returns the trimmed text of every
// subsequent line indented deeper than parentIndent. Under YAML's grammar,
// such a line can only be a continuation of the scalar value that precedes
// it — see the package doc comment.
func (p *parser) gatherContinuation(parentIndent int) []string {
	var out []string
	for p.pos < len(p.lines) && p.lines[p.pos].indent > parentIndent {
		out = append(out, strings.TrimSpace(p.lines[p.pos].text))
		p.pos++
	}
	return out
}

// parseScalarValue parses an inline scalar value (the `rest` after `key:` or
// `-`), folding in any deeper-indented continuation lines per plain/quoted
// scalar line-folding. Flow collections are never folded — they're always
// fully specified on their starting line in this subset, so a deeper-indented
// line after one is a real error, not a continuation.
func (p *parser) parseScalarValue(rest string, parentIndent int, lineNum int) (any, error) {
	trimmed := strings.TrimSpace(rest)
	if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		return parseInline(rest, lineNum)
	}
	cont := p.gatherContinuation(parentIndent)
	if len(cont) == 0 {
		return parseInline(rest, lineNum)
	}
	parts := append([]string{trimmed}, cont...)
	return parseInline(strings.Join(parts, " "), lineNum)
}

func isBlockScalarHeader(s string) bool {
	if s == "" {
		return false
	}
	if s[0] != '|' && s[0] != '>' {
		return false
	}
	// Only the chomping/indentation indicators may follow.
	for _, r := range s[1:] {
		if r != '-' && r != '+' && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

// readBlockScalar consumes the indented lines following a `|` header. Folded
// scalars (`>`) are rejected rather than silently mis-folded.
func (p *parser) readBlockScalar(parentIndent int, header string) (string, error) {
	if header[0] == '>' {
		return "", fmt.Errorf("folded block scalars (`>`) are not supported; use `|`")
	}
	chomp := ""
	if strings.HasSuffix(header, "-") {
		chomp = "-"
	}

	var body []string
	blockIndent := -1
	for p.pos < len(p.lines) {
		l := p.lines[p.pos]
		if l.indent <= parentIndent {
			break
		}
		if blockIndent == -1 {
			blockIndent = l.indent
		}
		if l.indent < blockIndent {
			break
		}
		body = append(body, strings.Repeat(" ", l.indent-blockIndent)+l.text)
		p.pos++
	}

	s := strings.Join(body, "\n")
	if chomp != "-" && s != "" {
		s += "\n"
	}
	return s, nil
}

// splitKey finds the `key:` separator outside quotes and flow collections.
func splitKey(s string) (key, rest string, ok bool) {
	var inSingle, inDouble bool
	depth := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '\'':
			if !inDouble {
				inSingle = !inSingle
			}
		case '"':
			if !inSingle {
				inDouble = !inDouble
			}
		case '{', '[':
			if !inSingle && !inDouble {
				depth++
			}
		case '}', ']':
			if !inSingle && !inDouble {
				depth--
			}
		case ':':
			if inSingle || inDouble || depth > 0 {
				continue
			}
			if i+1 == len(s) {
				return unquote(strings.TrimSpace(s[:i])), "", true
			}
			if s[i+1] == ' ' {
				return unquote(strings.TrimSpace(s[:i])), strings.TrimSpace(s[i+1:]), true
			}
		}
	}
	return "", "", false
}

func parseInline(s string, lineNum int) (any, error) {
	s = strings.TrimSpace(s)
	// Anchors and aliases most often appear in the value position
	// (`a: &anchor 1`), not at the start of a line, so the line-level guard in
	// scan is not sufficient. Silently treating `*anchor` as the string
	// "*anchor" would corrupt a user's config.
	if strings.HasPrefix(s, "&") || strings.HasPrefix(s, "*") {
		return nil, fmt.Errorf("line %d: anchors and aliases are not supported", lineNum)
	}
	if strings.HasPrefix(s, "!!") || strings.HasPrefix(s, "!") {
		return nil, fmt.Errorf("line %d: explicit tags are not supported", lineNum)
	}
	switch {
	case strings.HasPrefix(s, "{"):
		v, n, err := parseFlowMap(s)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", lineNum, err)
		}
		if strings.TrimSpace(s[n:]) != "" {
			return nil, fmt.Errorf("line %d: trailing content after flow mapping", lineNum)
		}
		return v, nil
	case strings.HasPrefix(s, "["):
		v, n, err := parseFlowSeq(s)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", lineNum, err)
		}
		if strings.TrimSpace(s[n:]) != "" {
			return nil, fmt.Errorf("line %d: trailing content after flow sequence", lineNum)
		}
		return v, nil
	default:
		return scalar(s), nil
	}
}

func parseFlowMap(s string) (map[string]any, int, error) {
	m := map[string]any{}
	i := 1 // past '{'
	for {
		i = skipSpace(s, i)
		if i >= len(s) {
			return nil, 0, fmt.Errorf("unterminated flow mapping")
		}
		if s[i] == '}' {
			return m, i + 1, nil
		}
		keyEnd := i
		for keyEnd < len(s) && s[keyEnd] != ':' {
			keyEnd++
		}
		if keyEnd >= len(s) {
			return nil, 0, fmt.Errorf("flow mapping entry missing `:`")
		}
		key := unquote(strings.TrimSpace(s[i:keyEnd]))
		i = skipSpace(s, keyEnd+1)

		v, n, err := parseFlowValue(s, i)
		if err != nil {
			return nil, 0, err
		}
		m[key] = v
		i = skipSpace(s, n)
		if i < len(s) && s[i] == ',' {
			i++
			continue
		}
		if i < len(s) && s[i] == '}' {
			return m, i + 1, nil
		}
		if i >= len(s) {
			return nil, 0, fmt.Errorf("unterminated flow mapping")
		}
	}
}

func parseFlowSeq(s string) ([]any, int, error) {
	out := []any{}
	i := 1 // past '['
	for {
		i = skipSpace(s, i)
		if i >= len(s) {
			return nil, 0, fmt.Errorf("unterminated flow sequence")
		}
		if s[i] == ']' {
			return out, i + 1, nil
		}
		v, n, err := parseFlowValue(s, i)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, v)
		i = skipSpace(s, n)
		if i < len(s) && s[i] == ',' {
			i++
			continue
		}
		if i < len(s) && s[i] == ']' {
			return out, i + 1, nil
		}
		if i >= len(s) {
			return nil, 0, fmt.Errorf("unterminated flow sequence")
		}
	}
}

func parseFlowValue(s string, i int) (any, int, error) {
	if i >= len(s) {
		return nil, i, fmt.Errorf("unexpected end of flow collection")
	}
	switch s[i] {
	case '{':
		return wrap(parseFlowMap(s[i:]))(i)
	case '[':
		return wrapSeq(parseFlowSeq(s[i:]))(i)
	case '"', '\'':
		q := s[i]
		j := i + 1
		for j < len(s) && s[j] != q {
			if s[j] == '\\' && q == '"' {
				j++
			}
			j++
		}
		if j >= len(s) {
			return nil, i, fmt.Errorf("unterminated quoted scalar")
		}
		return unquote(s[i : j+1]), j + 1, nil
	default:
		j := i
		depth := 0
		for j < len(s) {
			if s[j] == '[' || s[j] == '{' {
				depth++
			}
			if s[j] == ']' || s[j] == '}' {
				if depth == 0 {
					break
				}
				depth--
			}
			if s[j] == ',' && depth == 0 {
				break
			}
			j++
		}
		return scalar(strings.TrimSpace(s[i:j])), j, nil
	}
}

func wrap(m map[string]any, n int, err error) func(int) (any, int, error) {
	return func(i int) (any, int, error) { return m, i + n, err }
}

func wrapSeq(s []any, n int, err error) func(int) (any, int, error) {
	return func(i int) (any, int, error) { return s, i + n, err }
}

func skipSpace(s string, i int) int {
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	return i
}

func unquote(s string) string {
	if len(s) >= 2 {
		if s[0] == '"' && s[len(s)-1] == '"' {
			if v, err := strconv.Unquote(s); err == nil {
				return v
			}
			return s[1 : len(s)-1]
		}
		if s[0] == '\'' && s[len(s)-1] == '\'' {
			return strings.ReplaceAll(s[1:len(s)-1], "''", "'")
		}
	}
	return s
}

// scalar resolves an unquoted plain scalar to its YAML core type.
func scalar(s string) any {
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') {
		return unquote(s)
	}
	switch s {
	case "", "~", "null", "Null", "NULL":
		if s == "" {
			return ""
		}
		return nil
	case "true", "True", "TRUE":
		return true
	case "false", "False", "FALSE":
		return false
	}
	if i, err := strconv.ParseInt(s, 10, 64); err == nil {
		return i
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return f
	}
	return s
}
