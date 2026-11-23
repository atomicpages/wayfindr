package webui

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/atomicpages/wayfindr/internal/admin"
)

// The wizard's stages, in order (ADR 0021). The first four are pages of the
// wizard proper; stageDone shows the new token, the connection test, and the
// agent's setup text, because all three need the token's value, which only
// that answer holds. ChatGPT's path has no connectors or token stage: agent,
// url, stageOAuth, then stageDone, which holds no secret (ADR 0022).
const (
	stageAgent      = "agent"
	stageConnectors = "connectors"
	stageURL        = "url"
	stageToken      = "token"
	stageOAuth      = "oauth"
	stageDone       = "done"
)

// cloudPath is the Cloud agents page.
const cloudPath = "/ui/cloud-agents"

// Agent ids, the closed set of agents the wizard offers.
const (
	agentGrok = "grok"
	agentMuse = "muse"
	// agentChatGPT signs in with OAuth instead of a token: its custom
	// connector form takes no Authorization header (ADR 0022). The wizard
	// reads that from agentInfo.OAuth, not from this id.
	agentChatGPT = "chatgpt"
)

// agentInfo is one agent the wizard lists.
type agentInfo struct {
	ID, Name string
	// Blurb is the one line under the name; a Disabled agent says why in it.
	Blurb    string
	Disabled bool
	// DefaultName is the token's default name.
	DefaultName string
	// OAuth is set for an agent that signs in with OAuth instead of using a
	// token (ADR 0022): it has no connectors or token stage, and its done
	// stage holds no secret.
	OAuth bool
}

// flowStage places one of a path's stages on the progress list.
type flowStage struct {
	Stage string
	// Step is the stage's step number, one of the step constants.
	Step int
}

// path is the stages an agent's wizard goes through, with the step each is
// on, and the progress list's names, in step order. The token path's done
// stage is on stepTest and its page covers stepSetUp too.
func (a agentInfo) path() (stages []flowStage, names []string) {
	if a.OAuth {
		return []flowStage{
				{stageAgent, oauthStepAgent}, {stageURL, oauthStepURL}, {stageOAuth, oauthStepSignIn}, {stageDone, oauthStepConnect},
			},
			[]string{"Agent", "Public URL", "Sign-in", "Connect"}
	}

	return []flowStage{
			{stageAgent, stepAgent}, {stageConnectors, stepConnectors}, {stageURL, stepPublicURL}, {stageToken, stepToken}, {stageDone, stepTest},
		},
		[]string{"Agent", "Connectors", "Public URL", "Token", "Test", "Set up"}
}

// agents lists every agent in the order shown.
func agents() []agentInfo {
	return []agentInfo{
		{ID: agentGrok, Name: "Grok Bot", Blurb: "Adds Wayfindr as a custom connector with a header.", DefaultName: "grok-bot"},
		{ID: agentMuse, Name: "Muse", Blurb: "You ask Muse in chat to add Wayfindr as a custom connector.", DefaultName: "muse"},
		{ID: agentChatGPT, Name: "ChatGPT", Blurb: "Signs in with OAuth. You approve it on this computer.", OAuth: true},
	}
}

// agentByID finds an agent the wizard can set up; a disabled one is none.
func agentByID(id string) (agentInfo, bool) {
	for _, a := range agents() {
		if a.ID == id && !a.Disabled {
			return a, true
		}
	}

	return agentInfo{}, false
}

// agentRow is an agent in step 1.
type agentRow struct {
	agentInfo
	Checked bool
	// Connect offers ChatGPT's "Connect a client" button on its row: sign-in
	// is on, so connecting again needs no trip through the wizard.
	Connect bool
}

// stepRow is one entry of the progress list: State is done, current, or todo.
// Two steps can be current at once (Test and Set up), but only one, Aria,
// carries aria-current: WAI-ARIA expects one per set.
type stepRow struct {
	N           int
	Name, State string
	Aria        bool
}

// testView is a connection test's outcome in the words the page shows.
type testView struct {
	// Class is the alert's class: ok or err.
	Class string
	Title string
	// Prominent is set for proxy_treats_remote_as_local: the token worked,
	// but the tunnel hides that requests come from outside, which is a
	// security problem rather than a setup slip.
	Prominent bool
	Message   string
	Action    string
	URL       string
	// Detail is what a passing test found.
	Detail string
}

// setupView is what step 6 shows.
type setupView struct {
	// PublicURL is server.public_url, "" when unset; McpURL is where the
	// agent connects.
	PublicURL, McpURL string
	Token             string
	// Bearer is the Authorization header's value.
	Bearer string
	// MuseMessage is the chat message for Muse. It holds no secret.
	MuseMessage string
}

// cloudView is the Cloud agents page's data.
type cloudView struct {
	Stage string
	Steps []stepRow
	// Error is why the last action was refused.
	Error string

	Agents     []agentRow
	Agent      agentInfo
	Connectors []string
	Picked     map[string]bool
	// PickedList is Picked in the connectors' order.
	PickedList []string
	PublicURL  string
	TokenName  string
	// BackHref is the link to the stage before this one.
	BackHref string

	// Token is the new token's value. It is set only in the answers to the
	// create and test posts, and goes back to the server only in the test
	// form's body.
	Token string
	Test  *testView
	Setup setupView

	// StepN and StepTotal number the stage in its path ("Step 2 of 4"); the
	// wizard's own headings state theirs.
	StepN, StepTotal int
	// Notice is a confirmation shown above the stage.
	Notice string
	// ConnectOffer is whether ChatGPT's row on the first step has its
	// "Connect a client" button.
	ConnectOffer bool
	// ArmedUntil, when set, is when ChatGPT's connection arm ends, worded
	// for the page.
	ArmedUntil string

	Remote []tokenRow
	// RemoteUnavailable is set when the token list could not be read on the
	// done stage: the page shows a note in place of the list rather than
	// lose the new token to an error page.
	RemoteUnavailable bool
}

// The token path's steps, numbered as the progress list shows them.
const (
	stepAgent = iota + 1
	stepConnectors
	stepPublicURL
	stepToken
	stepTest
	stepSetUp
)

// The OAuth path's steps.
const (
	oauthStepAgent = iota + 1
	oauthStepURL
	oauthStepSignIn
	oauthStepConnect
)

// stageStep is the step number a stage is on in agent's path; a stage the
// path lacks is on the first step.
func stageStep(stage string, agent agentInfo) int {
	stages, _ := agent.path()

	for _, fs := range stages {
		if fs.Stage == stage {
			return fs.Step
		}
	}

	return stepAgent
}

// flowQuery is the wizard's place in a URL: the agent and connectors, never a
// secret.
func flowQuery(step, agent string, picked []string) string {
	q := url.Values{"step": {step}, "agent": {agent}}

	if len(picked) > 0 {
		q["connector"] = picked
	}

	return cloudPath + "?" + q.Encode()
}

// cloudAgents serves GET /ui/cloud-agents?step=&agent=&connector=. The
// stage comes from the query, which holds only the agent and the connector
// names, so a reload or the back button lands on the same stage.
func (c *console) cloudAgents(w http.ResponseWriter, r *http.Request, s sess) {
	q := r.URL.Query()
	body := cloudView{Stage: stageAgent}
	agent, ok := agentByID(q.Get("agent"))

	if !ok {
		c.renderCloud(w, r, s, body, http.StatusOK)

		return
	}

	body.Agent = agent
	step := q.Get("step")

	if step == "" || step == stageAgent {
		body.Stage = stageAgent
		c.renderCloud(w, r, s, body, http.StatusOK)

		return
	}

	if agent.OAuth {
		c.cloudChatGPT(w, r, s, body, step)

		return
	}

	st, err := c.Admin.Status(r.Context())

	if err != nil {
		c.failErr(w, r, s, err)

		return
	}

	body.Stage = stageConnectors
	body.Picked, body.PickedList = pick(st.Connectors, q["connector"])

	if step == stageConnectors {
		c.renderCloud(w, r, s, body, http.StatusOK)

		return
	}

	if len(body.PickedList) == 0 {
		body.Error = "Pick at least one connector for the agent to use."
		c.renderCloud(w, r, s, body, http.StatusOK)

		return
	}

	pu, err := c.Admin.PublicURL(r.Context())

	if err != nil {
		c.failErr(w, r, s, err)

		return
	}

	body.Stage = stageURL
	body.PublicURL = pu.URL

	if step == stageURL {
		c.renderCloud(w, r, s, body, http.StatusOK)

		return
	}

	if pu.URL == "" {
		body.Error = "Set your public URL first."
		c.renderCloud(w, r, s, body, http.StatusOK)

		return
	}

	body.Stage = stageToken
	body.TokenName = agent.DefaultName
	c.renderCloud(w, r, s, body, http.StatusOK)
}

// pick keeps the picked names that are configured connectors, in the
// connectors' order.
func pick(connectors []admin.Connector, picked []string) (map[string]bool, []string) {
	set, list := map[string]bool{}, []string(nil)

	for _, cn := range connectors {
		if slices.Contains(picked, cn.Name) {
			set[cn.Name] = true
			list = append(list, cn.Name)
		}
	}

	return set, list
}

// renderCloud fills the page's shared parts, the progress list and the remote
// tokens, and renders it.
func (c *console) renderCloud(w http.ResponseWriter, r *http.Request, s sess, body cloudView, status int) {
	// On the done stage this answer holds the only copy of the token, so what
	// the page merely decorates with (the sidebar's status, the token list)
	// may fail without taking the token with it.
	holdsToken := body.Stage == stageDone
	v, st, err := c.base(r, s, "cloud-agents", "Cloud agents")

	switch {
	case err != nil && !holdsToken:
		c.failErr(w, r, s, err)

		return
	case err != nil:
		c.Logger.Error().Err(err).Msg("read status for cloud agents page")

		v = shell(r, s, "cloud-agents", "Cloud agents")
	}

	list, err := c.Admin.ListTokens(r.Context())

	switch {
	case err != nil && !holdsToken:
		c.failErr(w, r, s, err)

		return
	case err != nil:
		c.Logger.Error().Err(err).Msg("list remote tokens for cloud agents page")

		body.RemoteUnavailable = true
	}

	now := c.Now()

	for _, t := range list.Tokens {
		if t.Remote {
			body.Remote = append(body.Remote, tokenRowFor(now, t))
		}
	}

	for _, cn := range st.Connectors {
		body.Connectors = append(body.Connectors, cn.Name)
	}

	if body.Stage == stageAgent {
		body.ConnectOffer = c.oauthOn(r)
	}

	for _, a := range agents() {
		body.Agents = append(body.Agents, agentRow{agentInfo: a, Checked: a.ID == body.Agent.ID, Connect: a.OAuth && body.ConnectOffer})
	}

	cur := stageStep(body.Stage, body.Agent)
	_, names := body.Agent.path()
	body.StepN, body.StepTotal = cur, len(names)

	for i, n := range names {
		num := i + 1
		state := "todo"

		switch {
		case num < cur:
			state = "done"
		case num == cur:
			state = "current"
		case body.Stage == stageDone && num > cur:
			// The answer to the token form covers both Test and Set up.
			state = "current"
		}

		body.Steps = append(body.Steps, stepRow{N: num, Name: n, State: state, Aria: num == cur})
	}

	switch {
	case body.Stage == stageConnectors, body.Stage == stageURL && body.Agent.OAuth:
		body.BackHref = cloudPath
	case body.Stage == stageURL:
		body.BackHref = flowQuery(stageConnectors, body.Agent.ID, body.PickedList)
	case body.Stage == stageToken:
		body.BackHref = flowQuery(stageURL, body.Agent.ID, body.PickedList)
	case body.Stage == stageOAuth:
		body.BackHref = flowQuery(stageURL, body.Agent.ID, nil)
	}

	v.Toast = toast(r)
	v.Body = body
	c.render(w, status, "cloudagents", v)
}

// formFlow reads the agent and connectors the wizard's forms carry. A form
// without a known agent starts the wizard over.
func formFlow(r *http.Request) (agentInfo, []string, bool) {
	agent, ok := agentByID(r.PostForm.Get("agent"))

	return agent, r.PostForm["connector"], ok
}

// keepAddress tells htmx not to put a POST's path into the history, for a
// post whose answer is a page rather than a redirect. The path has no GET
// page, so a reload or the back button on it would 404; with this the address
// bar stays on the wizard's step URL, which a reload can show again. A browser
// without htmx ignores the header, and the GET routes for these paths send it
// back to the wizard.
func keepAddress(w http.ResponseWriter) {
	w.Header().Set("HX-Push-Url", "false")
}

// backToWizard serves GET on a wizard form's POST path, which a browser
// without htmx can reach through its history or a typed address: it goes to
// the wizard's first page. Nothing about the flow is kept there, so nothing
// secret can be.
func (c *console) backToWizard(w http.ResponseWriter, r *http.Request, _ sess) {
	http.Redirect(w, r, cloudPath, http.StatusSeeOther)
}

// savePublicURL serves POST /ui/cloud-agents/url: it stores the public URL
// through the admin API, as `PUT /admin/server/public-url` does, then goes on
// to the token step. Nothing secret is in the redirect.
func (c *console) savePublicURL(w http.ResponseWriter, r *http.Request, s sess) {
	keepAddress(w)

	agent, picked, ok := formFlow(r)

	if !ok {
		http.Redirect(w, r, cloudPath, http.StatusSeeOther)

		return
	}

	raw := strings.TrimSpace(r.PostForm.Get("url"))
	body := cloudView{Stage: stageURL, Agent: agent, PublicURL: raw, Picked: pickedSet(picked), PickedList: picked}

	if raw == "" {
		body.Error = "Enter the https address your tunnel gives you."
		c.renderCloud(w, r, s, body, http.StatusBadRequest)

		return
	}

	cur, err := c.Admin.PublicURL(r.Context())

	if err != nil {
		c.failErr(w, r, s, err)

		return
	}

	// An unchanged address is not written again: a write rewrites the config
	// file.
	if cur.URL != raw {
		if _, err := c.Admin.SetPublicURL(r.Context(), raw); err != nil {
			status, msg := c.message(r, err)
			body.Error = msg
			c.renderCloud(w, r, s, body, status)

			return
		}
	}

	// An OAuth agent has no token: its next step turns sign-in on.
	if agent.OAuth {
		http.Redirect(w, r, flowQuery(stageOAuth, agent.ID, nil), http.StatusSeeOther)

		return
	}

	http.Redirect(w, r, flowQuery(stageToken, agent.ID, picked), http.StatusSeeOther)
}

// createCloudToken serves POST /ui/cloud-agents/token: it creates a remote
// token. Like createToken, it renders rather than redirects, because the
// value is in this answer only.
func (c *console) createCloudToken(w http.ResponseWriter, r *http.Request, s sess) {
	keepAddress(w)

	agent, picked, ok := formFlow(r)

	if !ok {
		http.Redirect(w, r, cloudPath, http.StatusSeeOther)

		return
	}

	name := strings.TrimSpace(r.PostForm.Get("name"))
	created, err := c.Admin.CreateToken(r.Context(), admin.TokenSpec{Name: name, Connectors: picked, Remote: true})

	if err != nil {
		status, msg := c.message(r, err)
		body := cloudView{Stage: stageToken, Agent: agent, TokenName: name, Error: msg, Picked: pickedSet(picked), PickedList: picked}
		c.renderCloud(w, r, s, body, status)

		return
	}

	c.renderDone(w, r, s, doneView{Agent: agent, Name: created.Name, Token: created.Token}, http.StatusOK)
}

// testCloudToken serves POST /ui/cloud-agents/test: it tests the connection
// with the token the form carries. The token is in the form body because
// Wayfindr kept only its hash; the answer carries it again so the test can
// run again.
func (c *console) testCloudToken(w http.ResponseWriter, r *http.Request, s sess) {
	keepAddress(w)

	agent, _, ok := formFlow(r)

	if !ok {
		http.Redirect(w, r, cloudPath, http.StatusSeeOther)

		return
	}

	name, token := r.PostForm.Get("name"), r.PostForm.Get("token")

	// An OAuth agent has no token: it tests Wayfindr's sign-in instead.
	if token == "" && agent.OAuth {
		c.testCloudOAuth(w, r, s)

		return
	}

	if token == "" {
		c.fail(w, r, s, http.StatusBadRequest, "There is no token to test. Create a token first.")

		return
	}

	res, err := c.Admin.TestRemote(r.Context(), token)

	if err != nil {
		status, msg := c.message(r, err)
		c.renderDone(w, r, s, doneView{Agent: agent, Name: name, Token: token, Test: &testView{Class: "err", Title: "The test could not run", Message: msg}}, status)

		return
	}

	c.renderDone(w, r, s, doneView{Agent: agent, Name: name, Token: token, Test: testOutcome(res, false)}, http.StatusOK)
}

// testOutcome words a test's result. It branches on the closed outcome, and
// the daemon's own message and action are the text. oauth is whether it was
// the sign-in test (ChatGPT), which has no token and lists no tools.
func testOutcome(res admin.RemoteTestResult, oauth bool) *testView {
	tv := &testView{Class: "err", Title: "The test failed", Message: res.Message, Action: res.Action, URL: res.URL}

	switch res.Outcome {
	case admin.RemoteOK:
		tv.Class, tv.Title = "ok", "The connection works"

		if oauth {
			tv.Detail = "Wayfindr's sign-in information is reachable through your public URL. Next, select Connect a client, " +
				"add the connector in ChatGPT, and approve it here."

			break
		}

		// The daemon's message already says how many connectors the token
		// can use; the detail adds only what came back through the tunnel.
		tv.Detail = fmt.Sprintf("Listed %s through your public URL.", countOf(res.Tools, "tool"))
	case admin.RemoteProxyTreatsRemoteAsLocal:
		tv.Title, tv.Prominent = "Warning: your tunnel makes outside requests look local", true
	case admin.RemoteOAuthOff:
		tv.Title = "Sign-in is off"
	case admin.RemoteMetadataMissing:
		tv.Title = "Wayfindr's sign-in information is not there"
	case admin.RemoteMetadataMismatch:
		tv.Title = "The sign-in information names another address"
	case admin.RemoteTokenInvalid:
		tv.Title = "Wayfindr does not accept this token"
	case admin.RemoteNotConfigured, admin.RemoteUnreachable, admin.RemoteTLSError, admin.RemoteTimeout, admin.RemoteInterstitial,
		admin.RemoteRedirect, admin.RemoteNotFound, admin.RemoteTokenRefused, admin.RemoteUnexpected:
	}

	return tv
}

// doneView is what the done stage shows: the agent, the new token's name and
// value, and the connection test run on it, if any.
type doneView struct {
	Agent       agentInfo
	Name, Token string
	Test        *testView
}

// renderDone shows the created token, the test, and the setup text.
func (c *console) renderDone(w http.ResponseWriter, r *http.Request, s sess, d doneView, status int) {
	body := cloudView{Stage: stageDone, Agent: d.Agent, TokenName: d.Name, Token: d.Token, Test: d.Test}
	pu, err := c.Admin.PublicURL(r.Context())

	if err != nil {
		// The token is on this page and nowhere else: show it, and the
		// setup text without an address, rather than lose it to an error page.
		c.Logger.Error().Err(err).Msg("read public url for cloud agent setup")
	}

	body.Setup = setupFor(pu.URL, d.Token)
	c.renderCloud(w, r, s, body, status)
}

// setupFor builds the text step 6 shows.
func setupFor(publicURL, token string) setupView {
	sv := setupView{PublicURL: publicURL, Token: token, Bearer: "Bearer " + token}

	if publicURL == "" {
		return sv
	}

	sv.McpURL = strings.TrimRight(publicURL, "/") + "/mcp"
	sv.MuseMessage = "Please create a custom connector for a remote MCP server for me. " +
		"It uses Streamable HTTP at " + sv.McpURL + " and authenticates with an Authorization: Bearer header. " +
		"Ask me for the key through your secure credential prompt. I will not type it into this chat."

	return sv
}

// oauthOn reports whether Wayfindr's sign-in (server.oauth) is on. A failed
// read counts as off: it only decides whether a convenience button shows.
func (c *console) oauthOn(r *http.Request) bool {
	set, err := c.Admin.OAuthSettings(r.Context())

	if err != nil {
		c.Logger.Error().Err(err).Msg("read oauth settings for cloud agents page")

		return false
	}

	return set.Enabled
}

// chatGPT is the wizard's entry for ChatGPT.
func chatGPT() agentInfo {
	a, _ := agentByID(agentChatGPT)

	return a
}

// cloudChatGPT serves GET /ui/cloud-agents?step=&agent=chatgpt past the first
// step. ChatGPT has no connectors step: the approve page picks them. The
// stage comes from the query, which holds no secret, and is checked against
// what is set: no public URL is the url stage, and sign-in off is the oauth
// stage, wherever the query points.
func (c *console) cloudChatGPT(w http.ResponseWriter, r *http.Request, s sess, body cloudView, step string) {
	pu, err := c.Admin.PublicURL(r.Context())

	if err != nil {
		c.failErr(w, r, s, err)

		return
	}

	body.Stage = stageURL
	body.PublicURL = pu.URL

	if step != stageOAuth && step != stageDone {
		c.renderCloud(w, r, s, body, http.StatusOK)

		return
	}

	if pu.URL == "" {
		body.Error = "Set your public URL first."
		c.renderCloud(w, r, s, body, http.StatusOK)

		return
	}

	set, err := c.Admin.OAuthSettings(r.Context())

	if err != nil {
		c.failErr(w, r, s, err)

		return
	}

	if !set.Enabled {
		body.Stage = stageOAuth
		c.renderCloud(w, r, s, body, http.StatusOK)

		return
	}

	c.renderChatGPTDone(w, r, s, chatGPTDone{}, http.StatusOK)
}

// chatGPTDone is what ChatGPT's last stage shows: the connection test run on
// it, if any, when the arm ends, if it was just armed, and why the last
// action was refused.
type chatGPTDone struct {
	Test       *testView
	ArmedUntil time.Time
	Error      string
}

// renderChatGPTDone shows the form values for ChatGPT, the "Connect a
// client" button, the test, and the way to turn sign-in off. It holds no
// secret, so it is a page a reload can show again.
func (c *console) renderChatGPTDone(w http.ResponseWriter, r *http.Request, s sess, d chatGPTDone, status int) {
	body := cloudView{Stage: stageDone, Agent: chatGPT(), Test: d.Test, Error: d.Error}
	pu, err := c.Admin.PublicURL(r.Context())

	if err != nil {
		c.Logger.Error().Err(err).Msg("read public url for chatgpt setup")
	}

	body.Setup = setupFor(pu.URL, "")

	if !d.ArmedUntil.IsZero() {
		body.ArmedUntil = clock(c.Now(), d.ArmedUntil)
	}

	c.renderCloud(w, r, s, body, status)
}

// setCloudOAuth serves POST /ui/cloud-agents/oauth: it turns Wayfindr's
// sign-in on or off. Turning it on also arms a connection, so the user can
// add the connector in ChatGPT at once. Both render, never redirect, and the
// address stays on the stage's own URL.
func (c *console) setCloudOAuth(w http.ResponseWriter, r *http.Request, s sess) {
	keepAddress(w)

	agent, _, ok := formFlow(r)

	if !ok || !agent.OAuth {
		http.Redirect(w, r, cloudPath, http.StatusSeeOther)

		return
	}

	enabled := r.PostForm.Get("enabled")

	if enabled != "true" && enabled != "false" {
		c.fail(w, r, s, http.StatusBadRequest, "Choose whether to turn sign-in on or off.")

		return
	}

	on := enabled == "true"

	if _, err := c.Admin.SetOAuthEnabled(r.Context(), on); err != nil {
		status, msg := c.message(r, err)
		c.renderCloud(w, r, s, cloudView{Stage: stageOAuth, Agent: agent, Error: msg}, status)

		return
	}

	if !on {
		c.renderCloud(w, r, s, cloudView{Stage: stageOAuth, Agent: agent, Notice: "Sign-in is off. ChatGPT is refused until you turn it on again."}, http.StatusOK)

		return
	}

	c.armAndRender(w, r, s)
}

// armCloudOAuth serves POST /ui/cloud-agents/arm: "Connect a client".
func (c *console) armCloudOAuth(w http.ResponseWriter, r *http.Request, s sess) {
	keepAddress(w)

	agent, _, ok := formFlow(r)

	if !ok || !agent.OAuth {
		http.Redirect(w, r, cloudPath, http.StatusSeeOther)

		return
	}

	c.armAndRender(w, r, s)
}

// armAndRender arms the authorization server and shows ChatGPT's last stage
// with when the arm ends. Sign-in off (409) shows the stage that turns it on,
// with the daemon's message.
func (c *console) armAndRender(w http.ResponseWriter, r *http.Request, s sess) {
	arm, err := c.Admin.ArmOAuth(r.Context())

	if ae := (*admin.APIError)(nil); errors.As(err, &ae) && ae.Status == http.StatusConflict {
		c.renderCloud(w, r, s, cloudView{Stage: stageOAuth, Agent: chatGPT(), Error: ae.Error()}, http.StatusConflict)

		return
	}

	if err != nil {
		status, msg := c.message(r, err)
		c.renderChatGPTDone(w, r, s, chatGPTDone{Error: msg}, status)

		return
	}

	c.renderChatGPTDone(w, r, s, chatGPTDone{ArmedUntil: arm.ExpiresAt}, http.StatusOK)
}

// testCloudOAuth tests ChatGPT's path: the sign-in information through the
// public URL, with no token.
func (c *console) testCloudOAuth(w http.ResponseWriter, r *http.Request, s sess) {
	res, err := c.Admin.TestRemoteOAuth(r.Context())

	if err != nil {
		status, msg := c.message(r, err)
		c.renderChatGPTDone(w, r, s, chatGPTDone{Test: &testView{Class: "err", Title: "The test could not run", Message: msg}}, status)

		return
	}

	c.renderChatGPTDone(w, r, s, chatGPTDone{Test: testOutcome(res, true)}, http.StatusOK)
}
