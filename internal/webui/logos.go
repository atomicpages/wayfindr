package webui

import (
	"bufio"
	"bytes"
	"compress/gzip"
	_ "embed"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// brandsGz is theSVG's brand slugs at one commit (scripts/brand-index, make
// brands): a "# thesvg <sha>" line, then "slug" or "slug\tdark" per line.
//
//go:embed brands.gz
var brandsGz []byte

// logo is how the console marks a connector: its first letter, with a
// theSVG logo over it when the name matches a brand. The browser loads the
// logo from jsDelivr; console.js hides one that can't load (offline, say),
// which shows the letter again.
type logo struct {
	Letter string
	// Src is the brand's default.svg; empty when no brand matches.
	Src string
	// Dark is the brand's dark.svg, for dark mode; empty when it has none.
	Dark string
}

// brandIndex is theSVG's slugs at the commit its logo URLs pin. Matching
// against it means the browser only asks the CDN for real brands, never
// for a connector's own name.
type brandIndex struct {
	sha   string
	slugs map[string]bool // slug → has a dark variant
}

// aliases maps common connector names that are not theSVG slugs to one.
func aliases() map[string]string {
	return map[string]string{"postgres": "postgresql", "pgsql": "postgresql"}
}

var (
	shaRE  = regexp.MustCompile(`^[0-9a-f]{40}$`)
	slugRE = regexp.MustCompile(`^[a-z0-9-]+$`)
)

// loadBrands reads the embedded index.
func loadBrands() (brandIndex, error) {
	zr, err := gzip.NewReader(bytes.NewReader(brandsGz))

	if err != nil {
		return brandIndex{}, fmt.Errorf("brand index: %w", err)
	}

	defer func() { _ = zr.Close() }()

	sc := bufio.NewScanner(zr)

	if !sc.Scan() {
		return brandIndex{}, errors.New("brand index: empty")
	}

	sha, ok := strings.CutPrefix(sc.Text(), "# thesvg ")

	if !ok || !shaRE.MatchString(sha) {
		return brandIndex{}, fmt.Errorf("brand index: bad header %q", sc.Text())
	}

	b := brandIndex{sha: sha, slugs: map[string]bool{}}

	for sc.Scan() {
		slug, flag, _ := strings.Cut(sc.Text(), "\t")

		if !slugRE.MatchString(slug) {
			return brandIndex{}, fmt.Errorf("brand index: bad slug %q", slug)
		}

		b.slugs[slug] = flag == "dark"
	}

	if err := sc.Err(); err != nil {
		return brandIndex{}, fmt.Errorf("brand index: %w", err)
	}

	return b, nil
}

// logo picks connector's logo. A connector's name is user-chosen, so this
// tries the whole name, then the name with trailing "-parts" dropped:
// "google-calendar-work", "google-calendar", "google".
func (b brandIndex) logo(connector string) logo {
	l := logo{Letter: "?"}

	if r, _ := utf8.DecodeRuneInString(connector); r != utf8.RuneError {
		l.Letter = string(unicode.ToUpper(r))
	}

	alias := aliases()

	for name := normalize(connector); name != ""; {
		slug := name

		if a, ok := alias[slug]; ok {
			slug = a
		}

		if dark, ok := b.slugs[slug]; ok {
			base := "https://cdn.jsdelivr.net/gh/glincker/thesvg@" + b.sha + "/public/icons/" + slug + "/"
			l.Src = base + "default.svg"

			if dark {
				l.Dark = base + "dark.svg"
			}

			return l
		}

		i := strings.LastIndexByte(name, '-')

		if i < 0 {
			break
		}

		name = strings.TrimRight(name[:i], "-")
	}

	return l
}

// normalize lowercases name, turns "_", "." and spaces into "-", drops
// every other character outside [a-z0-9-], and trims "-" from both ends.
func normalize(name string) string {
	var sb strings.Builder

	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			sb.WriteRune(r)
		case r == '_', r == '.', r == ' ':
			sb.WriteByte('-')
		}
	}

	return strings.Trim(sb.String(), "-")
}
