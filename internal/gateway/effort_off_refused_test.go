package gateway

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/yetone/magpie/internal/catalog"
	"github.com/yetone/magpie/internal/provider"
)

// Claude Code's auto mode classifier on Command Code's DeepSeek V4.1 Flash
// Fast through its plugin (#394): its levels borrowed from those listing it
// (none, low, high, max), the classifier asked for none, which Command Code
// turns away naming the levels it takes. Asked again at low, and at low
// from then on.
func TestTranslatedEffortNoneRefused(t *testing.T) {
	var sent []any
	f := &fake{t: t, reply: sse(
		`data: {"id":"c1","model":"m1","choices":[{"delta":{"role":"assistant","content":"<block>no"}}]}`,
		`data: {"id":"c1","choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`data: [DONE]`)}
	f.refuse = func(b []byte) (int, string) {
		var v map[string]any
		json.Unmarshal(b, &v)
		sent = append(sent, v["reasoning_effort"])
		if e, ok := v["reasoning_effort"].(string); ok && !slices.Contains([]string{"low", "medium", "high", "xhigh", "max"}, e) {
			return 400, `{"error":{"message":"Invalid option: expected one of \"low\"|\"medium\"|\"high\"|\"xhigh\"|\"max\"","type":"invalid_request_error"}}`
		}
		return 0, ""
	}
	up := setup(t, provider.Chat, f)
	if err := catalog.SaveLive("fake", up.URL+"/v1", []catalog.Model{{ID: "m1", Context: 128000, Efforts: []string{"none", "low", "high", "max"}}}); err != nil {
		t.Fatal(err)
	}
	srv := New()
	for turn, want := range [][]any{{"none", "low"}, {"low"}} {
		sent = nil
		code, body := postTo(t, srv, "/v1/messages", autoModeAsk("m1"))
		if code != 200 || !saidNo(body) {
			t.Fatalf("turn %d: %d %s", turn+1, code, body)
		}
		if !slices.Equal(sent, want) {
			t.Errorf("turn %d: efforts sent %v, want %v", turn+1, sent, want)
		}
	}
}
