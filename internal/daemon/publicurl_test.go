package daemon_test

import (
	"bytes"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/atomicpages/wayfindr/internal/admin"
)

func TestSetPublicURLWritesTheFileAndApplies(t *testing.T) {
	const url = "https://wayfindr.example.com"

	h := newHost(t)
	c := addingDaemon(t, h)
	start := readConfig(t, h)

	got, err := c.PublicURL(t.Context())

	if err != nil || got.URL != "" {
		t.Fatalf("PublicURL before = %q, %v; want empty", got.URL, err)
	}

	t.Run("set writes the key and the running config has it at once", func(t *testing.T) {
		set, err := c.SetPublicURL(t.Context(), url)

		if err != nil || set.URL != url {
			t.Fatalf("SetPublicURL = %q, %v; want %q", set.URL, err, url)
		}

		file := readConfig(t, h)

		if !bytes.Contains(file, []byte(`public_url = "`+url+`"`)) || !bytes.Contains(file, []byte("listen")) {
			t.Errorf("config file after set:\n%s\nwant public_url added and listen kept", file)
		}

		if got, err := c.PublicURL(t.Context()); err != nil || got.URL != url {
			t.Errorf("PublicURL after set = %q, %v; want %q without waiting for the watcher", got.URL, err, url)
		}
	})

	t.Run("an invalid URL is 400 and leaves the file alone", func(t *testing.T) {
		before := readConfig(t, h)
		_, err := c.SetPublicURL(t.Context(), "http://wayfindr.example.com")

		var ae *admin.APIError

		if !errors.As(err, &ae) || ae.Status != http.StatusBadRequest || !strings.Contains(ae.Message, "https") {
			t.Fatalf("SetPublicURL error = %v, want 400 naming https", err)
		}

		if after := readConfig(t, h); !bytes.Equal(before, after) {
			t.Errorf("config file changed:\n%s", after)
		}
	})

	t.Run("clear removes the key", func(t *testing.T) {
		set, err := c.SetPublicURL(t.Context(), "")

		if err != nil || set.URL != "" {
			t.Fatalf("SetPublicURL(\"\") = %q, %v", set.URL, err)
		}

		if file := readConfig(t, h); !bytes.Equal(file, start) {
			t.Errorf("config file after set and clear:\n%s\nwant what it began as:\n%s", file, start)
		}

		if got, err := c.PublicURL(t.Context()); err != nil || got.URL != "" {
			t.Errorf("PublicURL after clear = %q, %v; want empty", got.URL, err)
		}
	})
}

// A person who sets server.public_url by hand and then sets the same value in
// the console must find the daemon running it: the file already holds the
// value, so nothing is written, but the running config has not caught up yet
// (the watcher waits for the file to be quiet).
func TestSetPublicURLReloadsWhenTheFileAlreadyHoldsTheValue(t *testing.T) {
	const url = "https://wayfindr.example.com"

	h := newHost(t)
	c := addingDaemon(t, h)
	file := readConfig(t, h)
	edited := bytes.Replace(file, []byte("[server]\n"), []byte("[server]\n  public_url = \""+url+"\"\n"), 1)

	if bytes.Equal(file, edited) {
		t.Fatalf("test setup: no [server] header to add the key to in:\n%s", file)
	}

	if err := os.WriteFile(h.paths.ConfigFile, edited, 0o600); err != nil {
		t.Fatal(err)
	}

	set, err := c.SetPublicURL(t.Context(), url)

	if err != nil || set.URL != url {
		t.Fatalf("SetPublicURL = %q, %v; want %q", set.URL, err, url)
	}

	if got, err := c.PublicURL(t.Context()); err != nil || got.URL != url {
		t.Errorf("PublicURL right after = %q, %v; want %q without waiting for the watcher", got.URL, err, url)
	}

	if after := readConfig(t, h); !bytes.Equal(after, edited) {
		t.Errorf("config file was rewritten:\n%s\nwant it left as written by hand:\n%s", after, edited)
	}
}
