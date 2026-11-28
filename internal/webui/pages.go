package webui

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/atomicpages/wayfindr/internal/admin"
	"github.com/atomicpages/wayfindr/internal/store"
	"github.com/atomicpages/wayfindr/internal/upstream"
)

const (
	// callsLimit is how many calls the calls page lists.
	callsLimit = 100
	// searchSize is how many tools the search page ranks.
	searchSize = 10
)

// view is what the layout reads: the sidebar, the top bar, a toast, and the
// page's own data in Body.
type view struct {
	Title string
	// Nav is the active sidebar item: connectors, calls, search, tokens,
	// cloud-agents.
	Nav string
	// Parent, when set, is the breadcrumb above Title.
	Parent, ParentHref string
	Version            string
	Listen             string
	// Need is how many connectors need the user.
	Need  int
	CSRF  string
	Toast string
	Body  any
}

// badge is a status pill: a CSS class (ok, warn, err, info) and its word.
type badge struct{ Class, Label string }

// base fetches the status every page's sidebar shows and starts the view.
func (c *console) base(r *http.Request, s sess, nav, title string) (view, admin.Status, error) {
	st, err := c.Admin.Status(r.Context())

	if err != nil {
		return view{}, admin.Status{}, err
	}

	v := shell(r, s, nav, title)
	v.Version = st.Version

	for _, cn := range st.Connectors {
		if needsUser(cn.State) {
			v.Need++
		}
	}

	return v, st, nil
}

// shell is a page's frame before the daemon's status is known: what base
// fills in first, and what a page falls back on when the status can't be read.
func shell(r *http.Request, s sess, nav, title string) view {
	return view{Title: title, Nav: nav, Listen: r.Host, CSRF: s.csrf}
}

// pickedSet is the set of names a form's connector checkboxes sent, which
// refills the form after a refusal.
func pickedSet(picked []string) map[string]bool {
	set := make(map[string]bool, len(picked))

	for _, p := range picked {
		set[p] = true
	}

	return set
}

// fail renders the error page with status and msg.
func (c *console) fail(w http.ResponseWriter, r *http.Request, s sess, status int, msg string) {
	v := view{Title: "Something went wrong", CSRF: s.csrf, Listen: r.Host, Body: msg}

	if st, err := c.Admin.Status(r.Context()); err == nil {
		v.Version = st.Version
	}

	c.render(w, status, "error", v)
}

// failErr renders err with the status and sentence message gives it.
func (c *console) failErr(w http.ResponseWriter, r *http.Request, s sess, err error) {
	status, msg := c.message(r, err)
	c.fail(w, r, s, status, msg)
}

// needsUser reports whether a connector in state waits on the user rather
// than on time.
func needsUser(st upstream.State) bool {
	switch st {
	case upstream.StateNeedsAuth, upstream.StateAuthError, upstream.StateUnreachable:
		return true
	case upstream.StateStarting, upstream.StateReady:
		return false
	}

	return false
}

// stateBadge words a connector state for the console.
func stateBadge(st upstream.State) badge {
	switch st {
	case upstream.StateReady:
		return badge{"ok", "Ready"}
	case upstream.StateStarting:
		return badge{"info", "Starting"}
	case upstream.StateNeedsAuth:
		return badge{"warn", "Sign-in needed"}
	case upstream.StateAuthError:
		return badge{"err", "Sign-in failed"}
	case upstream.StateUnreachable:
		return badge{"err", "Can't reach"}
	}

	return badge{"info", string(st)}
}

// connectorRow is one connector as the console shows it.
type connectorRow struct {
	Name   string
	Logo   logo
	Badge  badge
	Ready  bool
	Advice *admin.Advice
	// Warnings are failures ahead; urgent ones show, notes do not.
	Warnings  []admin.Warning
	Tools     int
	Hidden    int
	LastSync  string
	SyncError string
	Expires   string
	// SignIn is set when an auth flow fixes the connector or a warning:
	// the daemon put an auth link in the advice.
	SignIn      bool
	SignInLabel string
	// SignOut is set when the connector holds an OAuth token.
	SignOut bool
}

func (c *console) row(cn admin.Connector) connectorRow {
	now := c.Now()
	r := connectorRow{
		Name:   cn.Name,
		Logo:   c.brands.logo(cn.Name),
		Badge:  stateBadge(cn.State),
		Ready:  cn.State == upstream.StateReady,
		Advice: cn.Advice,
		Tools:  cn.Tools,
		Hidden: cn.HiddenTools,
	}

	if cn.LastSync != nil {
		r.LastSync = ago(now, cn.LastSync.At)
		r.SyncError = cn.LastSync.Error
	}

	if cn.TokenExpiresAt != nil {
		r.Expires = date(now, *cn.TokenExpiresAt)
		r.SignOut = true
	}

	if cn.Advice != nil && cn.Advice.Link != "" {
		r.SignIn, r.SignInLabel = true, "Sign in"
	}

	for _, w := range cn.Warnings {
		if w.Note {
			continue
		}

		r.Warnings = append(r.Warnings, w)

		if w.Link != "" && !r.SignIn {
			r.SignIn, r.SignInLabel = true, "Sign in again"
		}
	}

	return r
}

// connectorsView is the connectors page's data.
type connectorsView struct {
	Rows                        []connectorRow
	Ready, Total, Need, Exposed int
	Hidden                      int
	NeedNames                   string
	ConfigError                 string
	Orphaned                    []string
}

// connectors serves GET /ui/: every connector, the ones that need the user
// first.
func (c *console) connectors(w http.ResponseWriter, r *http.Request, s sess) {
	v, st, err := c.base(r, s, "connectors", "Connectors")

	if err != nil {
		c.failErr(w, r, s, err)

		return
	}

	body := connectorsView{Total: len(st.Connectors), ConfigError: st.ConfigError, Orphaned: st.OrphanedCredentials}
	var need []string

	for _, cn := range st.Connectors {
		row := c.row(cn)
		body.Rows = append(body.Rows, row)

		if row.Ready {
			body.Ready++
			body.Exposed += row.Tools
			body.Hidden += row.Hidden
		}

		if needsUser(cn.State) {
			need = append(need, cn.Name)
		}
	}

	slices.SortStableFunc(body.Rows, func(a, b connectorRow) int { return rank(a) - rank(b) })
	body.Need = len(need)
	body.NeedNames = strings.Join(need, ", ")
	v.Toast = toast(r)
	v.Body = body
	c.render(w, http.StatusOK, "connectors", v)
}

// rank orders rows: the ones that need the user, then starting, then
// ready, each kept in name order.
func rank(r connectorRow) int {
	switch {
	case r.Badge.Class == "err":
		return 0
	case r.Badge.Class == "warn":
		return 1
	case !r.Ready:
		return 2
	default:
		return 3
	}
}

// connectorView is a connector page's data.
type connectorView struct {
	Row connectorRow
	// Tools is the live listing; ToolsError is why there is none.
	Tools      []toolRow
	ToolsError string
}

// toolRow is one tool on a connector page: the listing's entry and its
// definition indented for reading, or "" when there is none.
type toolRow struct {
	admin.ToolEntry
	Spec string
}

// toolRows indents each tool's definition. One that isn't valid JSON gets
// no Spec, so its row shows no definition rather than garbage.
func toolRows(tools []admin.ToolEntry) []toolRow {
	rows := make([]toolRow, len(tools))

	for i, t := range tools {
		rows[i].ToolEntry = t

		var buf bytes.Buffer

		if len(t.Definition) > 0 && json.Indent(&buf, t.Definition, "", "  ") == nil {
			rows[i].Spec = buf.String()
		}
	}

	return rows
}

// connector serves GET /ui/connectors/{name}: one connector and its tools.
func (c *console) connector(w http.ResponseWriter, r *http.Request, s sess) {
	name := r.PathValue("name")
	v, st, err := c.base(r, s, "connectors", name)

	if err != nil {
		c.failErr(w, r, s, err)

		return
	}

	i := slices.IndexFunc(st.Connectors, func(cn admin.Connector) bool { return cn.Name == name })

	if i < 0 {
		c.fail(w, r, s, http.StatusNotFound, fmt.Sprintf("No connector named %q is configured.", name))

		return
	}

	body := connectorView{Row: c.row(st.Connectors[i])}

	if list, err := c.Admin.Tools(r.Context(), name); err != nil {
		_, body.ToolsError = c.message(r, err)
	} else {
		body.Tools = toolRows(list.Tools)
	}

	v.Parent, v.ParentHref = "Connectors", "/ui/"
	v.Toast = toast(r)
	v.Body = body
	c.render(w, http.StatusOK, "connector", v)
}

// signIn serves POST /ui/connectors/{name}/sign-in. The form opens a new
// tab, which this sends on to the authorization server, or, for a
// connector whose grant is the device code, shows the user code and where
// to enter it; the connectors page picks up the result on its next
// refresh.
func (c *console) signIn(w http.ResponseWriter, r *http.Request, s sess) {
	f, err := c.Admin.StartAuth(r.Context(), r.PathValue("name"), admin.AuthStart{})

	if err != nil {
		c.failErr(w, r, s, err)

		return
	}

	if f.Device() {
		c.deviceCode(w, r, s, f)

		return
	}

	// Like the CLI's opener: never send the browser to another scheme.
	if u, err := url.Parse(f.AuthorizeURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		c.fail(w, r, s, http.StatusBadGateway, "The authorization server gave a sign-in address that is not a web page.")

		return
	}

	http.Redirect(w, r, f.AuthorizeURL, http.StatusSeeOther)
}

// deviceView is the device code page's data.
type deviceView struct {
	Connector string
	UserCode  string
	// URI is where the code is entered: the complete verification URI
	// when the server sent one.
	URI string
	// Expires words when the code expires.
	Expires string
}

// deviceCode renders a started device code flow: the user code, and a link
// to the verification URI, which auth has checked is https or loopback
// http.
func (c *console) deviceCode(w http.ResponseWriter, r *http.Request, s sess, f admin.AuthFlow) {
	uri := cmp.Or(f.VerificationURIComplete, f.VerificationURI)

	// Like the CLI's opener: never link to another scheme.
	if u, err := url.Parse(uri); err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		c.fail(w, r, s, http.StatusBadGateway, "The authorization server gave a sign-in address that is not a web page.")

		return
	}

	v, _, err := c.base(r, s, "connectors", "Sign in to "+f.Connector)

	if err != nil {
		c.failErr(w, r, s, err)

		return
	}

	v.Parent, v.ParentHref = "Connectors", "/ui/"
	v.Body = deviceView{Connector: f.Connector, UserCode: f.UserCode, URI: uri, Expires: clock(c.Now(), f.ExpiresAt)}
	c.render(w, http.StatusOK, "device", v)
}

// signOut serves POST /ui/connectors/{name}/sign-out: it deletes the
// connector's OAuth credential, as `wayfindr auth logout` does.
func (c *console) signOut(w http.ResponseWriter, r *http.Request, s sess) {
	name := r.PathValue("name")

	if _, err := c.Admin.Revoke(r.Context(), name); err != nil {
		c.failErr(w, r, s, err)

		return
	}

	http.Redirect(w, r, "/ui/connectors/"+url.PathEscape(name)+"?"+url.Values{"done": {"signed-out"}, "name": {name}}.Encode(), http.StatusSeeOther)
}

// callRow is one call as the calls page shows it.
type callRow struct {
	When      string
	Connector string
	Logo      logo
	Tool      string
	Token     string
	Duration  string
	// Size is the result's size in binary units, such as 312 B or 1.2 MiB;
	// empty when not known.
	Size  string
	Badge badge
	Why   string
	// Error is the error text the agent got back, shown when the row is
	// opened; empty for a call that is ok or has none.
	Error string
}

// callsView is the calls page's data.
type callsView struct {
	Rows       []callRow
	Off        bool
	Retention  string
	Connectors []string
	Connector  string
	Failed     bool
}

// calls serves GET /ui/calls?connector=&result=failed.
func (c *console) calls(w http.ResponseWriter, r *http.Request, s sess) {
	v, st, err := c.base(r, s, "calls", "Calls")

	if err != nil {
		c.failErr(w, r, s, err)

		return
	}

	q := r.URL.Query()
	body := callsView{Connector: q.Get("connector"), Failed: q.Get("result") == "failed"}

	for _, cn := range st.Connectors {
		body.Connectors = append(body.Connectors, cn.Name)
	}

	cq := admin.CallsQuery{ErrorsOnly: body.Failed, Limit: callsLimit}

	if body.Connector != "" {
		cq.Connectors = []string{body.Connector}
	}

	list, err := c.Admin.Calls(r.Context(), cq)

	if err != nil {
		c.failErr(w, r, s, err)

		return
	}

	retention, _ := time.ParseDuration(list.Retention)
	body.Off = retention == 0
	body.Retention = days(retention)
	now := c.Now()

	for _, call := range list.Calls {
		row := callRow{
			When:      clock(now, call.StartedAt),
			Connector: call.Connector,
			Logo:      c.brands.logo(call.Connector),
			Tool:      call.Tool,
			Token:     call.TokenName,
			Badge:     resultBadge(call.Result),
			Why:       call.Case,
			Error:     call.Error,
		}

		if call.DurationMS != nil {
			row.Duration = fmt.Sprintf("%d ms", *call.DurationMS)
		}

		if call.ResultBytes != nil {
			row.Size = admin.FormatBytes(*call.ResultBytes)
		}

		body.Rows = append(body.Rows, row)
	}

	v.Body = body
	c.render(w, http.StatusOK, "calls", v)
}

// resultBadge words how a call ended.
func resultBadge(res store.CallResult) badge {
	switch res {
	case store.CallOK:
		return badge{"ok", "OK"}
	case store.CallToolError:
		return badge{"warn", "Tool error"}
	case store.CallRejected:
		return badge{"err", "Refused"}
	case store.CallFailed:
		return badge{"err", "Failed"}
	case store.CallInputRequired:
		return badge{"info", "Asked for input"}
	}

	return badge{"info", string(res)}
}

// searchRow is one ranked tool.
type searchRow struct {
	Rank      int
	Connector string
	Tool      string
	Logo      logo
	Title     string
	Summary   string
	// Fused is the fused score; Share is it as a percentage of the top
	// one, for the bar.
	Fused       string
	Share       int
	LexicalRank int
	VectorRank  int
}

// searchView is the search page's data.
type searchView struct {
	Query       string
	Searched    bool
	Error       string
	Rows        []searchRow
	Lexical     bool
	Reason      string
	Unavailable []admin.SearchUnavailable
	Warnings    []admin.SearchWarning
}

// search serves GET /ui/search?q=: what search_tools returns for q, with
// each tool's scores.
func (c *console) search(w http.ResponseWriter, r *http.Request, s sess) {
	v, _, err := c.base(r, s, "search", "Search")

	if err != nil {
		c.failErr(w, r, s, err)

		return
	}

	body := searchView{Query: strings.TrimSpace(r.URL.Query().Get("q"))}

	if body.Query != "" {
		body.Searched = true
		res, err := c.Admin.Search(r.Context(), admin.SearchRequest{Query: body.Query, Size: searchSize, Explain: true})

		if err != nil {
			_, body.Error = c.message(r, err)
		} else {
			body.Rows = c.searchRows(res.Tools)
			body.Lexical = res.Embedding.Mode != "on"
			body.Reason = res.Embedding.Reason
			body.Unavailable = res.Unavailable
			body.Warnings = res.Warnings
		}
	}

	v.Body = body
	c.render(w, http.StatusOK, "search", v)
}

func (c *console) searchRows(tools []admin.SearchTool) []searchRow {
	var top float64

	for _, t := range tools {
		if t.Score != nil && t.Score.Fused > top {
			top = t.Score.Fused
		}
	}

	rows := make([]searchRow, 0, len(tools))

	for i, t := range tools {
		connector, tool, _ := strings.Cut(t.Name, ".")
		row := searchRow{Rank: i + 1, Connector: connector, Tool: tool, Logo: c.brands.logo(connector), Title: t.Title, Summary: t.Summary}

		if t.Score != nil {
			row.Fused = fmt.Sprintf("%.4f", t.Score.Fused)
			row.LexicalRank, row.VectorRank = t.Score.LexicalRank, t.Score.VectorRank

			if top > 0 {
				// Rounded to a step of 5 so the bar is one of the
				// stylesheet's classes; the console allows no inline style.
				row.Share = int(t.Score.Fused/top*20+0.5) * 5
			}
		}

		rows = append(rows, row)
	}

	return rows
}

// tokensView is the client tokens page's data.
type tokensView struct {
	Tokens     []tokenRow
	Connectors []string
	// Created is the token just created, whose value shows this once.
	Created *admin.TokenCreated
	// Error, Name, and Picked refill the form after a refusal.
	Error  string
	Name   string
	Picked map[string]bool
}

// tokenRow is one scoped client token.
type tokenRow struct {
	Name       string
	Connectors []string
	Remote     bool
	// OAuth marks an OAuth grant (ADR 0022); ClientHost is its client's
	// host.
	OAuth      bool
	ClientHost string
	Created    string
	LastUsed   string
}

// revokeForm is what the "revoke" template needs for one token row.
type revokeForm struct {
	CSRF, Name string
	// Return is where the revoke sends the browser back to: "cloud-agents",
	// or "" for the Client tokens page.
	Return string
}

// revokeFor builds the "revoke" template's data.
func revokeFor(csrf, name, ret string) revokeForm {
	return revokeForm{CSRF: csrf, Name: name, Return: ret}
}

// pickerView is what the "picker" template needs: the connectors to list and
// which are ticked.
type pickerView struct {
	Connectors []string
	Picked     map[string]bool
}

// pickerFor builds the "picker" template's data.
func pickerFor(connectors []string, picked map[string]bool) pickerView {
	return pickerView{Connectors: connectors, Picked: picked}
}

// tokenRowFor words t for a table row, relative to now.
func tokenRowFor(now time.Time, t admin.Token) tokenRow {
	row := tokenRow{Name: t.Name, Connectors: t.Connectors, Remote: t.Remote, Created: date(now, t.CreatedAt), LastUsed: "Never"}

	if t.Kind == admin.TokenKindOAuth {
		row.OAuth = true

		if u, err := url.Parse(t.Client); err == nil {
			row.ClientHost = u.Host
		}
	}

	if t.LastUsedAt != nil {
		row.LastUsed = ago(now, *t.LastUsedAt)
	}

	return row
}

// tokens serves GET /ui/tokens.
func (c *console) tokens(w http.ResponseWriter, r *http.Request, s sess) {
	c.renderTokens(w, r, s, tokensView{}, http.StatusOK)
}

func (c *console) renderTokens(w http.ResponseWriter, r *http.Request, s sess, body tokensView, status int) {
	v, st, err := c.base(r, s, "tokens", "Client tokens")

	if err != nil {
		c.failErr(w, r, s, err)

		return
	}

	list, err := c.Admin.ListTokens(r.Context())

	if err != nil {
		c.failErr(w, r, s, err)

		return
	}

	now := c.Now()

	for _, t := range list.Tokens {
		body.Tokens = append(body.Tokens, tokenRowFor(now, t))
	}

	for _, cn := range st.Connectors {
		body.Connectors = append(body.Connectors, cn.Name)
	}

	v.Toast = toast(r)
	v.Body = body
	c.render(w, status, "tokens", v)
}

// createToken serves POST /ui/tokens. The new token's value is in this
// answer only, so it renders the page rather than redirecting.
func (c *console) createToken(w http.ResponseWriter, r *http.Request, s sess) {
	name := strings.TrimSpace(r.PostForm.Get("name"))
	picked := r.PostForm["connector"]
	created, err := c.Admin.CreateToken(r.Context(), admin.TokenSpec{Name: name, Connectors: picked})

	if err != nil {
		status, msg := c.message(r, err)
		body := tokensView{Error: msg, Name: name, Picked: pickedSet(picked)}
		c.renderTokens(w, r, s, body, status)

		return
	}

	c.renderTokens(w, r, s, tokensView{Created: &created}, http.StatusOK)
}

// revokeToken serves POST /ui/tokens/{name}/revoke.
func (c *console) revokeToken(w http.ResponseWriter, r *http.Request, s sess) {
	name := r.PathValue("name")

	if _, err := c.Admin.RevokeToken(r.Context(), name); err != nil {
		c.failErr(w, r, s, err)

		return
	}

	// The Cloud agents page lists remote tokens and sends its revokes back to
	// itself; any other value of return is ignored.
	dest := "/ui/tokens"

	if r.PostForm.Get("return") == "cloud-agents" {
		dest = cloudPath
	}

	http.Redirect(w, r, dest+"?"+url.Values{"done": {"revoked"}, "name": {name}}.Encode(), http.StatusSeeOther)
}

// toast is the confirmation a redirect after an action asked for, from a
// closed set, so a link can't make the console say anything else.
func toast(r *http.Request) string {
	q := r.URL.Query()
	name := q.Get("name")

	switch q.Get("done") {
	case "signed-out":
		return "Signed out of " + name + "."
	case "revoked":
		return "Revoked " + name + ". Clients using it are refused from their next request."
	case "added":
		switch q.Get("oauth") {
		case admin.OAuthDetected:
			return "Added " + name + ". It uses OAuth, so sign in to finish."
		case admin.OAuthUnreachable:
			return "Added " + name + ". Wayfindr couldn't reach it to check for OAuth."
		default:
			return "Added " + name + "."
		}
	default:
		return ""
	}
}

// ago words how long before now t was.
func ago(now, t time.Time) string {
	d := now.Sub(t)

	switch {
	case d < time.Minute:
		return "Just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d h ago", int(d.Hours()))
	default:
		return date(now, t)
	}
}

// date words a day: "Dec 28", with the year when it is not now's.
func date(now, t time.Time) string {
	t = t.In(now.Location())

	if t.Year() != now.Year() {
		return t.Format("Jan 2, 2006")
	}

	return t.Format("Jan 2")
}

// clock words a time of day, with its day when that is not today.
func clock(now, t time.Time) string {
	t = t.In(now.Location())

	if y, m, d := t.Date(); y == now.Year() && m == now.Month() && d == now.Day() {
		return t.Format("15:04:05")
	}

	return date(now, t) + " " + t.Format("15:04")
}

// countOf words n of noun: "1 connector", "3 connectors".
func countOf(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}

	return fmt.Sprintf("%d %ss", n, noun)
}

// days words a retention: "30 days", or hours below one day.
func days(d time.Duration) string {
	if d >= 24*time.Hour {
		n := int(d / (24 * time.Hour))

		if n == 1 {
			return "1 day"
		}

		return fmt.Sprintf("%d days", n)
	}

	return fmt.Sprintf("%d hours", int(d.Hours()))
}
