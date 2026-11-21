package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/store"
	"github.com/atomicpages/wayfindr/internal/upstream"
)

// searchResponse is search_tools' result, in the shape prd.md specifies.
// Missing is set only by a names lookup.
type searchResponse struct {
	Tools       []json.RawMessage `json:"tools"`
	Page        int               `json:"page"`
	Size        int               `json:"size"`
	HasMore     bool              `json:"has_more"`
	Unavailable []Unavailable     `json:"unavailable"`
	Warnings    []Warning         `json:"warnings"`
	Missing     []string          `json:"missing,omitzero"`
}

// briefEntry is a tool in a detail=brief result: enough to choose it,
// without its schemas.
type briefEntry struct {
	Name        string          `json:"name"`
	Title       string          `json:"title,omitempty"`
	Summary     string          `json:"summary,omitempty"`
	Required    []string        `json:"required"`
	Annotations json.RawMessage `json:"annotations,omitempty"`
}

// maxSummary is the most runes a brief entry's summary has.
const maxSummary = 160

// Unavailable is one connector that is not ready: prd.md's
// {connector, status}, plus the advice that says what to do about it.
type Unavailable struct {
	Connector string `json:"connector"`
	Status    string `json:"status"`
	Summary   string `json:"summary,omitempty"`
	Action    string `json:"action,omitempty"`
	Link      string `json:"link,omitempty"`
}

// searchArgs are search_tools' arguments. Numbers decode as float64 so that
// a client sending 5.0 for an integer is not refused for it.
type searchArgs struct {
	Query      *string   `json:"query"`
	Page       *float64  `json:"page"`
	Size       *float64  `json:"size"`
	Connectors *[]string `json:"connectors"`
	Detail     *string   `json:"detail"`
	Names      *[]string `json:"names"`
}

// searchQuery is a validated search_tools request: a search (text, page,
// size, connectors, brief) or, when names is set, a lookup by name.
type searchQuery struct {
	text       string
	page, size int
	connectors []string
	brief      bool
	names      []string
	// scope is what narrows the request: its profile and project scope.
	scope config.Narrowing
}

func (g *Gateway) searchTools(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if g.onSearch != nil {
		g.onSearch()
	}

	client, authed := clientOf(req)
	scope, err := g.narrowingOf(req, client, authed)

	if err != nil {
		return g.errorResult(err), nil
	}

	statuses := inScope(g.upstreams.Status(), scope)
	q, err := parseSearch(req.Params.Arguments, statuses)

	if err != nil {
		return g.errorResult(err), nil
	}

	q.scope = scope

	if q.names != nil {
		return g.lookup(ctx, scope, q.names, statuses), nil
	}

	page, _, err := g.search(ctx, q, false)

	if err != nil {
		return g.errorResult(err), nil
	}

	resp := searchResponse{
		Tools:       make([]json.RawMessage, 0, len(page.Tools)),
		Page:        q.page,
		Size:        q.size,
		HasMore:     page.HasMore,
		Unavailable: unavailableConnectors(statuses, q.connectors),
		Warnings:    g.readyWarnings(ctx, statuses, q.connectors),
	}

	render := qualify

	if q.brief {
		render = brief
	}

	for _, t := range page.Tools {
		def, err := render(t)

		if err != nil {
			return g.errorResult(&Error{Kind: KindInternal, Case: CaseCatalog, Err: err}), nil
		}

		resp.Tools = append(resp.Tools, def)
	}

	return g.respond(resp), nil
}

// search runs a validated search (not a names lookup): it embeds the query
// and ranks the catalog, with each result's scores when explain is set.
func (g *Gateway) search(ctx context.Context, q searchQuery, explain bool) (store.SearchPage, QueryEmbedding, error) {
	connectors := q.connectors

	// A search of every connector is bounded by the connectors both the
	// profile and the scope allow, in the store, so pages stay full. The
	// store reads no connectors as all, so an allowlist of none is
	// answered here.
	if allow := q.scope.Allowlist(); len(connectors) == 0 && allow != nil {
		if len(allow) == 0 {
			return store.SearchPage{}, QueryEmbedding{}, nil
		}

		connectors = allow
	}

	vec, modelID, how := g.queries.embed(ctx, q.text)
	page, err := g.catalog.Search(ctx, store.SearchQuery{
		UserID:     g.userID,
		Text:       q.text,
		Vector:     vec,
		ModelID:    modelID,
		Connectors: connectors,
		Page:       q.page,
		Size:       q.size,
		Explain:    explain,
	})

	if err != nil {
		return store.SearchPage{}, how, catalogError(ctx, err, "", "")
	}

	return g.dropHidden(q.scope, page), how, nil
}

// dropHidden removes from page the tools the live policy, the profile, or
// the scope hides. Syncs keep tools the policy hides out of the catalog, so
// for the policy this only matters after a reload, until the connector's
// re-sync succeeds. The tools lists of a profile and a scope are applied
// only here. Either way the page is short by those tools. A tool on a
// connector the client token leaves out is dropped too, though search
// already limits the store to the token's connectors.
func (g *Gateway) dropHidden(scope config.Narrowing, page store.SearchPage) store.SearchPage {
	hides := func(t store.Tool) bool { return !scope.TokenAllows(t.Connector) || g.hiddenBy(scope, t) != "" }

	if !slices.ContainsFunc(page.Tools, hides) {
		return page
	}

	out := page
	out.Tools, out.Scores = nil, nil

	for i, t := range page.Tools {
		if hides(t) {
			continue
		}

		out.Tools = append(out.Tools, t)

		if page.Scores != nil {
			out.Scores = append(out.Scores, page.Scores[i])
		}
	}

	return out
}

// SearchRequest is a search run for a person rather than an agent, as by
// `wayfindr search`. It is validated as search_tools' arguments are, with
// detail full and no names.
type SearchRequest struct {
	Query string
	// Connectors limits the search to these connectors; empty searches all.
	Connectors []string
	// Page and Size are as in search_tools; 0 means its default.
	Page, Size int
	// Explain fills each hit's Score.
	Explain bool
}

// SearchResult is one page of a search, ranked as search_tools ranks it.
type SearchResult struct {
	Hits        []SearchHit
	Page, Size  int
	HasMore     bool
	Unavailable []Unavailable
	Warnings    []Warning
	Embedding   QueryEmbedding
}

// SearchHit is one ranked tool.
type SearchHit struct {
	// Name is the qualified name, <connector>.<tool>.
	Name    string
	Title   string
	Summary string // as in a detail=brief entry
	// Score is why the tool ranked where it did; zero unless the request
	// asked for Explain.
	Score store.Score
}

// Search runs r as search_tools would run the same query. An invalid r is
// a KindInvalidSearch *Error; a catalog failure is KindInternal, or
// KindCanceled once ctx has ended.
func (g *Gateway) Search(ctx context.Context, r SearchRequest) (SearchResult, error) {
	statuses := g.upstreams.Status()
	a := searchArgs{Query: &r.Query}

	if r.Page != 0 {
		p := float64(r.Page)
		a.Page = &p
	}

	if r.Size != 0 {
		s := float64(r.Size)
		a.Size = &s
	}

	if len(r.Connectors) > 0 {
		a.Connectors = &r.Connectors
	}

	q, err := validateSearch(a, statuses)

	if err != nil {
		return SearchResult{}, err
	}

	page, how, err := g.search(ctx, q, r.Explain)

	if err != nil {
		return SearchResult{}, err
	}

	if r.Explain && len(page.Scores) != len(page.Tools) {
		return SearchResult{}, &Error{Kind: KindInternal, Case: CaseCatalog,
			Err: fmt.Errorf("catalog returned %d scores for %d tools", len(page.Scores), len(page.Tools))}
	}

	res := SearchResult{
		Hits:        make([]SearchHit, 0, len(page.Tools)),
		Page:        q.page,
		Size:        q.size,
		HasMore:     page.HasMore,
		Unavailable: unavailableConnectors(statuses, q.connectors),
		Warnings:    g.readyWarnings(ctx, statuses, q.connectors),
		Embedding:   how,
	}

	for i, t := range page.Tools {
		h := SearchHit{Name: t.Connector + "." + t.Name, Title: t.Title, Summary: summarize(t.Description, t.Title)}

		if r.Explain {
			h.Score = page.Scores[i]
		}

		res.Hits = append(res.Hits, h)
	}

	return res, nil
}

// lookup returns the full definitions of names, in order. A name that is
// not qualified, whose connector is not configured, or that the catalog
// does not have is listed under missing; finding none is an error.
// unavailable and warnings cover the configured connectors the names
// mention. A tool the policy, the profile, or the scope hides is missing
// too.
func (g *Gateway) lookup(ctx context.Context, scope config.Narrowing, names []string, statuses []upstream.ConnectorStatus) *mcp.CallToolResult {
	resp := searchResponse{
		Tools:   make([]json.RawMessage, 0, len(names)),
		Page:    1,
		Missing: []string{},
	}

	var connectors []string

	for _, name := range names {
		// Connector names may not contain dots, as in callTool.
		connector, tool, ok := strings.Cut(name, ".")

		if !ok || connector == "" || tool == "" || !slices.ContainsFunc(statuses, func(s upstream.ConnectorStatus) bool {
			return s.Connector == connector
		}) {
			resp.Missing = append(resp.Missing, name)

			continue
		}

		if !slices.Contains(connectors, connector) {
			connectors = append(connectors, connector)
		}

		t, err := g.catalog.GetTool(ctx, g.userID, connector, tool)

		if errors.Is(err, store.ErrNotFound) || (err == nil && g.hiddenBy(scope, t) != "") {
			resp.Missing = append(resp.Missing, name)

			continue
		}

		if err != nil {
			return g.errorResult(catalogError(ctx, err, name, connector))
		}

		def, err := qualify(t)

		if err != nil {
			return g.errorResult(&Error{Kind: KindInternal, Case: CaseCatalog, Tool: name, Connector: connector, Err: err})
		}

		resp.Tools = append(resp.Tools, def)
	}

	resp.Unavailable = unavailableConnectors(statuses, connectors)
	resp.Warnings = g.readyWarnings(ctx, statuses, connectors)

	if len(resp.Tools) == 0 {
		return noneFound(names, resp.Unavailable)
	}

	resp.Size = len(resp.Tools)

	return g.respond(resp)
}

// noneFound is the error for a lookup that found none of names. When a
// connector the names mention is not ready, which may be why, its advice
// replaces the generic one, auth link included.
func noneFound(names []string, unavailable []Unavailable) *mcp.CallToolResult {
	a := Advise(&Error{Kind: KindUnknownTool, Case: CaseNoneFound, Detail: strings.Join(names, ", ")})

	if len(unavailable) > 0 {
		u := unavailable[0]
		a.Summary += " " + u.Connector + " is " + u.Status + ": " + u.Summary
		a.Action, a.Link = u.Action, u.Link
	}

	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: render(a)}}}
}

// respond renders resp as search_tools' successful result.
func (g *Gateway) respond(resp searchResponse) *mcp.CallToolResult {
	b, err := json.Marshal(resp)

	if err != nil {
		return g.errorResult(&Error{Kind: KindInternal, Case: CaseCatalog, Err: err})
	}

	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}}
}

// parseSearch decodes search_tools' arguments and validates them with
// validateSearch.
func parseSearch(raw json.RawMessage, statuses []upstream.ConnectorStatus) (searchQuery, error) {
	var a searchArgs

	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &a); err != nil {
			return searchQuery{}, &Error{Kind: KindInvalidSearch, Case: CaseMalformedSearch, Err: err}
		}
	}

	return validateSearch(a, statuses)
}

// validateSearch validates search arguments against the configured
// connectors in statuses. detail is "full" (default) or "brief". With
// names, nothing else but detail "full" may be given, and names holds 1 to
// 10 non-blank entries, kept in order without duplicates. Otherwise:
// a non-blank query, page at least 1 (default 1), size 1 to 10 (default 5)
// or, brief, 1 to 25 (default 15), and only configured connectors.
func validateSearch(a searchArgs, statuses []upstream.ConnectorStatus) (searchQuery, error) {
	q := searchQuery{page: 1, size: defaultSize}
	limit := maxSize

	if a.Detail != nil {
		switch *a.Detail {
		case detailFull:
		case detailBrief:
			q.brief, q.size, limit = true, defaultBriefSize, maxBriefSize
		default:
			return q, &Error{Kind: KindInvalidSearch, Case: CaseBadDetail, Detail: fmt.Sprintf("%q", *a.Detail)}
		}
	}

	if a.Names != nil {
		return parseNames(a, q.brief)
	}

	if a.Query == nil || strings.TrimSpace(*a.Query) == "" {
		return q, &Error{Kind: KindInvalidSearch, Case: CaseMissingQuery}
	}

	q.text = *a.Query

	if a.Page != nil {
		p, ok := wholeNumber(*a.Page)

		if !ok || p < 1 {
			return q, &Error{Kind: KindInvalidSearch, Case: CaseBadPage, Detail: fmt.Sprint(*a.Page)}
		}

		q.page = p
	}

	if a.Size != nil {
		s, ok := wholeNumber(*a.Size)

		if !ok || s < 1 || s > limit {
			return q, &Error{Kind: KindInvalidSearch, Case: CaseBadSize, Detail: fmt.Sprint(*a.Size)}
		}

		q.size = s
	}

	if a.Connectors != nil {
		configured := make([]string, 0, len(statuses))

		for _, s := range statuses {
			configured = append(configured, s.Connector)
		}

		for _, c := range *a.Connectors {
			if !slices.Contains(configured, c) {
				return q, &Error{Kind: KindInvalidSearch, Case: CaseUnknownConnector, Connector: c, Configured: configured}
			}
		}

		q.connectors = *a.Connectors
	}

	return q, nil
}

// parseNames validates a names lookup: names alone (detail "full" aside),
// 1 to maxNames entries, none blank. Duplicates are dropped, keeping the
// first. isBrief reports whether detail was "brief".
func parseNames(a searchArgs, isBrief bool) (searchQuery, error) {
	conflicts := []struct {
		field string
		set   bool
	}{
		{"query", a.Query != nil},
		{"page", a.Page != nil},
		{"size", a.Size != nil},
		{"connectors", a.Connectors != nil},
		{"detail brief", isBrief},
	}

	for _, c := range conflicts {
		if c.set {
			return searchQuery{}, &Error{Kind: KindInvalidSearch, Case: CaseNamesConflict, Detail: c.field}
		}
	}

	names := *a.Names

	if len(names) == 0 || len(names) > maxNames {
		return searchQuery{}, &Error{Kind: KindInvalidSearch, Case: CaseBadNames, Detail: fmt.Sprint(len(names))}
	}

	q := searchQuery{names: make([]string, 0, len(names))}

	for _, n := range names {
		if strings.TrimSpace(n) == "" {
			return searchQuery{}, &Error{Kind: KindInvalidSearch, Case: CaseBadNames, Detail: fmt.Sprintf("%q", n)}
		}

		if !slices.Contains(q.names, n) {
			q.names = append(q.names, n)
		}
	}

	return q, nil
}

// wholeNumber returns f as an int if it is a whole number in a sane range.
func wholeNumber(f float64) (int, bool) {
	if f != math.Trunc(f) || math.Abs(f) > 1e9 {
		return 0, false
	}

	return int(f), true
}

// qualify returns t's definition with its name rewritten to the qualified
// name <connector>.<tool> (GLOSSARY: Tool). Every other field is passed
// through as the upstream sent it, annotations included, so client
// permission prompts keep working (ADR 0001).
func qualify(t store.Tool) (json.RawMessage, error) {
	var def map[string]json.RawMessage

	if err := json.Unmarshal(t.Definition, &def); err != nil {
		return nil, fmt.Errorf("decode definition of %s.%s: %w", t.Connector, t.Name, err)
	}

	name, err := json.Marshal(t.Connector + "." + t.Name)

	if err != nil {
		return nil, err
	}

	def["name"] = name

	return json.Marshal(def)
}

// brief returns t as a brief entry: its qualified name, title, a one-line
// summary, the names of its required arguments, and its annotations as the
// upstream sent them.
func brief(t store.Tool) (json.RawMessage, error) {
	var def struct {
		Annotations json.RawMessage `json:"annotations"`
	}

	if err := json.Unmarshal(t.Definition, &def); err != nil {
		return nil, fmt.Errorf("decode definition of %s.%s: %w", t.Connector, t.Name, err)
	}

	e := briefEntry{
		Name:     t.Connector + "." + t.Name,
		Title:    t.Title,
		Summary:  summarize(t.Description, t.Title),
		Required: t.Required,
	}

	if e.Required == nil {
		e.Required = []string{}
	}

	if string(def.Annotations) != "null" {
		e.Annotations = def.Annotations
	}

	return json.Marshal(e)
}

// summarize returns the first sentence of description's first line, cut
// to maxSummary runes on a word boundary, or title when description is
// blank.
func summarize(description, title string) string {
	s := strings.TrimSpace(description)

	if s == "" {
		return title
	}

	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}

	if i := strings.Index(s, ". "); i >= 0 {
		s = s[:i+1]
	}

	r := []rune(s)

	if len(r) <= maxSummary {
		return s
	}

	// Leave a rune for the ellipsis.
	cut := string(r[:maxSummary-1])

	if i := strings.LastIndexByte(cut, ' '); i > 0 {
		cut = cut[:i]
	}

	return strings.TrimRight(cut, " ,;:") + "…"
}

// readyWarnings lists the warnings of the ready connectors in filter (all
// when it is empty), as Options.Warnings gives them. A connector that is
// not ready is under unavailable already, with its own advice, so it is
// not asked about.
func (g *Gateway) readyWarnings(ctx context.Context, statuses []upstream.ConnectorStatus, filter []string) []Warning {
	var ready []string

	for _, s := range statuses {
		if s.State == upstream.StateReady && (len(filter) == 0 || slices.Contains(filter, s.Connector)) {
			ready = append(ready, s.Connector)
		}
	}

	if g.warnings == nil || len(ready) == 0 {
		return []Warning{}
	}

	return append([]Warning{}, g.warnings(ctx, ready)...)
}

// unavailableConnectors lists the connectors that are not ready, each with
// upstream's advice for its state, the same `wayfindr status` shows. A non-empty
// filter limits it to the connectors searched.
func unavailableConnectors(statuses []upstream.ConnectorStatus, filter []string) []Unavailable {
	out := []Unavailable{}

	for _, s := range statuses {
		if s.State == upstream.StateReady || (len(filter) > 0 && !slices.Contains(filter, s.Connector)) {
			continue
		}

		a := s.Advice()
		out = append(out, Unavailable{
			Connector: s.Connector,
			Status:    string(s.State),
			Summary:   a.Summary,
			Action:    a.Action,
			Link:      a.Link,
		})
	}

	return out
}
