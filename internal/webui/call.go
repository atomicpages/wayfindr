package webui

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/atomicpages/wayfindr/internal/admin"
)

// A tool's form page (GET and POST /ui/connectors/{name}/tools/{tool}):
// one input per argument the tool's inputSchema names, a result view, and
// the upstream's own form when it asks for input mid-call. Calls go through
// the admin API's call route, as `wayfindr call` does, as the owner.

// argPrefix names a tool argument's form field; answerPrefix names an
// answer to a question, as answerPrefix + request id + "." + name.
const (
	argPrefix    = "arg."
	answerPrefix = "in."
)

// toolView is the tool page's data.
type toolView struct {
	Connector, Tool string
	// Class is the tool's class; the submit button names it. It is empty
	// when ListError is set.
	Class string
	// ListError is why the connector's tools could not be listed: the class
	// and the schema are unknown, so the form takes the arguments as one
	// JSON object and runs the tool as a destructive one.
	ListError   string
	Description string
	Fields      []formField
	// Error is why the call was not made, or why the daemon refused it.
	Error    string
	Result   *resultView
	Question *questionView
}

// formField is one input of a form, built in Go so the template only lays
// it out.
type formField struct {
	// ID is the element id; Name the form field name.
	ID, Name string
	Label    string
	// Kind is "text", "number" (Step "1" for an integer), "select" (an
	// enum or a boolean: Options hold JSON values), or "json" (a labeled
	// textarea for an object, an array, or a property of no known type).
	Kind        string
	Step        string
	Type        string
	Required    bool
	Description string
	Options     []formOption
	Value       string
}

// formOption is one choice of a select field: its JSON value and label.
type formOption struct {
	Value, Label string
	Selected     bool
}

// resultView is a call's result: its text, other content as one line each,
// and its structured content indented.
type resultView struct {
	IsError    bool
	Elapsed    string
	Size       string
	Items      []string
	Structured string
}

// questionView is an input_required result: the sealed state and the
// arguments to send back unchanged, and each request.
type questionView struct {
	State     string
	Arguments string
	// Questions is the requests as JSON, sent back so the answers can be
	// read by their schema.
	Questions string
	Requests  []questionRequest
}

// questionRequest is one request: a form's fields, or a URL to open.
type questionRequest struct {
	ID, Message string
	// URL is a URL request's address, http or https only; empty for a form.
	URL    string
	Fields []formField
}

// toolPage serves GET /ui/connectors/{name}/tools/{tool}: the tool's form.
func (c *console) toolPage(w http.ResponseWriter, r *http.Request, s sess) {
	v, body, ok := c.toolStart(w, r, s)

	if !ok {
		return
	}

	v.Body = body
	c.render(w, http.StatusOK, "tool", v)
}

// toolStart finds the tool and builds its page's frame and form. A tool
// the connector doesn't serve, or one its tools list hides, gets the
// error page: agents can't call a hidden tool, so neither can the form.
func (c *console) toolStart(w http.ResponseWriter, r *http.Request, s sess) (view, toolView, bool) {
	name, tool := r.PathValue("name"), r.PathValue("tool")
	v, _, err := c.base(r, s, "connectors", tool)

	if err != nil {
		c.failErr(w, r, s, err)

		return view{}, toolView{}, false
	}

	v.Parent, v.ParentHref = name, "/ui/connectors/"+url.PathEscape(name)
	list, err := c.Admin.Tools(r.Context(), name)

	// A listing the daemon refused, such as for a connector whose sign-in
	// broke, still gets a form: the call shows the gateway's own error and
	// is in the call log. Its class is unknown, so it is run as a
	// destructive tool is, with its arguments as one JSON object.
	if ae := (*admin.APIError)(nil); errors.As(err, &ae) {
		return v, toolView{Connector: name, Tool: tool, ListError: ae.Error(), Fields: []formField{rawArguments()}}, true
	}

	if err != nil {
		c.failErr(w, r, s, err)

		return view{}, toolView{}, false
	}

	i := slices.IndexFunc(list.Tools, func(t admin.ToolEntry) bool { return t.Name == tool })

	if i < 0 || !list.Tools[i].Exposed {
		msg := fmt.Sprintf("%s serves no tool called %q.", name, tool)

		if i >= 0 {
			msg = fmt.Sprintf("%s.%s is hidden by the connector's tools list, so agents can't call it, and neither can this form.", name, tool)
		}

		c.fail(w, r, s, http.StatusNotFound, msg)

		return view{}, toolView{}, false
	}

	t := list.Tools[i]
	body := toolView{Connector: name, Tool: tool, Class: t.Class}

	var def struct {
		Description string          `json:"description"`
		InputSchema json.RawMessage `json:"inputSchema"`
	}

	// A definition that doesn't decode gets a form with no fields; the
	// gateway still validates what is sent.
	_ = json.Unmarshal(t.Definition, &def)
	body.Description = def.Description
	body.Fields = buildFields(def.InputSchema, argPrefix, "arg-")

	return v, body, true
}

// rawArgumentsName is the form field of a tool's arguments as one JSON
// object, when its schema is unknown.
const rawArgumentsName = "arguments_json"

// rawArguments is the one field of a tool whose schema is unknown.
func rawArguments() formField {
	return formField{ID: "arg-json", Name: rawArgumentsName, Label: "Arguments", Kind: "json", Type: "object",
		Description: "The tool's arguments as one JSON object; empty sends {}."}
}

// runTool serves POST /ui/connectors/{name}/tools/{tool}: the call, or the
// answer to the question an earlier call asked, through the admin API's
// call route. The page is rendered again with the result, the question,
// or the refusal, and the form filled from the post.
func (c *console) runTool(w http.ResponseWriter, r *http.Request, s sess) {
	v, body, ok := c.toolStart(w, r, s)

	if !ok {
		return
	}

	form := r.PostForm

	for i, f := range body.Fields {
		body.Fields[i] = refill(f, form.Get(f.Name))
	}

	req := admin.CallRequest{Name: body.Connector + "." + body.Tool, Client: admin.CallClientConsole}

	if state := form.Get("request_state"); state != "" {
		var err error

		req.RequestState = state
		req.Arguments = json.RawMessage(form.Get("arguments"))
		req.InputResponses, err = readAnswers(form)

		if err != nil {
			c.renderTool(w, http.StatusBadRequest, v, body, sentence(err.Error()))

			return
		}
	} else {
		var (
			args json.RawMessage
			err  error
		)

		if body.ListError != "" {
			args, err = readRawArguments(form.Get(rawArgumentsName))
		} else {
			args, err = readArguments(body.Fields, form)
		}

		if err != nil {
			c.renderTool(w, http.StatusBadRequest, v, body, sentence(err.Error()))

			return
		}

		req.Arguments = args
	}

	resp, err := c.Admin.Call(r.Context(), req)

	if err != nil {
		status, msg := c.message(r, err)
		c.renderTool(w, status, v, body, msg)

		return
	}

	res, err := resp.Decode()

	if err != nil {
		c.Logger.Error().Err(err).Str("path", r.URL.Path).Msg("console call result")
		c.renderTool(w, http.StatusInternalServerError, v, body, "The daemon's answer could not be read; see the daemon log.")

		return
	}

	if res.InputRequired() {
		body.Question = questionOf(res, req.Arguments)
	} else {
		body.Result = resultOf(resp, res)
	}

	v.Body = body
	c.render(w, http.StatusOK, "tool", v)
}

// sentence is an error's text as a sentence: capitalized, with a period.
func sentence(s string) string {
	r, n := utf8.DecodeRuneInString(s)

	if n == 0 {
		return s
	}

	return string(unicode.ToUpper(r)) + s[n:] + "."
}

// renderTool renders the tool page with msg as its error.
func (c *console) renderTool(w http.ResponseWriter, status int, v view, body toolView, msg string) {
	body.Error = msg
	v.Body = body
	c.render(w, status, "tool", v)
}

// buildFields is the form fields of an object schema: a text input per
// string, a number input per number and integer, a select per enum and
// boolean, and a JSON textarea for the rest. prefix starts each field's
// name and idPrefix its element id.
func buildFields(schema json.RawMessage, prefix, idPrefix string) []formField {
	// A schema that doesn't decode in full gives the fields read before the
	// fault; the gateway still validates what is sent.
	props, _ := admin.SchemaFields(schema)
	fields := make([]formField, 0, len(props))

	for i, p := range props {
		label := p.Title

		if label == "" {
			label = p.Name
		}

		f := formField{
			ID: idPrefix + strconv.Itoa(i), Name: prefix + p.Name, Label: label, Type: p.Type,
			Required: p.Required, Description: p.Description,
		}

		switch {
		case len(p.Enum) > 0:
			f.Kind = "select"
			f.Options = enumOptions(p.Enum)

			if f.Type == "" {
				f.Type = "choice"
			}
		case p.Type == "boolean":
			f.Kind = "select"
			f.Options = enumOptions([]json.RawMessage{json.RawMessage("true"), json.RawMessage("false")})
		case p.Type == "string":
			f.Kind = "text"
		case p.Type == "integer":
			f.Kind, f.Step = "number", "1"
		case p.Type == "number":
			f.Kind, f.Step = "number", "any"
		default:
			f.Kind = "json"

			if f.Type == "" {
				f.Type = "JSON"
			}
		}

		fields = append(fields, f)
	}

	return fields
}

// enumOptions are a select's options: an empty first one, then each value
// as its JSON, labeled without the quotes a string has. The empty one comes
// first for a required field too, so the browser preselects nothing: a
// required select (the template marks it required) then makes the user
// choose, and a destructive tool's required flag never goes out as the
// first value untouched.
func enumOptions(values []json.RawMessage) []formOption {
	opts := []formOption{{Value: "", Label: "—"}}

	for _, v := range values {
		label := string(v)

		var s string

		if json.Unmarshal(v, &s) == nil {
			label = s
		}

		opts = append(opts, formOption{Value: string(v), Label: label})
	}

	return opts
}

// refill sets f's value to what the form posted, so a refused or finished
// call shows the form as it was sent.
func refill(f formField, posted string) formField {
	f.Value = posted

	if f.Kind == "select" {
		opts := slices.Clone(f.Options)

		for i := range opts {
			opts[i].Selected = opts[i].Value == posted
		}

		f.Options = opts
	}

	return f
}

// readArguments builds the call's arguments, a JSON object, from the
// posted form: each field read by its kind. An empty optional field is
// left out; an empty required text field is sent as "".
func readArguments(fields []formField, form url.Values) (json.RawMessage, error) {
	args := map[string]json.RawMessage{}

	for _, f := range fields {
		raw, ok, err := readField(f, form.Get(f.Name))

		if err != nil {
			return nil, err
		}

		if ok {
			args[strings.TrimPrefix(f.Name, argPrefix)] = raw
		}
	}

	return json.Marshal(args)
}

// readRawArguments reads the arguments of a tool whose schema is unknown:
// a JSON object, or {} for nothing.
func readRawArguments(v string) (json.RawMessage, error) {
	v = strings.TrimSpace(v)

	if v == "" {
		return json.RawMessage("{}"), nil
	}

	if !json.Valid([]byte(v)) || v[0] != '{' {
		return nil, errors.New("the arguments must be a JSON object")
	}

	return json.RawMessage(v), nil
}

// readField reads one field's posted value as JSON, or false when it is
// left out.
func readField(f formField, v string) (json.RawMessage, bool, error) {
	v = strings.TrimSpace(v)

	if v == "" {
		if f.Required && f.Kind == "text" {
			return json.RawMessage(`""`), true, nil
		}

		return nil, false, nil
	}

	switch f.Kind {
	case "text":
		b, _ := json.Marshal(v) // a string always marshals

		return b, true, nil
	case "number":
		// The parsed value is sent, not what was typed: "007" is not JSON.
		if f.Step == "1" {
			n, err := strconv.ParseInt(v, 10, 64)

			if err != nil {
				return nil, false, fmt.Errorf("%s must be a whole number", f.Label)
			}

			b, _ := json.Marshal(n) // an int64 always marshals

			return b, true, nil
		}

		n, err := strconv.ParseFloat(v, 64)

		// JSON has no NaN or infinity.
		if err != nil || math.IsNaN(n) || math.IsInf(n, 0) {
			return nil, false, fmt.Errorf("%s must be a number", f.Label)
		}

		b, _ := json.Marshal(n) // a finite float64 always marshals

		return b, true, nil
	case "select":
		if !slices.ContainsFunc(f.Options, func(o formOption) bool { return o.Value == v }) {
			return nil, false, fmt.Errorf("%s must be one of its choices", f.Label)
		}

		return json.RawMessage(v), true, nil
	}

	if !json.Valid([]byte(v)) {
		return nil, false, fmt.Errorf("%s must be JSON", f.Label)
	}

	return json.RawMessage(v), true, nil
}

// readAnswers reads the answers to a question from the posted form: the
// action from the button pressed, and each form request's fields by the
// schema the page sent back in "questions".
func readAnswers(form url.Values) (map[string]admin.InputResponse, error) {
	var questions map[string]admin.ElicitRequest

	if err := json.Unmarshal([]byte(form.Get("questions")), &questions); err != nil {
		return nil, errors.New("the form is incomplete: run the tool again")
	}

	action := form.Get("action")

	if action != admin.ActionAccept && action != admin.ActionDecline {
		action = admin.ActionCancel
	}

	answers := make(map[string]admin.InputResponse, len(questions))

	for id, q := range questions {
		a := admin.InputResponse{Action: action}

		if action == admin.ActionAccept && q.Mode != admin.ElicitURL {
			a.Content = map[string]any{}

			for _, f := range buildFields(q.RequestedSchema, answerPrefix+id+".", "") {
				raw, ok, err := readField(f, form.Get(f.Name))

				if err != nil {
					return nil, err
				}

				if !ok {
					continue
				}

				var v any

				_ = json.Unmarshal(raw, &v) // readField returns only valid JSON
				a.Content[strings.TrimPrefix(f.Name, answerPrefix+id+".")] = v
			}
		}

		answers[id] = a
	}

	return answers, nil
}

// questionOf is the view of an input_required result. args are the
// arguments the call was made with: the answer must send them unchanged.
func questionOf(res admin.ToolResult, args json.RawMessage) *questionView {
	questions := make(map[string]admin.ElicitRequest, len(res.InputRequests))
	q := &questionView{State: res.RequestState, Arguments: string(args)}

	for i, id := range slices.Sorted(maps.Keys(res.InputRequests)) {
		p := res.InputRequests[id].Params
		questions[id] = p
		r := questionRequest{ID: id, Message: p.Message}

		if p.Mode == admin.ElicitURL {
			if u, err := url.Parse(p.URL); err == nil && (u.Scheme == "https" || u.Scheme == "http") {
				r.URL = p.URL
			}
		} else {
			r.Fields = buildFields(p.RequestedSchema, answerPrefix+id+".", "in-"+strconv.Itoa(i)+"-")
		}

		q.Requests = append(q.Requests, r)
	}

	b, _ := json.Marshal(questions) // decoded from JSON, so it marshals
	q.Questions = string(b)

	return q
}

// resultOf is the view of a finished call: its text content, other content
// as one line each (images are not shown: the CSP forbids data: URLs), and
// its structured content indented.
func resultOf(resp admin.CallResponse, res admin.ToolResult) *resultView {
	v := &resultView{
		IsError: res.IsError,
		Elapsed: elapsedWords(resp.ElapsedMS),
		Size:    admin.FormatBytes(int64(len(resp.Result))),
	}

	for _, c := range res.Content {
		v.Items = append(v.Items, c.Summary())
	}

	var b bytes.Buffer

	if len(res.StructuredContent) > 0 && json.Indent(&b, res.StructuredContent, "", "  ") == nil {
		v.Structured = b.String()
	}

	return v
}

// elapsedWords words a duration in milliseconds: "812 ms", "1.4 s".
func elapsedWords(ms int64) string {
	if ms < 1000 {
		return fmt.Sprintf("%d ms", ms)
	}

	return fmt.Sprintf("%.1f s", float64(ms)/1000)
}
