package gateway

import (
	"bytes"
	"encoding/json"

	"github.com/yetone/magpie/internal/provider"
)

// Grok's flat function names are local to one request. Codex gets the
// original namespace and name back in both stream items and final output.
func grokNamespaces(body []byte) map[string]nsTool {
	var q rRequest
	if json.Unmarshal(body, &q) != nil {
		return nil
	}
	names := map[string]nsTool{}
	for _, t := range q.Tools {
		if t.Type != "namespace" {
			continue
		}
		for _, fn := range t.Tools {
			if fn.Type == "function" {
				names[provider.FlatToolName(t.Name, fn.Name)] = nsTool{Namespace: t.Name, Name: fn.Name}
			}
		}
	}
	return names
}

type namespaceTidy struct {
	buf   []byte
	names map[string]nsTool
}

func (t *namespaceTidy) write(b []byte) []byte {
	t.buf = append(t.buf, b...)
	i := bytes.LastIndexByte(t.buf, '\n')
	if i < 0 {
		return nil
	}
	out := t.lines(t.buf[:i+1])
	t.buf = append(t.buf[:0], t.buf[i+1:]...)
	return out
}

func (t *namespaceTidy) flush() []byte {
	out := t.lines(t.buf)
	t.buf = nil
	return out
}

func (t *namespaceTidy) lines(b []byte) []byte {
	var out []byte
	for len(b) > 0 {
		line := b
		if i := bytes.IndexByte(b, '\n'); i >= 0 {
			line, b = b[:i+1], b[i+1:]
		} else {
			b = nil
		}
		body := bytes.TrimRight(line, "\r\n")
		if data, ok := bytes.CutPrefix(body, []byte("data:")); ok {
			if restored, changed := restoreNamespaces(data, t.names); changed {
				out = append(out, "data: "...)
				out = append(out, restored...)
				out = append(out, line[len(body):]...)
				continue
			}
		}
		out = append(out, line...)
	}
	return out
}

func restoreNamespaces(body []byte, names map[string]nsTool) ([]byte, bool) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var ev map[string]any
	if dec.Decode(&ev) != nil || ev == nil {
		return body, false
	}
	restore := func(it map[string]any) bool {
		if it == nil || it["type"] != "function_call" {
			return false
		}
		name, _ := it["name"].(string)
		ns, ok := names[name]
		if !ok {
			return false
		}
		it["name"], it["namespace"] = ns.Name, ns.Namespace
		return true
	}
	item, _ := ev["item"].(map[string]any)
	changed := restore(item)
	response := ev
	if r, _ := ev["response"].(map[string]any); r != nil {
		response = r
	}
	output, _ := response["output"].([]any)
	for _, it := range output {
		m, _ := it.(map[string]any)
		changed = restore(m) || changed
	}
	if !changed {
		return body, false
	}
	out, err := marshalPlain(ev)
	if err != nil {
		return body, false
	}
	return out, true
}
