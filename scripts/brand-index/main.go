// Command brand-index writes internal/webui/brands.gz: theSVG's brand slugs
// at one commit, which the console's logo URLs pin (internal/webui/logos.go).
// Run it with make brands; the build never fetches anything.
package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"
)

var (
	shaRE  = regexp.MustCompile(`^[0-9a-f]{40}$`)
	slugRE = regexp.MustCompile(`^[a-z0-9-]+$`)
)

// icon is one entry of theSVG's src/data/icons.json. Every slug has
// public/icons/<slug>/default.svg; variants names the others it has.
type icon struct {
	Slug     string            `json:"slug"`
	Variants map[string]string `json:"variants"`
}

func main() {
	sha := flag.String("sha", "", "theSVG commit (default: the head of main)")
	out := flag.String("o", "internal/webui/brands.gz", "output file")
	flag.Parse()

	if err := run(context.Background(), *sha, *out); err != nil {
		fmt.Fprintln(os.Stderr, "brand-index:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, sha, out string) error {
	c := &http.Client{Timeout: 2 * time.Minute}

	if sha == "" {
		var head struct {
			SHA string `json:"sha"`
		}

		if err := getJSON(ctx, c, "https://api.github.com/repos/glincker/thesvg/commits/main", &head); err != nil {
			return err
		}

		sha = head.SHA
	}

	if !shaRE.MatchString(sha) {
		return fmt.Errorf("commit %q is not a full SHA", sha)
	}

	// The manifest at the same commit the URLs pin, so they agree.
	var icons []icon

	if err := getJSON(ctx, c, "https://cdn.jsdelivr.net/gh/glincker/thesvg@"+sha+"/src/data/icons.json", &icons); err != nil {
		return err
	}

	slices.SortFunc(icons, func(a, b icon) int { return strings.Compare(a.Slug, b.Slug) })

	var sb strings.Builder

	fmt.Fprintf(&sb, "# thesvg %s\n", sha)

	for _, i := range icons {
		if !slugRE.MatchString(i.Slug) {
			return fmt.Errorf("slug %q is not [a-z0-9-]", i.Slug)
		}

		if _, ok := i.Variants["dark"]; ok {
			fmt.Fprintf(&sb, "%s\tdark\n", i.Slug)
		} else {
			fmt.Fprintln(&sb, i.Slug)
		}
	}

	var buf bytes.Buffer

	zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)

	if err != nil {
		return err
	}

	if _, err := zw.Write([]byte(sb.String())); err != nil {
		return err
	}

	if err := zw.Close(); err != nil {
		return err
	}

	if err := os.WriteFile(out, buf.Bytes(), 0o600); err != nil {
		return err
	}

	fmt.Printf("brand-index: %d slugs at %s to %s\n", len(icons), sha, out)

	return nil
}

func getJSON(ctx context.Context, c *http.Client, url string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)

	if err != nil {
		return err
	}

	resp, err := c.Do(req)

	if err != nil {
		return err
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}

	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		return fmt.Errorf("GET %s: %w", url, err)
	}

	return nil
}
