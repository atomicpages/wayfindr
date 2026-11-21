package embed_test

import (
	"context"
	"net/http"
	"os"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/atomicpages/wayfindr/internal/embed"
)

// TestLive runs against a real provider; see "Manual check" in the package
// doc. It is skipped unless WAYFINDR_EMBED_LIVE_URL is set.
func TestLive(t *testing.T) {
	base := os.Getenv("WAYFINDR_EMBED_LIVE_URL")

	if base == "" {
		t.Skip("WAYFINDR_EMBED_LIVE_URL not set")
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
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	in := make([]string, 70)

	for i := range in {
		in[i] = "tool number " + strconv.Itoa(i) + ": creates an issue in a tracker"
	}

	got, err := c.Embed(ctx, in)

	if err != nil {
		t.Fatalf("Embed: %v", err)
	}

	if len(got) != len(in) {
		t.Fatalf("got %d vectors for %d inputs", len(got), len(in))
	}

	for i, v := range got {
		if len(v) != dims {
			t.Errorf("vector %d has %d dimensions, want %d", i, len(v), dims)
		}
	}

	if slices.Equal(got[0], got[1]) {
		t.Error("distinct inputs gave identical vectors")
	}

	t.Logf("model identity %s", c.ModelID())
}
