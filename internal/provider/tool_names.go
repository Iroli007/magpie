package provider

import (
	"crypto/sha256"
	"encoding/hex"
)

// FlatToolName is the name a namespaced tool is offered to a model under, which
// takes one flat name: namespace__name, as Codex names an MCP server's tools.
// A name longer than the 64 characters APIs allow is cut and made unique by
// a hash of the whole.
func FlatToolName(namespace, name string) string {
	flat := namespace + "__" + name
	if len(flat) <= 64 {
		return flat
	}
	sum := sha256.Sum256([]byte(namespace + "\x00" + name))
	return flat[:55] + "_" + hex.EncodeToString(sum[:4])
}
