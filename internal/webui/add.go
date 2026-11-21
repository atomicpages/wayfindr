package webui

import (
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/atomicpages/wayfindr/internal/admin"
)

// secretEnvRows is how many secret variable rows the add form has. More
// can be added with `wayfindr connector edit`.
const secretEnvRows = 3

// Form values of the add form's kind and auth choices.
const (
	kindURL     = "url"
	kindCommand = "command"
	authHeader  = "header"
)

// addForm is the add form's data: what was entered, so a refusal shows it
// again. Secret values are never kept here, so they never reach a page.
type addForm struct {
	Kind, Name, URL string
	// Auth is admin.AuthDetect, admin.AuthOAuth, authHeader, or
	// admin.AuthNone.
	Auth           string
	HeaderName     string
	SSE            bool
	ClientID       string
	Scopes         string
	Timeout        string
	Command        string
	Args           string
	Env            string
	SecretEnvNames []string
	// Error is why the add was refused; SecretsDropped says secret values
	// were entered and must be entered again.
	Error          string
	SecretsDropped bool
}

// reviewView is the review page's data: exactly what a command connector
// will run, and the token that confirms it.
type reviewView struct {
	Name      string
	Command   string
	Args      []string
	Env       []envVar
	SecretEnv []string
	Timeout   string
	Token     string
}

type envVar struct{ Name, Value string }

// addPage serves GET /ui/add: an empty form.
func (c *console) addPage(w http.ResponseWriter, r *http.Request, s sess) {
	c.renderAdd(w, r, s, http.StatusOK, blankForm())
}

func blankForm() addForm {
	return addForm{Kind: kindURL, Auth: admin.AuthDetect, HeaderName: "Authorization", SecretEnvNames: make([]string, secretEnvRows)}
}

func (c *console) renderAdd(w http.ResponseWriter, r *http.Request, s sess, status int, f addForm) {
	v, _, err := c.base(r, s, "connectors", "Add connector")

	if err != nil {
		c.failErr(w, r, s, err)

		return
	}

	for len(f.SecretEnvNames) < secretEnvRows {
		f.SecretEnvNames = append(f.SecretEnvNames, "")
	}

	v.Parent, v.ParentHref = "Connectors", "/ui/"
	v.Body = f
	c.render(w, status, "add", v)
}

// add serves POST /ui/add. A URL connector is added at once. A command
// connector runs code on this machine, so it is kept for review instead,
// and only POST /ui/add/confirm adds it (ADR 0017).
func (c *console) add(w http.ResponseWriter, r *http.Request, s sess) {
	spec, f, msg := parseAddForm(r.PostForm)

	if msg != "" {
		f.Error = msg
		c.renderAdd(w, r, s, http.StatusBadRequest, f)

		return
	}

	if f.Kind == kindURL {
		c.finishAdd(w, r, s, spec, f)

		return
	}

	token, ok := c.Sessions.pend(s.id, spec)

	if !ok {
		toLogin(w, r)

		return
	}

	v, _, err := c.base(r, s, "connectors", "Review "+spec.Name)

	if err != nil {
		c.failErr(w, r, s, err)

		return
	}

	rv := reviewView{Name: spec.Name, Command: spec.Command, Args: spec.Args, Timeout: spec.Timeout, Token: token}

	for _, k := range slices.Sorted(maps.Keys(spec.Env)) {
		rv.Env = append(rv.Env, envVar{k, spec.Env[k]})
	}

	rv.SecretEnv = slices.Sorted(maps.Keys(spec.SecretEnv))
	v.Parent, v.ParentHref = "Add connector", "/ui/add"
	v.Body = rv
	c.render(w, http.StatusOK, "review", v)
}

// confirmAdd serves POST /ui/add/confirm: it adds the command connector the
// review showed, once, for the session it was shown to.
func (c *console) confirmAdd(w http.ResponseWriter, r *http.Request, s sess) {
	spec, ok := c.Sessions.take(s.id, r.PostForm.Get("token"))

	if !ok {
		c.Logger.Warn().Msg("console add confirmed without a live review token")
		c.fail(w, r, s, http.StatusForbidden, "This review has expired or was already used. Add the connector again.")

		return
	}

	c.finishAdd(w, r, s, spec, formOf(spec))
}

// backToAdd serves POST /ui/add/back: it drops the review and shows the
// form again with what was entered, secret values aside.
func (c *console) backToAdd(w http.ResponseWriter, r *http.Request, s sess) {
	spec, ok := c.Sessions.take(s.id, r.PostForm.Get("token"))

	if !ok {
		http.Redirect(w, r, "/ui/add", http.StatusSeeOther)

		return
	}

	c.renderAdd(w, r, s, http.StatusOK, formOf(spec))
}

// finishAdd asks the daemon to add spec, then shows the new connector, or
// the form again with the refusal.
func (c *console) finishAdd(w http.ResponseWriter, r *http.Request, s sess, spec admin.ConnectorSpec, f addForm) {
	added, err := c.Admin.AddConnector(r.Context(), spec)

	if err != nil {
		status, msg := c.message(r, err)
		f.Error = msg
		c.renderAdd(w, r, s, status, f)

		return
	}

	q := url.Values{"done": {"added"}, "name": {added.Result.Name}, "oauth": {added.OAuth}}
	http.Redirect(w, r, "/ui/connectors/"+url.PathEscape(added.Result.Name)+"?"+q.Encode(), http.StatusSeeOther)
}

// parseAddForm reads the add form into the spec it asks for and the form
// to show again. msg is why the form can't be sent as entered. Fields of
// the other kind are ignored: the form hides them, but still posts them.
func parseAddForm(form url.Values) (spec admin.ConnectorSpec, f addForm, msg string) {
	get := func(k string) string { return strings.TrimSpace(form.Get(k)) }
	f = addForm{
		Kind: get("kind"), Name: get("name"), URL: get("url"), Auth: get("auth"), HeaderName: get("header_name"),
		SSE: form.Get("transport") == "sse", ClientID: get("client_id"), Scopes: get("scopes"), Timeout: get("timeout"),
		Command: get("command"), Args: form.Get("args"), Env: form.Get("env"),
	}
	spec = admin.ConnectorSpec{Name: f.Name, Timeout: f.Timeout}

	for i := range secretEnvRows {
		f.SecretEnvNames = append(f.SecretEnvNames, get(fmt.Sprintf("secret_env_name_%d", i)))
	}

	// Any secret value entered is dropped from a form shown again.
	f.SecretsDropped = form.Get("header_value") != "" || form.Get("client_secret") != ""

	for i := range secretEnvRows {
		f.SecretsDropped = f.SecretsDropped || form.Get(fmt.Sprintf("secret_env_value_%d", i)) != ""
	}

	switch f.Kind {
	case kindURL:
		spec.URL = f.URL

		if f.SSE {
			spec.Transport = "sse"
		}

		switch f.Auth {
		case admin.AuthDetect, admin.AuthNone, "":
			spec.Auth = f.Auth
		case admin.AuthOAuth:
			spec.Auth = admin.AuthOAuth
			spec.ClientID = f.ClientID
			spec.ClientSecret = form.Get("client_secret")
			spec.Scopes = strings.FieldsFunc(f.Scopes, func(r rune) bool { return r == ',' || r == ' ' })
		case authHeader:
			spec.Auth = admin.AuthNone

			if f.HeaderName == "" || form.Get("header_value") == "" {
				return spec, f, "Give the header's name and its value, such as Authorization and Bearer <key>."
			}

			spec.SecretHeaders = map[string]string{f.HeaderName: form.Get("header_value")}
		default:
			return spec, f, "Choose how the connector signs in."
		}
	case kindCommand:
		spec.Command = f.Command

		for _, line := range strings.Split(f.Args, "\n") {
			if a := strings.TrimRight(line, "\r"); strings.TrimSpace(a) != "" {
				spec.Args = append(spec.Args, a)
			}
		}

		for _, line := range strings.Split(f.Env, "\n") {
			line = strings.TrimSpace(line)

			if line == "" {
				continue
			}

			k, v, ok := strings.Cut(line, "=")

			if !ok || strings.TrimSpace(k) == "" {
				return spec, f, fmt.Sprintf("Write each environment variable as NAME=value; %.40q is not.", line)
			}

			if spec.Env == nil {
				spec.Env = map[string]string{}
			}

			spec.Env[strings.TrimSpace(k)] = v
		}

		for i, name := range f.SecretEnvNames {
			value := form.Get(fmt.Sprintf("secret_env_value_%d", i))

			switch {
			case name == "" && value == "":
				continue
			case name == "" || value == "":
				return spec, f, "Give each secret variable both a name and a value."
			}

			if spec.SecretEnv == nil {
				spec.SecretEnv = map[string]string{}
			}

			spec.SecretEnv[name] = value
		}
	default:
		return spec, f, "Choose a remote URL or a local command."
	}

	return spec, f, ""
}

// formOf is the form that would send spec, its secret values aside.
func formOf(spec admin.ConnectorSpec) addForm {
	f := blankForm()
	f.Name, f.Timeout = spec.Name, spec.Timeout

	if spec.Command != "" {
		f.Kind, f.Command, f.Args = kindCommand, spec.Command, strings.Join(spec.Args, "\n")
		var env []string

		for _, k := range slices.Sorted(maps.Keys(spec.Env)) {
			env = append(env, k+"="+spec.Env[k])
		}

		f.Env = strings.Join(env, "\n")
		f.SecretEnvNames = slices.Sorted(maps.Keys(spec.SecretEnv))
		f.SecretsDropped = len(spec.SecretEnv) > 0

		return f
	}

	f.URL, f.SSE, f.ClientID, f.Scopes = spec.URL, spec.Transport == "sse", spec.ClientID, strings.Join(spec.Scopes, " ")
	f.Auth = spec.Auth

	if f.Auth == "" {
		f.Auth = admin.AuthDetect
	}

	for k := range spec.SecretHeaders {
		f.Auth, f.HeaderName = authHeader, k
	}

	f.SecretsDropped = len(spec.SecretHeaders) > 0 || spec.ClientSecret != ""

	return f
}
