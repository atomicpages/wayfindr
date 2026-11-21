package daemon_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/atomicpages/wayfindr/internal/admin"
	"github.com/atomicpages/wayfindr/internal/testutil/fakeupstream"
)

// POST /admin/search (`wayfindr search`) returns what search_tools returns
// for the same query, and refuses what it refuses, in its words.
func TestAdminSearchRanksAsSearchToolsDoes(t *testing.T) {
	h := newHost(t)
	r := serve(t, h, h.options(t), map[string]any{
		"server":     map[string]any{"listen": freeAddr(t)},
		"connectors": map[string]any{"files": stdioConnector(t, fakeupstream.V20251125)},
	})
	r.waitStatus(t, "files synced", synced("files"))
	c := admin.NewClient("http://"+r.addr, "", r.client)

	var page struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}

	if err := json.Unmarshal([]byte(call(t, r.mcp, "search_tools", map[string]any{"query": "echo"})), &page); err != nil {
		t.Fatalf("search_tools response: %v", err)
	}

	want := []string{}

	for _, tl := range page.Tools {
		want = append(want, tl.Name)
	}

	if len(want) == 0 {
		t.Fatal("search_tools found nothing for echo")
	}

	res, err := c.Search(t.Context(), admin.SearchRequest{Query: "echo", Explain: true})

	if err != nil {
		t.Fatalf("admin Search: %v", err)
	}

	got := []string{}

	for _, tl := range res.Tools {
		got = append(got, tl.Name)

		if tl.Score == nil || tl.Score.Fused <= 0 {
			t.Errorf("%s has score %+v, want a positive fused score", tl.Name, tl.Score)
		}
	}

	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("tools (-search_tools +admin):\n%s", diff)
	}

	if res.Embedding.Mode != "off" || res.Page != 1 || res.Size != 5 {
		t.Errorf("embedding %+v page %d size %d, want off, 1, 5", res.Embedding, res.Page, res.Size)
	}

	_, err = c.Search(t.Context(), admin.SearchRequest{Query: "echo", Connectors: []string{"jira"}})

	if statusOf(err) != http.StatusBadRequest || !strings.Contains(err.Error(), "No connector is called jira. Configured connectors: files.") {
		t.Errorf("search of an unknown connector: %v, want 400 naming jira and files", err)
	}
}
