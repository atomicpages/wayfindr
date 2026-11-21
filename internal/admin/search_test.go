package admin_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/atomicpages/wayfindr/internal/admin"
)

func TestSearchPassesTheRequestAndReturnsTheResult(t *testing.T) {
	st, user := newStore(t)
	opts := options(t, st, user, &fakeUpstreams{}, admin.ConfigState{})

	var got admin.SearchRequest

	want := admin.SearchResult{
		Embedding: admin.SearchEmbedding{Mode: "on", Model: "text-embedding-3-small"},
		Tools: []admin.SearchTool{{
			Name: "linear.create_issue", Title: "Create issue", Summary: "Create an issue.",
			Score: &admin.SearchScore{LexicalRank: 2, Lexical: 7.41, VectorRank: 1, Cosine: 0.712, Fused: 1.0/7 + 1.0/6},
		}},
		Page: 2, Size: 1, HasMore: true,
		Unavailable: []admin.SearchUnavailable{{Connector: "jira", Status: "needs_auth", Advice: admin.Advice{Summary: "jira needs you to sign in."}}},
		Warnings:    []admin.SearchWarning{},
	}
	opts.Search = func(_ context.Context, req admin.SearchRequest) (admin.SearchResult, error) {
		got = req

		return want, nil
	}
	c, _ := serve(t, opts)

	req := admin.SearchRequest{Query: "open a ticket", Connectors: []string{"linear", "jira"}, Page: 2, Size: 1, Explain: true}
	res, err := c.Search(t.Context(), req)

	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	if diff := cmp.Diff(req, got); diff != "" {
		t.Errorf("request (-sent +received):\n%s", diff)
	}

	if diff := cmp.Diff(want, res); diff != "" {
		t.Errorf("result (-want +got):\n%s", diff)
	}
}

// tools, unavailable, and warnings are arrays even when the searcher
// leaves them nil, so `wayfindr search --json` never prints null.
func TestSearchAnswersEmptyListsAsArrays(t *testing.T) {
	st, user := newStore(t)
	opts := options(t, st, user, &fakeUpstreams{}, admin.ConfigState{})
	opts.Search = func(context.Context, admin.SearchRequest) (admin.SearchResult, error) {
		return admin.SearchResult{Embedding: admin.SearchEmbedding{Mode: "off"}, Page: 1, Size: 5}, nil
	}
	_, srv := serve(t, opts)

	status, body := postSearch(t, srv, `{"query":"zzzz"}`)

	if status != http.StatusOK {
		t.Fatalf("status %d: %s", status, body)
	}

	wantJSON := `{"embedding":{"mode":"off"},"tools":[],"page":1,"size":5,"has_more":false,"unavailable":[],"warnings":[]}`

	if got := strings.TrimSpace(body); got != wantJSON {
		t.Errorf("body = %s\nwant   %s", got, wantJSON)
	}
}

func TestSearchRefusals(t *testing.T) {
	tests := []struct {
		name       string
		search     func(context.Context, admin.SearchRequest) (admin.SearchResult, error)
		body       string
		wantStatus int
		wantBody   string
	}{
		{
			name:       "no searcher",
			body:       `{"query":"post"}`,
			wantStatus: http.StatusNotImplemented,
			wantBody:   "search is not available",
		},
		{
			name: "refused by the gateway",
			search: func(context.Context, admin.SearchRequest) (admin.SearchResult, error) {
				return admin.SearchResult{}, &admin.SearchError{
					Kind: "invalid_search", Case: "unknown_connector",
					Advice: admin.Advice{
						Summary: "No connector is called jira. Configured connectors: linear.",
						Action:  "Use one of those names, or omit connectors to search them all.",
					},
				}
			},
			body:       `{"query":"post","connectors":["jira"]}`,
			wantStatus: http.StatusBadRequest,
			wantBody:   "No connector is called jira. Configured connectors: linear.",
		},
		{
			name: "failed",
			search: func(context.Context, admin.SearchRequest) (admin.SearchResult, error) {
				return admin.SearchResult{}, errors.New("catalog: disk I/O error")
			},
			body:       `{"query":"post"}`,
			wantStatus: http.StatusInternalServerError,
			wantBody:   "search failed; see the daemon log",
		},
		{
			name: "unknown field",
			search: func(context.Context, admin.SearchRequest) (admin.SearchResult, error) {
				return admin.SearchResult{}, nil
			},
			body:       `{"query":"post","detail":"brief"}`,
			wantStatus: http.StatusBadRequest,
			wantBody:   `body must be {"query": "…"} with optional connectors, page, size, and explain`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st, user := newStore(t)
			opts := options(t, st, user, &fakeUpstreams{}, admin.ConfigState{})
			opts.Search = tt.search
			_, srv := serve(t, opts)

			status, body := postSearch(t, srv, tt.body)

			if status != tt.wantStatus || strings.TrimSpace(body) != tt.wantBody {
				t.Errorf("got %d %q, want %d %q", status, strings.TrimSpace(body), tt.wantStatus, tt.wantBody)
			}
		})
	}
}

// postSearch sends body to POST /admin/search as is and returns the status
// and the response body.
func postSearch(t *testing.T, srv *httptest.Server, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/admin/search", strings.NewReader(body))

	if err != nil {
		t.Fatal(err)
	}

	resp, err := srv.Client().Do(req)

	if err != nil {
		t.Fatal(err)
	}

	b, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if err != nil {
		t.Fatal(err)
	}

	return resp.StatusCode, string(b)
}
