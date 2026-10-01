package qoder

import "testing"

// The listing marks a model free with is_free or a price_factor of 0, in
// snake or camel case; Qoder's client shows either as free.
func TestParseModelsFree(t *testing.T) {
	body := []byte(`{"chat":[
		{"key":"flag","enable":true,"is_free":true,"price_factor":1},
		{"key":"camel-flag","enable":true,"isFree":true},
		{"key":"zero","enable":true,"price_factor":0},
		{"key":"camel-zero","enable":true,"priceFactor":0},
		{"key":"paid","enable":true,"is_free":false,"price_factor":0.5},
		{"key":"unsaid","enable":true}
	]}`)
	ms, err := ParseModels(body, ProviderKey)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"flag": true, "camel-flag": true, "zero": true, "camel-zero": true, "paid": false, "unsaid": false}
	if len(ms) != len(want) {
		t.Fatalf("models %+v", ms)
	}
	for _, m := range ms {
		if m.Free != want[m.ID] {
			t.Errorf("%s: Free = %v, want %v", m.ID, m.Free, want[m.ID])
		}
	}
}
