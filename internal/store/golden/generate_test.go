package golden_test

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"maps"
	"net/http"
	"os"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/atomicpages/wayfindr/internal/catalog"
	"github.com/atomicpages/wayfindr/internal/embed"
	"github.com/atomicpages/wayfindr/internal/store/golden"
)

// update regenerates the golden vectors; see TestGenerateHybridGolden.
var update = flag.Bool("update", false, "regenerate vectors-<model id>.json from a live embedding provider")

// TestGenerateHybridGolden writes vectors-<model id>.json into this package
// from a live embedding provider, configured by the same variables as
// internal/embed's live test:
//
//	WAYFINDR_EMBED_LIVE_URL=http://localhost:11434/v1 \
//	WAYFINDR_EMBED_LIVE_MODEL=qwen3-embedding:0.6b \
//	WAYFINDR_EMBED_LIVE_DIMENSIONS=1024 \
//	go test ./internal/store/golden -run TestGenerateHybridGolden -count=1 -args -update
//
// (plus WAYFINDR_EMBED_LIVE_KEY for a provider that needs one). Rerun it
// whenever a fixture tool or query changes.
func TestGenerateHybridGolden(t *testing.T) {
	if !*update {
		t.Skip("run with -args -update to regenerate the golden vectors")
	}

	base := os.Getenv("WAYFINDR_EMBED_LIVE_URL")

	if base == "" {
		t.Fatal("-update needs WAYFINDR_EMBED_LIVE_URL, _MODEL, _DIMENSIONS, and optionally _KEY")
	}

	dims, err := strconv.Atoi(os.Getenv("WAYFINDR_EMBED_LIVE_DIMENSIONS"))

	if err != nil {
		t.Fatalf("WAYFINDR_EMBED_LIVE_DIMENSIONS: %v", err)
	}

	cfg := embed.Config{
		BaseURL:    base,
		Model:      os.Getenv("WAYFINDR_EMBED_LIVE_MODEL"),
		Dimensions: dims,
		APIKey:     os.Getenv("WAYFINDR_EMBED_LIVE_KEY"),
	}
	c, err := embed.New(cfg, &http.Client{}, zerolog.New(zerolog.NewTestWriter(t)))

	if err != nil {
		t.Fatalf("embed.New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	var names, texts, hashes []string
	tools := golden.Tools(t)

	for _, connector := range slices.Sorted(maps.Keys(tools)) {
		for _, tl := range tools[connector] {
			names = append(names, connector+"."+tl.Name)
			texts = append(texts, catalog.EmbedText(tl))
			hashes = append(hashes, golden.EmbedTextHash(tl))
		}
	}

	var queries []string

	for _, q := range slices.Concat(golden.Queries(t), golden.HybridQueries(t)) {
		if !slices.Contains(queries, q.Query) {
			queries = append(queries, q.Query)
		}
	}

	vecs, err := c.Embed(ctx, slices.Concat(texts, queries))

	if err != nil {
		t.Fatalf("Embed: %v", err)
	}

	gv := golden.Vectors{
		ModelID:    c.ModelID(),
		Model:      cfg.Model,
		Dimensions: dims,
		Tools:      map[string]golden.ToolVector{},
		Queries:    map[string][]float32{},
	}

	for i, name := range names {
		gv.Tools[name] = golden.ToolVector{TextSHA256: hashes[i], Vector: vecs[i]}
	}

	for i, q := range queries {
		gv.Queries[q] = vecs[len(names)+i]
	}

	out, err := marshalVectors(gv)

	if err != nil {
		t.Fatal(err)
	}

	// go test runs in this package's directory, where the fixture is embedded
	// from.
	path := golden.VectorsFileName(gv.ModelID)

	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatal(err)
	}

	t.Logf("wrote internal/store/golden/%s: %d tools, %d queries", path, len(names), len(queries))
}

// marshalVectors writes gv as JSON with one tool or query per line, in key
// order, so a regenerated file diffs line by line.
func marshalVectors(gv golden.Vectors) ([]byte, error) {
	var b bytes.Buffer
	field := func(key string, v any, last bool) error {
		k, _ := json.Marshal(key)
		val, err := json.Marshal(v)

		if err != nil {
			return err
		}

		b.Write(k)
		b.WriteString(": ")
		b.Write(val)

		if !last {
			b.WriteByte(',')
		}

		b.WriteByte('\n')

		return nil
	}
	b.WriteString("{\n")

	if err := field("model_id", gv.ModelID, false); err != nil {
		return nil, err
	}

	if err := field("model", gv.Model, false); err != nil {
		return nil, err
	}

	if err := field("dimensions", gv.Dimensions, false); err != nil {
		return nil, err
	}

	b.WriteString("\"tools\": {\n")
	toolNames := slices.Sorted(maps.Keys(gv.Tools))

	for i, name := range toolNames {
		if err := field(name, gv.Tools[name], i == len(toolNames)-1); err != nil {
			return nil, err
		}
	}

	b.WriteString("},\n\"queries\": {\n")
	queries := slices.Sorted(maps.Keys(gv.Queries))

	for i, q := range queries {
		if err := field(q, gv.Queries[q], i == len(queries)-1); err != nil {
			return nil, err
		}
	}

	b.WriteString("}\n}\n")

	return b.Bytes(), nil
}
