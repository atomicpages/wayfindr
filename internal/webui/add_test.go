package webui_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/atomicpages/wayfindr/internal/admin"
	"github.com/atomicpages/wayfindr/internal/webui"
)

// tokenOf reads the review token from a review page.
func tokenOf(t *testing.T, body string) string {
	t.Helper()
	_, rest, ok := strings.Cut(body, `name="token" value="`)

	if !ok {
		t.Fatal("the page has no review token")
	}

	tok, _, _ := strings.Cut(rest, `"`)

	return tok
}

func TestAddForm(t *testing.T) {
	c := newConsole(t)
	ck := c.signIn(t)
	rec := c.get("/ui/add", ck)
	body := rec.Body.String()

	if rec.Code != http.StatusOK || !strings.Contains(body, `action="/ui/add"`) || !strings.Contains(body, `id="kind-command"`) {
		t.Fatalf("GET /ui/add = %d, want the add form", rec.Code)
	}

	if strings.Contains(body, "style=") {
		t.Error("the form has inline style, which the console's CSP refuses")
	}

	// htmx swaps no 4xx or 5xx answer by default, which would hide a
	// refused form's message behind a boosted submit.
	if !strings.Contains(body, `{"code":"[2345]..","swap":true,"error":false}`) {
		t.Error("htmx is not configured to swap the pages the console answers with an error status")
	}
}

func TestAddURLConnector(t *testing.T) {
	c := newConsole(t)
	ck := c.signIn(t)
	csrf := csrfOf(t, c.get("/ui/add", ck).Body.String())
	form := url.Values{
		"csrf": {csrf}, "kind": {"url"}, "name": {"linear"}, "url": {"https://mcp.linear.app/mcp"},
		"auth": {"header"}, "header_name": {"Authorization"}, "header_value": {"Bearer sk-live-123"},
		// The command fields are hidden for a URL connector, but still post.
		"command": {"rm"}, "args": {"-rf\n/"},
	}

	t.Run("adds it at once", func(t *testing.T) {
		rec := c.post("/ui/add", ck, form)

		if rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/ui/connectors/linear?") {
			t.Fatalf("add = %d to %q, want 303 to the connector", rec.Code, rec.Header().Get("Location"))
		}

		want := []admin.ConnectorSpec{{Name: "linear", URL: "https://mcp.linear.app/mcp", Auth: admin.AuthNone,
			SecretHeaders: map[string]string{"Authorization": "Bearer sk-live-123"}}}

		if diff := cmp.Diff(want, c.admin.adds); diff != "" {
			t.Errorf("specs sent (-want +got):\n%s", diff)
		}
	})

	t.Run("a refusal shows the form again without secret values", func(t *testing.T) {
		c.admin.addErr = &admin.APIError{Status: http.StatusConflict, Message: `A connector named "linear" is already configured. Pick another name.`}
		rec := c.post("/ui/add", ck, form)
		body := rec.Body.String()

		if rec.Code != http.StatusConflict || !strings.Contains(body, "already configured") || !strings.Contains(body, `value="linear"`) {
			t.Errorf("refused add = %d, want 409 with the message and the name kept", rec.Code)
		}

		if strings.Contains(body, "sk-live-123") || !strings.Contains(body, "Enter the secret values again") {
			t.Error("the secret value came back, or the page doesn't ask for it again")
		}
	})
}

func TestAddCommandConnectorNeedsAConfirm(t *testing.T) {
	c := newConsole(t)
	ck := c.signIn(t)
	csrf := csrfOf(t, c.get("/ui/add", ck).Body.String())
	form := url.Values{
		"csrf": {csrf}, "kind": {"command"}, "name": {"files"}, "command": {"npx"},
		"args": {"-y\r\n@modelcontextprotocol/server-filesystem\r\n/Users/me/My Docs\r\n"}, "env": {"LOG_LEVEL=info"},
		"secret_env_name_0": {"API_KEY"}, "secret_env_value_0": {"key-never-shown"},
		// The URL fields are hidden for a command connector, but still post.
		"auth": {"detect"}, "header_name": {"Authorization"},
	}
	review := func(t *testing.T) string {
		t.Helper()
		rec := c.post("/ui/add", ck, form)
		body := rec.Body.String()

		if rec.Code != http.StatusOK || !strings.Contains(body, "Add and run") {
			t.Fatalf("add = %d, want the review page", rec.Code)
		}

		return body
	}

	t.Run("the review shows exactly what runs, and adds nothing", func(t *testing.T) {
		body := review(t)

		for _, w := range []string{`<code class="argv">npx</code>`, `<code class="argv">-y</code>`, `<code class="argv">/Users/me/My Docs</code>`,
			`<code class="argv">LOG_LEVEL=info</code>`, `<code class="argv">API_KEY=••••••</code>`} {
			if !strings.Contains(body, w) {
				t.Errorf("the review lacks %s", w)
			}
		}

		if strings.Contains(body, "key-never-shown") {
			t.Error("the review shows the secret value")
		}

		if n := c.admin.addCount(); n != 0 {
			t.Errorf("added %d connectors before the confirm, want 0", n)
		}
	})

	t.Run("confirm adds it once", func(t *testing.T) {
		token := tokenOf(t, review(t))
		rec := c.post("/ui/add/confirm", ck, url.Values{"csrf": {csrf}, "token": {token}})

		if rec.Code != http.StatusSeeOther {
			t.Fatalf("confirm = %d, want 303", rec.Code)
		}

		want := admin.ConnectorSpec{Name: "files", Command: "npx", Args: []string{"-y", "@modelcontextprotocol/server-filesystem", "/Users/me/My Docs"},
			Env: map[string]string{"LOG_LEVEL": "info"}, SecretEnv: map[string]string{"API_KEY": "key-never-shown"}}

		if diff := cmp.Diff([]admin.ConnectorSpec{want}, c.admin.adds); diff != "" {
			t.Errorf("specs sent (-want +got):\n%s", diff)
		}

		if got := c.post("/ui/add/confirm", ck, url.Values{"csrf": {csrf}, "token": {token}}).Code; got != http.StatusForbidden {
			t.Errorf("the token again = %d, want 403", got)
		}

		if n := c.admin.addCount(); n != 1 {
			t.Errorf("added %d times, want once", n)
		}
	})

	t.Run("another session's token is refused", func(t *testing.T) {
		token := tokenOf(t, review(t))
		other := c.signIn(t)
		otherCSRF := csrfOf(t, c.get("/ui/add", other).Body.String())

		if got := c.post("/ui/add/confirm", other, url.Values{"csrf": {otherCSRF}, "token": {token}}).Code; got != http.StatusForbidden {
			t.Errorf("confirm from another session = %d, want 403", got)
		}
	})

	t.Run("an expired token is refused", func(t *testing.T) {
		before := c.admin.addCount()
		token := tokenOf(t, review(t))
		c.clock.advance(webui.PendingTTL)

		if got := c.post("/ui/add/confirm", ck, url.Values{"csrf": {csrf}, "token": {token}}).Code; got != http.StatusForbidden {
			t.Errorf("an expired confirm = %d, want 403", got)
		}

		if c.admin.addCount() != before {
			t.Error("an expired confirm added the connector")
		}
	})

	t.Run("back shows the form without secret values", func(t *testing.T) {
		token := tokenOf(t, review(t))
		rec := c.post("/ui/add/back", ck, url.Values{"csrf": {csrf}, "token": {token}})
		body := rec.Body.String()

		if !strings.Contains(body, `value="npx"`) || !strings.Contains(body, `value="API_KEY"`) || strings.Contains(body, "key-never-shown") {
			t.Error("back lost the command or the variable's name, or showed its value")
		}
	})

	t.Run("a malformed environment line is 400", func(t *testing.T) {
		bad := url.Values{"csrf": {csrf}, "kind": {"command"}, "name": {"x"}, "command": {"npx"}, "env": {"NOEQUALS"}}

		if got := c.post("/ui/add", ck, bad).Code; got != http.StatusBadRequest {
			t.Errorf("bad env = %d, want 400", got)
		}
	})
}
