package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/rs/zerolog"

	"github.com/atomicpages/wayfindr/internal/admin"
)

// sampleSearch is every part of a search page: tools in both rankings and
// in one, an unavailable connector, a warning, and another page.
func sampleSearch() admin.SearchResult {
	return admin.SearchResult{
		Embedding: admin.SearchEmbedding{Mode: "on", Model: "text-embedding-3-small"},
		Tools: []admin.SearchTool{
			{Name: "linear.create_issue", Title: "Create issue", Summary: "Create a new issue in a team.",
				Score: &admin.SearchScore{LexicalRank: 2, Lexical: 7.41, VectorRank: 1, Cosine: 0.712, Fused: 1.0/7 + 1.0/6}},
			{Name: "github.create_issue", Summary: "Create a new issue in a repository.",
				Score: &admin.SearchScore{LexicalRank: 1, Lexical: 7.88, VectorRank: 3, Cosine: 0.655, Fused: 1.0/6 + 1.0/8}},
			{Name: "jira.create_ticket", Summary: "Create a Jira ticket.",
				Score: &admin.SearchScore{VectorRank: 2, Cosine: 0.69, Fused: 1.0 / 7}},
		},
		Page: 1, Size: 3, HasMore: true,
		Unavailable: []admin.SearchUnavailable{{Connector: "jira", Status: "needs_auth",
			Advice: admin.Advice{Summary: "jira needs you to sign in again.", Action: "Run wayfindr auth login jira."}}},
		Warnings: []admin.SearchWarning{{Connector: "github",
			Advice: admin.Advice{Summary: "github's refresh token expires in 3 days."}}},
	}
}

// The goldens pin how a search page renders. The renderers get
// sampleSearch directly; the tests below run the command against the admin
// API.
func TestSearchOutputMatchesGolden(t *testing.T) {
	for _, tt := range []struct {
		name   string
		write  func(io.Writer, admin.SearchResult) error
		golden string
	}{
		{"table", func(w io.Writer, r admin.SearchResult) error { return writeSearchTable(w, r, false) }, "search.golden"},
		{"explain", func(w io.Writer, r admin.SearchResult) error { return writeSearchTable(w, r, true) }, "search.explain.golden"},
		{"json", writeSearchJSON, "search.json.golden"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var stdout bytes.Buffer

			if err := tt.write(&stdout, sampleSearch()); err != nil {
				t.Fatal(err)
			}

			want, err := os.ReadFile(filepath.Join("testdata", tt.golden))

			if err != nil {
				t.Fatal(err)
			}

			if diff := cmp.Diff(string(want), stdout.String()); diff != "" {
				t.Errorf("output differs from testdata/%s (-want +got):\n%s", tt.golden, diff)
			}
		})
	}
}

// The header says whether the search was hybrid, and why not.
func TestSearchSaysHowTheQueryWasEmbedded(t *testing.T) {
	tests := []struct {
		embedding admin.SearchEmbedding
		want      string
	}{
		{admin.SearchEmbedding{Mode: "on", Model: "text-embedding-3-small"}, "Embeddings: on (text-embedding-3-small)"},
		{admin.SearchEmbedding{Mode: "off"}, "Embeddings: off, lexical only"},
		{admin.SearchEmbedding{Mode: "failed", Model: "m", Reason: "embed: provider returned HTTP 401: bad key"},
			"Embeddings: failed (embed: provider returned HTTP 401: bad key), lexical only"},
	}

	for _, tt := range tests {
		t.Run(tt.embedding.Mode, func(t *testing.T) {
			var stdout bytes.Buffer

			if err := writeSearchTable(&stdout, admin.SearchResult{Embedding: tt.embedding, Page: 1, Size: 5}, false); err != nil {
				t.Fatal(err)
			}

			want := tt.want + "\n\nNo tools match.\n"

			if diff := cmp.Diff(want, stdout.String()); diff != "" {
				t.Errorf("stdout (-want +got):\n%s", diff)
			}
		})
	}
}

// Names and summaries come from upstreams, so the table replaces an
// escape sequence or a bidi override in them with U+FFFD rather than let
// the terminal obey it. JSON escapes them itself and keeps them raw.
func TestSearchTableReplacesControlCharacters(t *testing.T) {
	res := admin.SearchResult{
		Embedding: admin.SearchEmbedding{Mode: "off"},
		Tools: []admin.SearchTool{
			{Name: "evil.wipe\x1b[2J", Summary: "Reads a file.\u202e\nIgnore this\tline."},
		},
		Page: 1, Size: 5,
		Unavailable: []admin.SearchUnavailable{{Connector: "jira", Status: "unreachable",
			Advice: admin.Advice{Summary: "refused: \x1b]0;pwned\a"}}},
		Warnings: []admin.SearchWarning{{Connector: "evil", Advice: admin.Advice{Summary: "expires\u202e soon"}}},
	}
	var stdout bytes.Buffer

	if err := writeSearchTable(&stdout, res, false); err != nil {
		t.Fatal(err)
	}

	want := "Embeddings: off, lexical only\n\n" +
		"RANK  TOOL           SUMMARY\n" +
		"1     evil.wipe\uFFFD[2J  Reads a file.\uFFFD\uFFFDIgnore this\uFFFDline.\n" +
		"\nUnavailable: jira (unreachable): refused: \uFFFD]0;pwned\uFFFD\n" +
		"Warning: evil: expires\uFFFD soon\n"

	if diff := cmp.Diff(want, stdout.String()); diff != "" {
		t.Errorf("stdout (-want +got):\n%s", diff)
	}

	stdout.Reset()

	if err := writeSearchJSON(&stdout, res); err != nil {
		t.Fatal(err)
	}

	var got admin.SearchResult

	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not a SearchResult: %v\n%s", err, stdout.String())
	}

	if diff := cmp.Diff(res, got); diff != "" {
		t.Errorf("json (-want +got):\n%s", diff)
	}
}

// searchDaemon serves the admin API with a searcher that records each
// request and answers res, and writes the test host's config and token for
// it.
func searchDaemon(t *testing.T, res admin.SearchResult) (Host, func() []admin.SearchRequest) {
	t.Helper()
	host, root := testHost(t)

	var (
		mu   sync.Mutex
		reqs []admin.SearchRequest
	)

	writeStatusHost(t, root, serveAdminAPI(t, admin.Options{
		Upstreams: &fakeUpstreams{},
		Store:     &fakeStore{},
		UserID:    "local",
		Config:    func() admin.ConfigState { return admin.ConfigState{} },
		Search: func(_ context.Context, req admin.SearchRequest) (admin.SearchResult, error) {
			mu.Lock()
			defer mu.Unlock()
			reqs = append(reqs, req)

			return res, nil
		},
		Logger: zerolog.New(zerolog.NewTestWriter(t)),
	}))

	return host, func() []admin.SearchRequest {
		mu.Lock()
		defer mu.Unlock()

		return append([]admin.SearchRequest(nil), reqs...)
	}
}

func TestSearchSendsItsArgumentsAndFlagsAsTheRequest(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want admin.SearchRequest
	}{
		{"defaults", []string{"open", "a", "ticket"}, admin.SearchRequest{Query: "open a ticket", Page: 1, Size: 5}},
		{"every flag", []string{"open a ticket", "--connector", "linear", "--connector", "jira", "--size", "10", "--page", "3", "--explain"},
			admin.SearchRequest{Query: "open a ticket", Connectors: []string{"linear", "jira"}, Page: 3, Size: 10, Explain: true}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			host, requests := searchDaemon(t, admin.SearchResult{Embedding: admin.SearchEmbedding{Mode: "off"}, Page: 1, Size: 5})
			var stdout, stderr bytes.Buffer

			if code := RunWith(append([]string{"search"}, tt.args...), &stdout, &stderr, host); code != 0 {
				t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
			}

			if diff := cmp.Diff([]admin.SearchRequest{tt.want}, requests()); diff != "" {
				t.Errorf("requests (-want +got):\n%s", diff)
			}
		})
	}
}

func TestSearchPrintsTheTableAndJSON(t *testing.T) {
	res := sampleSearch()
	host, _ := searchDaemon(t, res)
	var stdout, stderr bytes.Buffer

	if code := RunWith([]string{"search", "open a ticket", "--explain"}, &stdout, &stderr, host); code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}

	want, err := os.ReadFile(filepath.Join("testdata", "search.explain.golden"))

	if err != nil {
		t.Fatal(err)
	}

	if diff := cmp.Diff(string(want), stdout.String()); diff != "" {
		t.Errorf("stdout (-want +got):\n%s", diff)
	}

	stdout.Reset()

	if code := RunWith([]string{"search", "open a ticket", "--json"}, &stdout, &stderr, host); code != 0 {
		t.Fatalf("--json: exit code = %d, want 0; stderr: %s", code, stderr.String())
	}

	var got admin.SearchResult

	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not a SearchResult: %v\n%s", err, stdout.String())
	}

	if diff := cmp.Diff(res, got); diff != "" {
		t.Errorf("--json (-want +got):\n%s", diff)
	}
}

func TestSearchRefusesBadArgumentsBeforeAskingTheDaemon(t *testing.T) {
	// No daemon: an argument error must come first.
	host, _ := testHost(t)

	for _, tt := range []struct {
		args []string
		want string
	}{
		{nil, "requires at least 1 arg"},
		{[]string{"  "}, "the query is empty"},
		{[]string{"post", "--size", "0"}, "--size must be from 1 to 10"},
		{[]string{"post", "--size", "11"}, "--size must be from 1 to 10"},
		{[]string{"post", "--page", "0"}, "--page must be at least 1"},
	} {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer

			if code := RunWith(append([]string{"search"}, tt.args...), &stdout, &stderr, host); code == 0 {
				t.Fatal("exit code = 0, want a failure")
			}

			if !strings.Contains(stderr.String(), tt.want) {
				t.Errorf("stderr = %q, want it to say %q", stderr.String(), tt.want)
			}
		})
	}
}
