package catalog

import (
	"crypto/sha256"
	"strings"

	"github.com/atomicpages/wayfindr/internal/store"
)

// EmbedText is the text a catalog entry is embedded from (design §7.4):
// "<connector>.<name>", the title, and the description, one per line, then
// one "<param>: <param description>" line per top-level input parameter in
// name order. Every line ends in a newline. t.Params must already be sorted,
// as store.NewTool leaves them, so the text never depends on the order of the
// definition's JSON keys.
func EmbedText(t store.Tool) string {
	var b strings.Builder
	b.WriteString(t.Connector + "." + t.Name + "\n")
	b.WriteString(t.Title + "\n")
	b.WriteString(t.Description + "\n")

	for _, p := range t.Params {
		b.WriteString(p.Name + ": " + p.Description + "\n")
	}

	return b.String()
}

// contentHash is the sha256 of a catalog entry's embed text.
func contentHash(t store.Tool) []byte {
	sum := sha256.Sum256([]byte(EmbedText(t)))

	return sum[:]
}
