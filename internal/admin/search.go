package admin

import (
	"context"
	"errors"
	"net/http"
)

// maxSearchBody bounds the body of POST /admin/search: a query and a few
// connector names.
const maxSearchBody = 16 << 10

// SearchRequest is the body of POST /admin/search: a search_tools query,
// detail full, run for a person.
type SearchRequest struct {
	Query string `json:"query"`
	// Connectors limits the search to these connectors; empty searches all.
	Connectors []string `json:"connectors,omitempty"`
	// Page and Size are as in search_tools; 0 means its default.
	Page int `json:"page,omitempty"`
	Size int `json:"size,omitempty"`
	// Explain adds each tool's score.
	Explain bool `json:"explain,omitempty"`
}

// SearchResult is the body of POST /admin/search: the page search_tools
// would return for the same query, and how the query was embedded.
type SearchResult struct {
	Embedding SearchEmbedding `json:"embedding"`
	// Tools are the results in rank order. Always an array.
	Tools   []SearchTool `json:"tools"`
	Page    int          `json:"page"`
	Size    int          `json:"size"`
	HasMore bool         `json:"has_more"`
	// Unavailable are the searched connectors that are not ready. Always an
	// array.
	Unavailable []SearchUnavailable `json:"unavailable"`
	// Warnings are failures ahead for ready connectors searched, as
	// search_tools lists them. Always an array.
	Warnings []SearchWarning `json:"warnings"`
}

// SearchEmbedding says how the query was embedded.
type SearchEmbedding struct {
	// Mode is "on" (hybrid search), "off" (no embedding provider), or
	// "failed" (the provider failed or timed out). Off and failed searches
	// are lexical only.
	Mode string `json:"mode"`
	// Model is embedding.model; absent when Mode is "off".
	Model string `json:"model,omitempty"`
	// Reason is why the embedding failed; present only when Mode is
	// "failed".
	Reason string `json:"reason,omitempty"`
}

// SearchTool is one ranked tool.
type SearchTool struct {
	// Name is the qualified name, <connector>.<tool>.
	Name    string `json:"name"`
	Title   string `json:"title,omitempty"`
	Summary string `json:"summary,omitempty"`
	// Score is present only when the request asked for explain.
	Score *SearchScore `json:"score,omitempty"`
}

// SearchScore is why a tool ranked where it did. A rank counts from 1 over
// the whole catalog, before the connector filter; 0 means the tool is not
// in that ranking, and its score is then 0.
type SearchScore struct {
	LexicalRank int     `json:"lexical_rank"`
	Lexical     float64 `json:"lexical"` // BM25F
	VectorRank  int     `json:"vector_rank"`
	Cosine      float64 `json:"cosine"`
	Fused       float64 `json:"fused"` // Σ 1/(5 + rank)
}

// SearchUnavailable is a connector that is not ready, with what to do
// about it.
type SearchUnavailable struct {
	Connector string `json:"connector"`
	Status    string `json:"status"`
	Advice
}

// SearchWarning is a failure ahead for a ready connector (GLOSSARY:
// Warning).
type SearchWarning struct {
	Connector string `json:"connector"`
	Advice
}

// SearchError is a search the gateway refused, such as one naming a
// connector that is not configured: the gateway's Kind and Case and its
// advice. The daemon copies them and words nothing. It is answered as a
// 400 with the advice's summary, which names what is wrong; the action is
// worded for search_tools' arguments, not the CLI's flags.
type SearchError struct {
	Kind, Case string
	Advice     Advice
}

// Error is technical text for logs: the Kind and Case. What the error
// means to the user is its Advice.
func (e *SearchError) Error() string { return "search: " + e.Kind + "/" + e.Case }

// search serves POST /admin/search.
func (a api) search(w http.ResponseWriter, r *http.Request) {
	if a.Search == nil {
		http.Error(w, "search is not available", http.StatusNotImplemented)

		return
	}

	var req SearchRequest

	if err := readJSON(w, r, maxSearchBody, &req); err != nil {
		if mbe := (*http.MaxBytesError)(nil); errors.As(err, &mbe) {
			http.Error(w, "search request is too large", http.StatusRequestEntityTooLarge)

			return
		}

		http.Error(w, `body must be {"query": "…"} with optional connectors, page, size, and explain`, http.StatusBadRequest)

		return
	}

	res, err := a.Search(r.Context(), req)

	if se := (*SearchError)(nil); errors.As(err, &se) {
		http.Error(w, se.Advice.Summary, http.StatusBadRequest)

		return
	}

	if err != nil {
		a.Logger.Error().Err(err).Msg("admin search")
		http.Error(w, "search failed; see the daemon log", http.StatusInternalServerError)

		return
	}

	if res.Tools == nil {
		res.Tools = []SearchTool{}
	}

	if res.Unavailable == nil {
		res.Unavailable = []SearchUnavailable{}
	}

	if res.Warnings == nil {
		res.Warnings = []SearchWarning{}
	}

	writeJSON(w, res)
}

// Search calls POST /admin/search: the page search_tools would return for
// req, and how the query was embedded.
func (c *Client) Search(ctx context.Context, req SearchRequest) (SearchResult, error) {
	var res SearchResult
	err := c.do(ctx, http.MethodPost, pathSearch, req, &res)

	return res, err
}
