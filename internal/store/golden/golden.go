// Package golden is the search golden fixture (design §10, search quality):
// about 200 real-style tool definitions, queries with hand-written top-5
// expectations, and, once generated, real embeddings of both. It belongs to
// no driver: SQLite's golden tests and the cross-driver parity tests load it
// from here. README.md has its provenance and the meaning of each query's
// expectations.
package golden

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"path"
	"slices"
	"strings"
	"testing"

	"github.com/atomicpages/wayfindr/internal/catalog"
	"github.com/atomicpages/wayfindr/internal/store"
)

// fixture holds the tool definitions, the query files, and any vectors
// files. A vectors file appears here once TestGenerateHybridGolden writes it
// next to this package's source.
//
//go:embed tools/*.json *.json
var fixture embed.FS

// Query is one entry of queries.json or hybrid_queries.json. First, when
// set, must be the top result; every name in Top5 must be among the top
// five.
type Query struct {
	Kind       string   `json:"kind"`
	Query      string   `json:"query"`
	Connectors []string `json:"connectors"`
	First      string   `json:"first"`
	Top5       []string `json:"top5"`
}

// Failures lists how got, a first page of qualified names, misses q's
// expectations; nil means it meets them.
func (q Query) Failures(got []string) []string {
	var out []string

	if q.First != "" && (len(got) == 0 || got[0] != q.First) {
		out = append(out, "first = "+strings.Join(got, ", ")+"; want "+q.First)
	}

	for _, want := range q.Top5 {
		if !slices.Contains(got, want) {
			out = append(out, "top 5 = "+strings.Join(got, ", ")+"; missing "+want)
		}
	}

	return out
}

// Expected is every name q expects in the top five, First included.
func (q Query) Expected() []string {
	out := slices.Clone(q.Top5)

	if q.First != "" && !slices.Contains(out, q.First) {
		out = append(out, q.First)
	}

	return out
}

// SearchQuery is q as a lexical search for the first page of five.
func (q Query) SearchQuery(userID string) store.SearchQuery {
	return store.SearchQuery{UserID: userID, Text: q.Query, Connectors: q.Connectors, Page: 1, Size: 5}
}

// Tools reads the fixture's tool definitions, one connector per file, keyed
// by connector.
func Tools(t testing.TB) map[string][]store.Tool {
	t.Helper()
	files, err := fs.Glob(fixture, "tools/*.json")

	if err != nil || len(files) == 0 {
		t.Fatalf("glob golden tools: %v (%d files)", err, len(files))
	}

	out := map[string][]store.Tool{}

	for _, f := range files {
		connector := strings.TrimSuffix(path.Base(f), ".json")
		var defs []json.RawMessage
		readJSON(t, f, &defs)

		for _, d := range defs {
			tl, err := store.NewTool(connector, d, []byte(d))

			if err != nil {
				t.Fatalf("%s: NewTool: %v", f, err)
			}

			out[connector] = append(out[connector], tl)
		}
	}

	return out
}

// Queries reads queries.json: expectations lexical search must meet.
func Queries(t testing.TB) []Query {
	t.Helper()
	var qs []Query
	readJSON(t, "queries.json", &qs)

	return qs
}

// HybridQueries reads hybrid_queries.json: expectations that need query
// vectors, which lexical search alone cannot meet.
func HybridQueries(t testing.TB) []Query {
	t.Helper()
	var qs []Query
	readJSON(t, "hybrid_queries.json", &qs)

	return qs
}

// Load stores the fixture's tools as userID's catalog in s and returns them
// by connector.
func Load(t testing.TB, s store.Store, userID string) map[string][]store.Tool {
	t.Helper()
	tools := Tools(t)

	for connector, ts := range tools {
		if _, err := s.ApplySync(context.Background(), userID, store.SyncOutcome{Connector: connector, Tools: ts}); err != nil {
			t.Fatalf("ApplySync %s: %v", connector, err)
		}
	}

	return tools
}

// TopFive runs q on s and returns the first page's qualified names in rank
// order.
func TopFive(t testing.TB, s store.Store, q store.SearchQuery) []string {
	t.Helper()
	page, err := s.Search(context.Background(), q)

	if err != nil {
		t.Fatalf("Search(%q): %v", q.Text, err)
	}

	names := []string{}

	for _, tl := range page.Tools {
		names = append(names, tl.Connector+"."+tl.Name)
	}

	return names
}

// Vectors is a vectors-<model id>.json file: real embeddings, made by one
// model, of every fixture tool's embed text and every golden query.
type Vectors struct {
	ModelID string `json:"model_id"`
	// Model and Dimensions are informational; ModelID is the identity.
	Model      string                `json:"model"`
	Dimensions int                   `json:"dimensions"`
	Tools      map[string]ToolVector `json:"tools"`   // by qualified name
	Queries    map[string][]float32  `json:"queries"` // by query text
}

// ToolVector is one tool's vector, with the sha256 of the embed text it was
// made from, so a fixture edit that changes the text is caught.
type ToolVector struct {
	TextSHA256 string    `json:"text_sha256"`
	Vector     []float32 `json:"vector"`
}

// EmbedTextHash is the hex sha256 of tl's embed text (catalog.EmbedText).
func EmbedTextHash(tl store.Tool) string {
	sum := sha256.Sum256([]byte(catalog.EmbedText(tl)))

	return hex.EncodeToString(sum[:])
}

// VectorsFileName is the fixture file that holds modelID's vectors.
func VectorsFileName(modelID string) string { return "vectors-" + modelID + ".json" }

// VectorsFiles names the fixture's vectors files, in name order. There are
// none until TestGenerateHybridGolden makes one (wayfindr-mvg.4).
func VectorsFiles(t testing.TB) []string {
	t.Helper()
	files, err := fs.Glob(fixture, VectorsFileName("*"))

	if err != nil {
		t.Fatal(err)
	}

	return files
}

// LoadVectors reads the named vectors file.
func LoadVectors(t testing.TB, name string) Vectors {
	t.Helper()
	var v Vectors
	readJSON(t, name, &v)

	return v
}

// Embed stores v's vector for each of tools (by connector, as Load returns
// them) as userID's embedding under v.ModelID. It fails t, asking for a
// regeneration, when a tool's current embed text has no vector.
func (v Vectors) Embed(t testing.TB, s store.Store, userID string, tools map[string][]store.Tool) {
	t.Helper()
	var tvs []store.ToolVector

	for connector, ts := range tools {
		for _, tl := range ts {
			name := connector + "." + tl.Name
			tv, ok := v.Tools[name]

			if !ok || tv.TextSHA256 != EmbedTextHash(tl) {
				t.Fatalf("%s has no vector for the current text of %s: regenerate it (TestGenerateHybridGolden)", VectorsFileName(v.ModelID), name)
			}

			tvs = append(tvs, store.ToolVector{Connector: connector, Name: tl.Name, ContentHash: tl.ContentHash, Vector: tv.Vector})
		}
	}

	if err := s.SetEmbeddings(context.Background(), userID, v.ModelID, tvs); err != nil {
		t.Fatalf("SetEmbeddings: %v", err)
	}
}

// QueryVector is the vector of query text. It fails t, asking for a
// regeneration, when there is none.
func (v Vectors) QueryVector(t testing.TB, text string) []float32 {
	t.Helper()
	vec, ok := v.Queries[text]

	if !ok {
		t.Fatalf("%s has no vector for query %q: regenerate it (TestGenerateHybridGolden)", VectorsFileName(v.ModelID), text)
	}

	return vec
}

// readJSON parses the fixture file name into v.
func readJSON(t testing.TB, name string, v any) {
	t.Helper()
	raw, err := fixture.ReadFile(name)

	if err != nil {
		t.Fatalf("read golden %s: %v", name, err)
	}

	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("parse golden %s: %v", name, err)
	}
}
