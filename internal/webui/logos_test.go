package webui

import (
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestBrandLogo(t *testing.T) {
	b, err := loadBrands()

	if err != nil {
		t.Fatalf("loadBrands: %v", err)
	}

	if len(b.slugs) < 7000 {
		t.Fatalf("the index has %d slugs, want more than 7000", len(b.slugs))
	}

	cdn := "https://cdn.jsdelivr.net/gh/glincker/thesvg@" + b.sha + "/public/icons/"

	for _, tc := range []struct {
		name string
		want logo
	}{
		{"gmail", logo{Letter: "G", Src: cdn + "gmail/default.svg"}},
		{"GitHub-Work", logo{Letter: "G", Src: cdn + "github/default.svg", Dark: cdn + "github/dark.svg"}},
		{"google_calendar", logo{Letter: "G", Src: cdn + "google-calendar/default.svg"}},
		{"google-calendar--work", logo{Letter: "G", Src: cdn + "google-calendar/default.svg"}},
		{"postgres", logo{Letter: "P", Src: cdn + "postgresql/default.svg"}},
		{"acme-internal", logo{Letter: "A"}},
		{"élan", logo{Letter: "É"}},
		{"", logo{Letter: "?"}},
	} {
		if diff := cmp.Diff(tc.want, b.logo(tc.name)); diff != "" {
			t.Errorf("logo(%q) (-want +got):\n%s", tc.name, diff)
		}
	}
}
