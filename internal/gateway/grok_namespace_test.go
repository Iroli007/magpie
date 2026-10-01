package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/provider"
)

func TestGrokNamespaceStreamChunks(t *testing.T) {
	names := grokNamespaces([]byte(`{` + namespacedTools + `}`))
	stream := "event: response.output_item.added\r\n" +
		"data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"function_call\",\"name\":\"collaboration__spawn_agent\",\"call_id\":\"c1\",\"arguments\":\"{}\"}}\r\n\r\n" +
		"data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"function_call\",\"name\":\"collaboration__spawn_agent\",\"call_id\":\"c1\"}}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"type\":\"function_call\",\"name\":\"collaboration__spawn_agent\",\"call_id\":\"c1\"},{\"type\":\"function_call\",\"name\":\"exec_command\"}],\"usage\":{\"input_tokens\":9007199254740993}}}\n\n" +
		"data: [DONE]"
	for _, size := range []int{1, 7, len(stream)} {
		tidy := &namespaceTidy{names: names}
		var out []byte
		for b := []byte(stream); len(b) > 0; {
			n := min(size, len(b))
			out = append(out, tidy.write(b[:n])...)
			b = b[n:]
		}
		out = append(out, tidy.flush()...)
		if strings.Count(string(out), `"namespace":"collaboration"`) != 3 || strings.Contains(string(out), "collaboration__spawn_agent") || !bytes.Contains(out, []byte(`"name":"exec_command"`)) || !bytes.Contains(out, []byte("9007199254740993")) || !bytes.HasSuffix(out, []byte("data: [DONE]")) {
			t.Fatalf("chunk size %d: %s", size, out)
		}
	}
}

func TestGrokNamespacePassthrough(t *testing.T) {
	for _, stream := range []bool{true, false} {
		for _, agent := range []string{"grok", "codex"} {
			t.Run(fmt.Sprintf("%s/stream=%t", agent, stream), func(t *testing.T) {
				fresh(t)
				output := `{"output":[{"type":"function_call","name":"collaboration__spawn_agent","call_id":"c1","arguments":"{}"}]}`
				f := &fake{t: t, reply: output, ctype: "application/json"}
				if stream {
					f.ctype = "text/event-stream"
					f.reply = sse(`data: {"type":"response.output_item.added","item":{"type":"function_call","name":"collaboration__spawn_agent","call_id":"c1"}}`, `data: {"type":"response.completed","response":`+output+`}`)
				}
				up := httptest.NewServer(f)
				defer up.Close()
				p := provider.Provider{ID: "fake", Name: "fake", Key: "k", Responses: up.URL, Account: &provider.Account{Agent: agent}}
				rec := httptest.NewRecorder()
				req := httptest.NewRequest("POST", "/v1/responses", nil)
				status, _, done := New().passthrough(rec, req, p, provider.Responses, "m", []byte(`{"model":"m","input":"hi",`+namespacedTools+`}`), &Usage{})
				if status != 200 || !done {
					t.Fatalf("status=%d done=%v: %s", status, done, rec.Body.String())
				}
				if got := strings.Contains(rec.Body.String(), `"namespace":"collaboration"`); got != (agent == "grok") {
					t.Fatalf("response: %s", rec.Body.String())
				}
				if agent == "codex" {
					var q struct {
						Tools []rTool `json:"tools"`
					}
					if err := json.Unmarshal(f.got, &q); err != nil || len(q.Tools) != 2 || q.Tools[1].Type != "namespace" {
						t.Fatalf("Codex request changed: %s", f.got)
					}
				}
			})
		}
	}
}

func TestGrokNamespaceLongName(t *testing.T) {
	namespace, name := strings.Repeat("n", 40), strings.Repeat("f", 40)
	body := []byte(`{"tools":[{"type":"namespace","name":"` + namespace + `","tools":[{"type":"function","name":"` + name + `"}]}]}`)
	flat := provider.FlatToolName(namespace, name)
	if len(flat) != 64 {
		t.Fatalf("flat name length: %d", len(flat))
	}
	out, changed := restoreNamespaces([]byte(`{"output":[{"type":"function_call","name":"`+flat+`"}]}`), grokNamespaces(body))
	if !changed || !strings.Contains(string(out), `"namespace":"`+namespace+`"`) || !strings.Contains(string(out), `"name":"`+name+`"`) {
		t.Fatalf("output: %s", out)
	}
}

func TestGrokNamespaceAfterAdditionalTools(t *testing.T) {
	body := liftAdditionalTools([]byte(`{"input":[{"type":"additional_tools","tools":[{"type":"namespace","name":"functions","tools":[{"type":"function","name":"exec_command"}]},{"type":"namespace","name":"collaboration","tools":[{"type":"function","name":"spawn_agent"}]}]},{"type":"function_call","namespace":"functions","name":"exec_command","call_id":"c1","arguments":"{}"}]}`))
	names := grokNamespaces(body)
	if len(names) != 1 || names["collaboration__spawn_agent"].Name != "spawn_agent" {
		t.Fatalf("names: %+v", names)
	}
	if strings.Contains(string(body), "functions__exec_command") || !strings.Contains(string(body), `"name":"exec_command"`) {
		t.Fatalf("lifted body: %s", body)
	}
}
