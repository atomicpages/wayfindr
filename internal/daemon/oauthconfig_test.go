package daemon_test

import (
	"bytes"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/atomicpages/wayfindr/internal/admin"
	"github.com/atomicpages/wayfindr/internal/config"
)

func TestSetOAuthEnabledWritesTheFileAndApplies(t *testing.T) {
	const url = "https://wayfindr.example.com"

	h := newHost(t)
	c := addingDaemon(t, h)
	start := readConfig(t, h)

	got, err := c.OAuthSettings(t.Context())

	if want := (admin.OAuthSettings{Clients: []string{config.DefaultOAuthClient}}); err != nil || !cmp.Equal(want, got) {
		t.Fatalf("OAuthSettings before = %+v, %v; want off with the default client", got, err)
	}

	t.Run("without a public URL it is 400 and leaves the file alone", func(t *testing.T) {
		_, err := c.SetOAuthEnabled(t.Context(), true)

		var ae *admin.APIError

		if !errors.As(err, &ae) || ae.Status != http.StatusBadRequest || !strings.Contains(ae.Message, "server.public_url") {
			t.Fatalf("SetOAuthEnabled error = %v, want 400 naming server.public_url", err)
		}

		if after := readConfig(t, h); !bytes.Equal(after, start) {
			t.Errorf("config file changed:\n%s", after)
		}
	})

	if _, err := c.SetPublicURL(t.Context(), url); err != nil {
		t.Fatalf("SetPublicURL: %v", err)
	}

	t.Run("on writes the table and the running config has it at once", func(t *testing.T) {
		set, err := c.SetOAuthEnabled(t.Context(), true)

		want := admin.OAuthSettings{Enabled: true, Clients: []string{config.DefaultOAuthClient}, Issuer: url}

		if err != nil || !cmp.Equal(want, set) {
			t.Fatalf("SetOAuthEnabled(true) = %+v, %v; want %+v", set, err, want)
		}

		if file := readConfig(t, h); !bytes.Contains(file, []byte("[server.oauth]\nenabled = true\n")) {
			t.Errorf("config file after enabling:\n%s\nwant a [server.oauth] table with enabled = true", file)
		}

		if got, err := c.OAuthSettings(t.Context()); err != nil || !got.Enabled {
			t.Errorf("OAuthSettings after enabling = %+v, %v; want enabled without waiting for the watcher", got, err)
		}
	})

	t.Run("the public URL can't be cleared while it is on", func(t *testing.T) {
		before := readConfig(t, h)
		_, err := c.SetPublicURL(t.Context(), "")

		var ae *admin.APIError

		if !errors.As(err, &ae) || ae.Status != http.StatusBadRequest || !strings.Contains(ae.Message, "turn it off first") {
			t.Fatalf("SetPublicURL(\"\") error = %v, want 400 saying to turn it off first", err)
		}

		if after := readConfig(t, h); !bytes.Equal(before, after) {
			t.Errorf("config file changed:\n%s", after)
		}
	})

	t.Run("off writes false and keeps the table", func(t *testing.T) {
		set, err := c.SetOAuthEnabled(t.Context(), false)

		if err != nil || set.Enabled {
			t.Fatalf("SetOAuthEnabled(false) = %+v, %v; want off", set, err)
		}

		if file := readConfig(t, h); !bytes.Contains(file, []byte("[server.oauth]\nenabled = false\n")) {
			t.Errorf("config file after disabling:\n%s\nwant enabled = false", file)
		}

		if _, err := c.SetPublicURL(t.Context(), ""); err != nil {
			t.Errorf("SetPublicURL(\"\") with OAuth off: %v", err)
		}
	})
}

// A person who turns server.oauth on by hand and then does the same in the
// console must find the daemon running it: the file already holds the value,
// so nothing is written, but the running config has not caught up yet (the
// watcher waits for the file to be quiet).
func TestSetOAuthEnabledReloadsWhenTheFileAlreadyHoldsTheValue(t *testing.T) {
	const url = "https://wayfindr.example.com"

	h := newHost(t)
	c := addingDaemon(t, h)

	if _, err := c.SetPublicURL(t.Context(), url); err != nil {
		t.Fatalf("SetPublicURL: %v", err)
	}

	edited := append(bytes.Clone(readConfig(t, h)), []byte("\n[server.oauth]\nenabled = true\n")...)

	if err := os.WriteFile(h.paths.ConfigFile, edited, 0o600); err != nil {
		t.Fatal(err)
	}

	set, err := c.SetOAuthEnabled(t.Context(), true)

	if err != nil || !set.Enabled {
		t.Fatalf("SetOAuthEnabled(true) = %+v, %v; want enabled", set, err)
	}

	if got, err := c.OAuthSettings(t.Context()); err != nil || !got.Enabled {
		t.Errorf("OAuthSettings right after = %+v, %v; want enabled without waiting for the watcher", got, err)
	}

	if after := readConfig(t, h); !bytes.Equal(after, edited) {
		t.Errorf("config file was rewritten:\n%s\nwant it left as written by hand:\n%s", after, edited)
	}
}
