package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/atomicpages/wayfindr/internal/admin"
	"github.com/atomicpages/wayfindr/internal/redact"
)

// searchTimeout bounds the search request: the query embedding has its own
// deadline (embedding.timeout) inside it.
const searchTimeout = 30 * time.Second

// maxSearchSize is search_tools' largest full-detail page.
const maxSearchSize = 10

func newSearchCmd(host Host) *cobra.Command {
	var (
		connectors []string
		size, page int
		explain    bool
	)
	cmd := &cobra.Command{
		Use:   "search <query>...",
		Short: "Search the catalog as an agent's search_tools would",
		Long: `Ask the running daemon to run search_tools for a query and show the page an
agent would get, in rank order. The words of the query may be given as
separate arguments; they are joined with spaces.

With --explain, each result also shows its place in the lexical and the
vector ranking, its BM25F score and cosine, and the fused score that orders
the results. The header says whether the query was embedded or the search
fell back to lexical only.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			query := strings.Join(args, " ")

			if strings.TrimSpace(query) == "" {
				return errors.New("the query is empty")
			}

			if size < 1 || size > maxSearchSize {
				return fmt.Errorf("--size must be from 1 to %d", maxSearchSize)
			}

			if page < 1 {
				return errors.New("--page must be at least 1")
			}

			ctx, cancel := context.WithTimeout(cmd.Context(), searchTimeout)
			defer cancel()
			d, err := findDaemon(ctx, host)

			if err != nil {
				return err
			}

			res, err := d.Search(ctx, admin.SearchRequest{
				Query: query, Connectors: connectors, Page: page, Size: size, Explain: explain,
			})

			if err != nil {
				return d.explain(err)
			}

			if wantJSON(cmd) {
				return writeSearchJSON(cmd.OutOrStdout(), res)
			}

			return writeSearchTable(cmd.OutOrStdout(), res, explain)
		},
	}
	cmd.Flags().StringArrayVar(&connectors, "connector", nil, "only this connector's tools (repeatable)")
	cmd.Flags().IntVar(&size, "size", 5, fmt.Sprintf("results per page, 1 to %d", maxSearchSize))
	cmd.Flags().IntVar(&page, "page", 1, "page of results")
	cmd.Flags().BoolVar(&explain, "explain", false, "show each result's lexical, vector, and fused scores")
	supportsJSON(cmd)

	return cmd
}

func writeSearchJSON(w io.Writer, res admin.SearchResult) error {
	b, err := json.MarshalIndent(res, "", "  ")

	if err != nil {
		return err
	}

	_, err = w.Write(append(b, '\n'))

	return err
}

// writeSearchTable prints how the query was embedded, the results in rank
// order (ranks count across pages), then the connectors that could not be
// searched, warnings, and whether there is another page. With explain,
// the columns are the scores in place of the summary. Tool names and
// summaries come from upstreams, so they are made printable first.
func writeSearchTable(w io.Writer, res admin.SearchResult, explain bool) error {
	var b bytes.Buffer
	b.WriteString("Embeddings: " + embeddingLine(res.Embedding) + "\n\n")

	if len(res.Tools) == 0 {
		b.WriteString("No tools match.\n")
	} else {
		tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
		rows := []string{"RANK\tTOOL\tSUMMARY"}

		if explain {
			rows = []string{"RANK\tTOOL\tFUSED\tLEXICAL\tVECTOR"}
		}

		first := (res.Page-1)*res.Size + 1

		for i, t := range res.Tools {
			cells := []string{fmt.Sprint(first + i), redact.Printable(t.Name), redact.Printable(t.Summary)}

			if explain {
				cells = append(cells[:2], scoreCells(t.Score)...)
			}

			rows = append(rows, strings.Join(cells, "\t"))
		}

		// A tabwriter over a bytes.Buffer cannot fail.
		_, _ = io.WriteString(tw, strings.Join(rows, "\n")+"\n")
		_ = tw.Flush()
	}

	var notes []string

	for _, u := range res.Unavailable {
		notes = append(notes, fmt.Sprintf("Unavailable: %s (%s): %s", u.Connector, u.Status, redact.Printable(u.Summary)))
	}

	for _, wn := range res.Warnings {
		notes = append(notes, fmt.Sprintf("Warning: %s: %s", wn.Connector, redact.Printable(wn.Summary)))
	}

	if res.HasMore {
		notes = append(notes, fmt.Sprintf("More results: run the same command with --page %d", res.Page+1))
	}

	if len(notes) > 0 {
		b.WriteString("\n" + strings.Join(notes, "\n") + "\n")
	}

	_, err := w.Write(b.Bytes())

	return err
}

// embeddingLine says how the query was embedded.
func embeddingLine(e admin.SearchEmbedding) string {
	switch e.Mode {
	case "on":
		return "on (" + e.Model + ")"
	case "failed":
		return "failed (" + e.Reason + "), lexical only"
	default:
		return e.Mode + ", lexical only"
	}
}

// scoreCells renders a score as the FUSED, LEXICAL, and VECTOR cells: a
// ranking the tool is not in shows "-".
func scoreCells(s *admin.SearchScore) []string {
	if s == nil {
		return []string{"-", "-", "-"}
	}

	lexical, vector := "-", "-"

	if s.LexicalRank > 0 {
		lexical = fmt.Sprintf("#%d  %.2f", s.LexicalRank, s.Lexical)
	}

	if s.VectorRank > 0 {
		vector = fmt.Sprintf("#%d  %.3f", s.VectorRank, s.Cosine)
	}

	return []string{fmt.Sprintf("%.4f", s.Fused), lexical, vector}
}
