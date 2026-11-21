package sqlite

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"

	"github.com/atomicpages/wayfindr/internal/store"
)

// BM25F parameters and typo tolerance from design §7.5.
const (
	bm25K1 = 1.2
	bm25B  = 0.75

	// minExpandLen is the shortest query token, in runes, that is expanded
	// by trigram similarity when it has no exact match in the vocabulary.
	minExpandLen = 4
	// minTrigramSimilarity is the trigram Jaccard floor for an expansion.
	minTrigramSimilarity = 0.4
)

// Hybrid search parameters from design §7.5.
const (
	// maxVectorCandidates is how many nearest neighbors, by cosine, enter
	// fusion.
	maxVectorCandidates = 50
	// rrfK is the reciprocal-rank fusion constant: rank r in a ranking adds
	// 1/(rrfK + r) to a doc's fused score.
	rrfK = 5
)

// field is one weighted BM25F field of a catalog entry.
type field int

const (
	fieldName              field = iota // qualified name <connector>.<tool>
	fieldTitle                          // title
	fieldParamNames                     // top-level parameter names
	fieldDescription                    // description
	fieldParamDescriptions              // top-level parameter descriptions
	numFields
)

// weight is the field's BM25F weight (design §7.5).
func (f field) weight() float64 {
	switch f {
	case fieldName:
		return 3.0
	case fieldTitle:
		return 2.0
	case fieldParamNames:
		return 1.5
	case fieldDescription:
		return 1.0
	case fieldParamDescriptions:
		return 0.5
	case numFields: // a count, not a field
	}

	return 0
}

type toolKey struct{ connector, name string }

// doc is one catalog entry as the index holds it: the entry itself plus its
// per-field term frequencies and lengths. A doc is immutable once built;
// writes replace it.
type doc struct {
	tool      store.Tool
	qualified string
	// nameWords and qualifiedWords are nameKey of the tool's name and of
	// its qualified name, for the exact-name boost.
	nameWords, qualifiedWords string
	tf                        [numFields]map[string]int
	length                    [numFields]int
	norm                      float64 // Euclidean norm of tool.Embedding
}

func newDoc(t store.Tool) *doc {
	d := &doc{tool: t, qualified: t.Connector + "." + t.Name, norm: norm(t.Embedding)}
	d.nameWords, d.qualifiedWords = nameKey(t.Name), nameKey(d.qualified)
	add := func(f field, text string) {
		if d.tf[f] == nil {
			d.tf[f] = map[string]int{}
		}

		for _, tok := range tokenize(text) {
			d.tf[f][tok]++
			d.length[f]++
		}
	}
	add(fieldName, d.qualified)
	add(fieldTitle, t.Title)
	add(fieldDescription, t.Description)

	for _, p := range t.Params {
		add(fieldParamNames, p.Name)
		add(fieldParamDescriptions, p.Description)
	}

	return d
}

// corpus is one user's catalog with the collection statistics BM25F needs.
// They are kept up to date on every write rather than recomputed per query.
type corpus struct {
	docs map[toolKey]*doc
	df   map[string]int // token → number of docs containing it in any field
	// grams maps each token in df to its trigrams, surface plurals included,
	// so a typo of a word that folding mangles (serie for series, which
	// folds to sery) still finds it.
	grams  map[string]map[string]struct{}
	lenSum [numFields]int
}

func newCorpus() *corpus {
	return &corpus{docs: map[toolKey]*doc{}, df: map[string]int{}, grams: map[string]map[string]struct{}{}}
}

func (c *corpus) put(t store.Tool) {
	key := toolKey{t.Connector, t.Name}
	c.remove(key)
	d := newDoc(t)
	c.docs[key] = d

	for f := range numFields {
		c.lenSum[f] += d.length[f]
	}

	for tok := range d.vocabulary() {
		if c.df[tok] == 0 {
			c.grams[tok] = trigrams(tok)
		}

		c.df[tok]++
	}
}

func (c *corpus) remove(key toolKey) {
	d, ok := c.docs[key]

	if !ok {
		return
	}

	delete(c.docs, key)

	for f := range numFields {
		c.lenSum[f] -= d.length[f]
	}

	for tok := range d.vocabulary() {
		c.df[tok]--

		if c.df[tok] == 0 {
			delete(c.df, tok)
			delete(c.grams, tok)
		}
	}
}

// setEmbedding records a stored embedding on the entry at key, if present.
// The embedding is not a BM25F field, so the statistics are unchanged.
func (c *corpus) setEmbedding(key toolKey, modelID string, vec []float32) {
	d, ok := c.docs[key]

	if !ok {
		return
	}

	// Docs are shared with in-flight results, so build a new one rather
	// than mutating it.
	updated := *d
	updated.tool.Embedding = slices.Clone(vec)
	updated.tool.EmbeddingModel = modelID
	updated.norm = norm(vec)
	c.docs[key] = &updated
}

// vocabulary is the set of distinct tokens in any field of d.
func (d *doc) vocabulary() map[string]struct{} {
	v := map[string]struct{}{}

	for f := range numFields {
		for tok := range d.tf[f] {
			v[tok] = struct{}{}
		}
	}

	return v
}

// term is a vocabulary token to score, weighted 1 for an exact query token
// and by trigram similarity for a typo expansion.
type term struct {
	token  string
	weight float64
}

// queryTerms maps query words onto the vocabulary (design §7.5). Each word
// carries a total weight of 1. A word that folding changes splits it evenly
// between its surface and folded forms when the vocabulary has both, so a
// plural counts once yet an exact plural still outranks the bare singular;
// when only one form is present, that form takes the whole weight. A word
// with neither form is typo-expanded. The result is sorted by token so scores
// are summed in a fixed order.
func (c *corpus) queryTerms(text string) []term {
	best := map[string]float64{}
	add := func(tok string, w float64) {
		if w > best[tok] {
			best[tok] = w
		}
	}

	for _, word := range words(text) {
		folded := foldPlural(word)
		surfaceHit, foldedHit := c.df[word] > 0, c.df[folded] > 0

		switch {
		case surfaceHit && foldedHit && folded != word:
			add(word, 0.5)
			add(folded, 0.5)
		case surfaceHit:
			add(word, 1)
		case foldedHit:
			add(folded, 1)
		default:
			c.expand(word, add)

			if folded != word {
				c.expand(folded, add)
			}
		}
	}

	terms := make([]term, 0, len(best))

	for tok, w := range best {
		terms = append(terms, term{tok, w})
	}

	slices.SortFunc(terms, func(a, b term) int { return cmp.Compare(a.token, b.token) })

	return terms
}

// expand adds the vocabulary tokens whose trigram similarity to word reaches
// minTrigramSimilarity, each weighted by that similarity. Words shorter than
// minExpandLen are not expanded. A match on a surface plural is scored as its
// folded form: a doc holding the plural also holds the folded form, so
// scoring both would count the doc twice.
func (c *corpus) expand(word string, add func(tok string, w float64)) {
	if runeLen(word) < minExpandLen {
		return
	}

	q := trigrams(word)

	for v, g := range c.grams {
		if sim := jaccard(q, g); sim >= minTrigramSimilarity {
			add(foldPlural(v), sim)
		}
	}
}

// lexicalScore is the BM25F score of d for terms; 0 means no match.
func (c *corpus) lexicalScore(d *doc, terms []term) float64 {
	n := float64(len(c.docs))
	var score float64

	for _, t := range terms {
		var tf float64

		for f := range numFields {
			raw := d.tf[f][t.token]

			if raw == 0 {
				continue
			}

			avg := float64(c.lenSum[f]) / n
			norm := 1 - bm25B + bm25B*float64(d.length[f])/avg
			tf += f.weight() * float64(raw) / norm
		}

		if tf == 0 {
			continue
		}

		df := float64(c.df[t.token])
		idf := math.Log(1 + (n-df+0.5)/(df+0.5))
		score += t.weight * idf * tf / (bm25K1 + tf)
	}

	return score
}

// index is the in-memory search index over every user's catalog (ADR 0005).
// The Store loads it at Open and updates it after each committed catalog
// write; Search only reads it.
type index struct {
	mu    sync.RWMutex
	users map[string]*corpus
}

func newIndex() *index { return &index{users: map[string]*corpus{}} }

// put adds or replaces one entry.
func (x *index) put(userID string, t store.Tool) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.corpus(userID).put(t)
}

// replaceConnector makes tools the connector's complete set of entries.
func (x *index) replaceConnector(userID, connector string, tools []store.Tool) {
	x.mu.Lock()
	defer x.mu.Unlock()
	c := x.corpus(userID)
	c.dropConnector(connector)

	for _, t := range tools {
		c.put(t)
	}
}

// deleteConnector drops the connector's entries.
func (x *index) deleteConnector(userID, connector string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.corpus(userID).dropConnector(connector)
}

// setEmbedding records a stored embedding on an existing entry.
func (x *index) setEmbedding(userID, connector, name, modelID string, vec []float32) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.corpus(userID).setEmbedding(toolKey{connector, name}, modelID, vec)
}

func (x *index) corpus(userID string) *corpus {
	c, ok := x.users[userID]

	if !ok {
		c = newCorpus()
		x.users[userID] = c
	}

	return c
}

func (c *corpus) dropConnector(connector string) {
	for key := range c.docs {
		if key.connector == connector {
			c.remove(key)
		}
	}
}

// search ranks the user's catalog for q and returns the requested page
// (design §7.5). The lexical candidates and, when q has a vector, the vector
// candidates are ranked over the whole catalog and fused. The connector
// filter then drops results, so a filtered ranking is the unfiltered one
// restricted to those connectors.
func (x *index) search(q store.SearchQuery) (store.SearchPage, error) {
	if q.Page < 1 || q.Size < 1 {
		return store.SearchPage{}, fmt.Errorf("page %d and size %d must be positive", q.Page, q.Size)
	}

	x.mu.RLock()
	defer x.mu.RUnlock()
	c, ok := x.users[q.UserID]

	if !ok {
		return store.SearchPage{}, nil
	}

	ranked := fuse(c.lexicalRanking(q.Text), c.vectorRanking(q.Vector, q.ModelID), nameKey(q.Text))

	if len(q.Connectors) > 0 {
		ranked = slices.DeleteFunc(ranked, func(h fused) bool { return !slices.Contains(q.Connectors, h.doc.tool.Connector) })
	}

	// Compare via division first so a huge page can't overflow the offset.
	if q.Page-1 > len(ranked)/q.Size {
		return store.SearchPage{}, nil
	}

	start := min((q.Page-1)*q.Size, len(ranked))
	end := min(start+q.Size, len(ranked))
	page := store.SearchPage{HasMore: q.Page <= len(ranked)/q.Size && len(ranked) > q.Page*q.Size}

	for _, h := range ranked[start:end] {
		page.Tools = append(page.Tools, cloneTool(h.doc.tool))

		if q.Explain {
			page.Scores = append(page.Scores, h.score)
		}
	}

	return page, nil
}

// scored is a doc with its score in one ranking.
type scored struct {
	doc   *doc
	score float64
}

// sortScored orders hits by score, highest first, ties by qualified name.
func sortScored(hits []scored) {
	slices.SortFunc(hits, func(a, b scored) int {
		if byScore := cmp.Compare(b.score, a.score); byScore != 0 {
			return byScore
		}

		return cmp.Compare(a.doc.qualified, b.doc.qualified)
	})
}

// lexicalRanking returns the lexical candidates for text, best first: every
// doc whose BM25F score is above 0.
func (c *corpus) lexicalRanking(text string) []scored {
	terms := c.queryTerms(text)

	if len(terms) == 0 {
		return nil
	}

	var hits []scored

	for _, d := range c.docs {
		if s := c.lexicalScore(d, terms); s > 0 {
			hits = append(hits, scored{d, s})
		}
	}

	sortScored(hits)

	return hits
}

// vectorRanking returns the vector candidates for vec, best first: the
// maxVectorCandidates docs with the highest cosine above 0, among those
// embedded under modelID. A nil vec has none.
func (c *corpus) vectorRanking(vec []float32, modelID string) []scored {
	qnorm := norm(vec)

	if qnorm == 0 {
		return nil
	}

	var hits []scored

	for _, d := range c.docs {
		e := d.tool.Embedding

		// A vector of another length can't come from modelID, whose
		// identity includes the dimensions; skip it rather than misread it.
		if d.tool.EmbeddingModel != modelID || len(e) != len(vec) || d.norm == 0 {
			continue
		}

		var dot float64

		for i := range vec {
			dot += float64(vec[i]) * float64(e[i])
		}

		if cos := dot / (qnorm * d.norm); cos > 0 {
			hits = append(hits, scored{d, cos})
		}
	}

	sortScored(hits)

	return hits[:min(len(hits), maxVectorCandidates)]
}

// fused is a doc with its place in each ranking and its fused score.
type fused struct {
	doc   *doc
	score store.Score
}

// fuse merges the lexical and vector rankings by reciprocal-rank fusion: a
// doc scores Σ 1/(rrfK + rank) over the rankings it appears in, ranks
// counting from 1, lexical first. A doc that query, a nameKey, names
// exactly (its name or qualified name has that key) ranks ahead of every
// other doc, and is marked ExactName. Within each group the higher fused
// score comes first and ties go to the lower qualified name, so the order
// is total and pages are stable. A single ranking fuses to itself.
func fuse(lexical, vector []scored, query string) []fused {
	byDoc := map[*doc]*store.Score{}

	score := func(d *doc) *store.Score {
		s, ok := byDoc[d]

		if !ok {
			s = &store.Score{ExactName: query != "" && (d.nameWords == query || d.qualifiedWords == query)}
			byDoc[d] = s
		}

		return s
	}

	for i, h := range lexical {
		s := score(h.doc)
		s.LexicalRank, s.Lexical = i+1, h.score
		s.Fused += 1 / float64(rrfK+i+1)
	}

	for i, h := range vector {
		s := score(h.doc)
		s.VectorRank, s.Cosine = i+1, h.score
		s.Fused += 1 / float64(rrfK+i+1)
	}

	hits := make([]fused, 0, len(byDoc))

	for d, s := range byDoc {
		hits = append(hits, fused{d, *s})
	}

	slices.SortFunc(hits, func(a, b fused) int {
		if a.score.ExactName != b.score.ExactName {
			if a.score.ExactName {
				return -1
			}

			return 1
		}

		if byScore := cmp.Compare(b.score.Fused, a.score.Fused); byScore != 0 {
			return byScore
		}

		return cmp.Compare(a.doc.qualified, b.doc.qualified)
	})

	return hits
}

// nameKey is text read as a tool name, for the exact-name boost (design
// §7.5): its words by the tokenizer's rules, without plural folding, joined
// by single spaces. list issues, list_issues, and listIssues all give
// "list issues"; github.list_issues gives "github list issues".
func nameKey(text string) string {
	return strings.Join(words(text), " ")
}

// norm is the Euclidean norm of v; 0 for a nil v.
func norm(v []float32) float64 {
	var sum float64

	for _, f := range v {
		sum += float64(f) * float64(f)
	}

	return math.Sqrt(sum)
}

// cloneTool copies an entry so callers can't reach the index's slices.
func cloneTool(t store.Tool) store.Tool {
	t.Definition = slices.Clone(t.Definition)
	t.ContentHash = slices.Clone(t.ContentHash)
	t.Params = slices.Clone(t.Params)
	t.Embedding = slices.Clone(t.Embedding)

	return t
}
