package plugin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The list built in is what a magpie that can't reach the market shows:
// every entry a real package name, said in English and Chinese, once.
func TestBuiltinMarket(t *testing.T) {
	l, err := parseMarket(builtinMarket)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, x := range l {
		if seen[x.Package] {
			t.Errorf("%s listed twice", x.Package)
		}
		seen[x.Package] = true
		if x.Summary["en"] == "" || x.Summary["zh"] == "" {
			t.Errorf("%s: summary needs en and zh", x.Package)
		}
		if !x.Community {
			t.Errorf("%s: only magpie's community's plugins are listed", x.Package)
		}
		if len(x.Providers) == 0 || x.Icon == "" {
			t.Errorf("%s: needs providers and an icon", x.Package)
		}
	}
	if len(l) < 10 {
		t.Errorf("%d listed", len(l))
	}
}

// The market is fetched; one that fails is the copy fetched last, and
// with none, the one built in. "off" never asks.
func TestMarketSources(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	reset := func() { marketMu.Lock(); marketList = nil; marketMu.Unlock() }
	up := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !up {
			http.Error(w, "down", 500)
			return
		}
		w.Write([]byte(`{"version":1,"plugins":[{"package":"opencode-x-auth","name":"X","community":true},{"package":"not a name","community":true},{"package":"opencode-other-auth","name":"Other"}]}`))
	}))
	defer srv.Close()
	t.Setenv("MAGPIE_PLUGIN_MARKET", srv.URL)
	ctx := context.Background()

	reset()
	if l := Market(ctx); len(l) != 1 || l[0].Name != "X" {
		t.Fatalf("fetched: %+v", l)
	}
	up = false
	reset()
	if l := Market(ctx); len(l) != 1 || l[0].Package != "opencode-x-auth" {
		t.Fatalf("down, the copy kept: %+v", l)
	}
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	reset()
	builtin, _ := parseMarket(builtinMarket)
	if l := Market(ctx); len(l) != len(builtin) {
		t.Fatalf("down, nothing kept: %d, want the %d built in", len(l), len(builtin))
	}
	up = true
	t.Setenv("MAGPIE_PLUGIN_MARKET", "off")
	reset()
	if l := Market(ctx); len(l) != len(builtin) {
		t.Fatalf("off: %d", len(l))
	}
	reset()
}

// A plugin's provider shows its vendor's icon: the one its package is
// listed with, else that of a listed plugin for the same provider.
func TestMarketIcon(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	marketMu.Lock()
	marketList = nil
	marketMu.Unlock()
	for _, c := range []struct{ spec, id, want string }{
		{"@cognitionai/opencode-devin@0.3.1", "devin", "devin"},
		{"opencode-gemini-auth@2.0.1", "google", "gemini-color"},
		{"opencode-copilot-auth", "github-copilot", "githubcopilot"},
		{"some-unlisted-plugin", "anthropic", "claude-color"},
		{"opencode-antigravity-auth", "google", "antigravity-color"},
		{"/Users/me/plugins/packages/zed", "zed", "zed"},
		{"some-unlisted-plugin", "commandcode-plan", "commandcode"},
		{"some-unlisted-plugin", "nobody", ""},
	} {
		if got := Icon(c.spec, c.id); got != c.want {
			t.Errorf("Icon(%q, %q) = %q, want %q", c.spec, c.id, got, c.want)
		}
	}
}
