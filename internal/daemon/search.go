package daemon

import (
	"context"

	"github.com/atomicpages/wayfindr/internal/admin"
	"github.com/atomicpages/wayfindr/internal/gateway"
)

// adminSearch adapts the gateway's Search to admin.Options.Search. It only
// converts: a search the gateway refuses becomes an *admin.SearchError
// with the gateway's Kind, Case, and advice.
func adminSearch(gw *gateway.Gateway) func(context.Context, admin.SearchRequest) (admin.SearchResult, error) {
	return func(ctx context.Context, req admin.SearchRequest) (admin.SearchResult, error) {
		res, err := gw.Search(ctx, gateway.SearchRequest{
			Query:      req.Query,
			Connectors: req.Connectors,
			Page:       req.Page,
			Size:       req.Size,
			Explain:    req.Explain,
		})

		if e, ok := gateway.AsError(err); ok && e.Kind == gateway.KindInvalidSearch {
			a := gateway.Advise(e)

			return admin.SearchResult{}, &admin.SearchError{
				Kind:   string(e.Kind),
				Case:   string(e.Case),
				Advice: admin.Advice{Summary: a.Summary, Action: a.Action, Link: a.Link},
			}
		}

		if err != nil {
			return admin.SearchResult{}, err
		}

		out := admin.SearchResult{
			Embedding: admin.SearchEmbedding{
				Mode:   string(res.Embedding.Mode),
				Model:  res.Embedding.Model,
				Reason: res.Embedding.Reason,
			},
			Tools:       make([]admin.SearchTool, 0, len(res.Hits)),
			Page:        res.Page,
			Size:        res.Size,
			HasMore:     res.HasMore,
			Unavailable: make([]admin.SearchUnavailable, 0, len(res.Unavailable)),
			Warnings:    make([]admin.SearchWarning, 0, len(res.Warnings)),
		}

		for _, h := range res.Hits {
			t := admin.SearchTool{Name: h.Name, Title: h.Title, Summary: h.Summary}

			if req.Explain {
				t.Score = &admin.SearchScore{
					LexicalRank: h.Score.LexicalRank,
					Lexical:     h.Score.Lexical,
					VectorRank:  h.Score.VectorRank,
					Cosine:      h.Score.Cosine,
					Fused:       h.Score.Fused,
				}
			}

			out.Tools = append(out.Tools, t)
		}

		for _, u := range res.Unavailable {
			out.Unavailable = append(out.Unavailable, admin.SearchUnavailable{
				Connector: u.Connector,
				Status:    u.Status,
				Advice:    admin.Advice{Summary: u.Summary, Action: u.Action, Link: u.Link},
			})
		}

		for _, w := range res.Warnings {
			out.Warnings = append(out.Warnings, admin.SearchWarning{
				Connector: w.Connector,
				Advice:    admin.Advice{Summary: w.Summary, Action: w.Action, Link: w.Link},
			})
		}

		return out, nil
	}
}
