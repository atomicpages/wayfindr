package gateway

import (
	"cmp"
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/redact"
	"github.com/atomicpages/wayfindr/internal/upstream"
)

// Kind is the coarse class of a gateway-side failure (GLOSSARY: Kind). It
// is a closed set.
type Kind string

// Kinds.
const (
	// KindInvalidSearch: search_tools' arguments are out of range or
	// malformed.
	KindInvalidSearch Kind = "invalid_search"
	// KindInvalidName: call_tool's name is missing or not a qualified name.
	KindInvalidName Kind = "invalid_name"
	// KindUnknownTool: the catalog has no tool by the qualified name.
	KindUnknownTool Kind = "unknown_tool"
	// KindInvalidArguments: call_tool's arguments do not match the tool's
	// inputSchema. The upstream was not called.
	KindInvalidArguments Kind = "invalid_arguments"
	// KindInternal: Wayfindr itself failed (the catalog could not be read,
	// or upstream failed outside its error contract).
	KindInternal Kind = "internal"
	// KindCannotAsk: the upstream asked the user something, and the
	// calling client can't put the question to them. The upstream's
	// question was discarded.
	KindCannotAsk Kind = "cannot_ask"
	// KindInvalidRequestState: call_tool's requestState is not one the
	// gateway issued for this user, tool, and arguments, or it expired, or,
	// for a call the approval gate covers now or covered when the state was
	// issued, it was used already or issued before the daemon last started
	// (ADR 0024). The upstream was not called.
	KindInvalidRequestState Kind = "invalid_request_state"
	// KindInvalidScope: the request's project scope header does not parse
	// or validate (ADR 0010). It is refused rather than ignored, which
	// would widen what the project gets.
	KindInvalidScope Kind = "invalid_scope"
	// KindUnknownProfile: the request names a profile the running config
	// doesn't define (ADR 0011); Detail is the name. It is refused rather
	// than served unscoped, which would widen what the client gets.
	KindUnknownProfile Kind = "unknown_profile"
	// KindCanceled: the request ended before the gateway finished it,
	// because the client gave up or the daemon stopped; Err is the
	// request context's error. Nothing failed. It has no Cases.
	KindCanceled Kind = "canceled"
	// KindApproval: the client's approval rule gates the call, and the
	// owner has not approved exactly this call (ADR 0024). The upstream was
	// not called. Approval is the approval's id, except on CaseQueueFull.
	KindApproval Kind = "approval"
)

// Case is the finer reason within a [Kind] (GLOSSARY: Case). It is a
// closed set, and each Case belongs to one Kind.
type Case string

// Cases of [KindInvalidSearch].
const (
	// CaseMalformedSearch: the arguments are not an object of the declared
	// types.
	CaseMalformedSearch Case = "malformed_search"
	// CaseMissingQuery: query is missing or blank.
	CaseMissingQuery Case = "missing_query"
	// CaseBadPage: page is not a whole number of at least 1.
	CaseBadPage Case = "bad_page"
	// CaseBadSize: size is not a whole number from 1 to 10, or to 25 with
	// detail brief.
	CaseBadSize Case = "bad_size"
	// CaseUnknownConnector: connectors names a connector that is not
	// configured.
	CaseUnknownConnector Case = "unknown_connector"
	// CaseBadDetail: detail is neither "full" nor "brief"; Detail is the
	// value.
	CaseBadDetail Case = "bad_detail"
	// CaseBadNames: names is empty, too long, or has a blank entry.
	CaseBadNames Case = "bad_names"
	// CaseNamesConflict: names is combined with a search argument; Detail
	// is that argument.
	CaseNamesConflict Case = "names_conflict"
)

// Cases of [KindInvalidName].
const (
	// CaseMissingName: name is missing, blank, or not a string.
	CaseMissingName Case = "missing_name"
	// CaseUnqualified: name is not <connector>.<tool>.
	CaseUnqualified Case = "unqualified"
)

// Cases of [KindUnknownTool].
const (
	// CaseNoConnector: the name's connector is not configured.
	CaseNoConnector Case = "no_connector"
	// CaseNotInCatalog: the connector is configured, but its catalog has no
	// such tool.
	CaseNotInCatalog Case = "not_in_catalog"
	// CaseHiddenByPolicy: the connector's tools list in the config hides
	// the tool (GLOSSARY: Hidden tool).
	CaseHiddenByPolicy Case = "hidden_by_policy"
	// CaseHiddenByScope: the calling project's scope (its .wayfindr.toml)
	// hides the tool or its connector (ADR 0010).
	CaseHiddenByScope Case = "hidden_by_scope"
	// CaseHiddenByProfile: the profile the request names hides the tool or
	// its connector (ADR 0011); Detail is the profile name.
	CaseHiddenByProfile Case = "hidden_by_profile"
	// CaseBadProfile: within KindInvalidScope, the request names its
	// profile more than once or with an invalid name, which is not echoed.
	CaseBadProfile Case = "bad_profile"
	// CaseNoneFound: a names lookup found none of the names; Detail lists
	// them.
	CaseNoneFound Case = "none_found"
)

// Cases of [KindInvalidArguments].
const (
	// CaseNotObject: arguments is not a JSON object.
	CaseNotObject Case = "not_object"
	// CaseSchemaMismatch: arguments fail the tool's inputSchema; Detail
	// lists the failures.
	CaseSchemaMismatch Case = "schema_mismatch"
	// CaseAmbiguousArguments: the call is gated (ADR 0024), and its
	// arguments repeat a key in an object (keys that look alike, by case
	// or Unicode form, included), hold a string that isn't valid UTF-8, a
	// lone surrogate escape included, or have data after the arguments
	// object, so what the owner approves could differ from what the
	// upstream reads.
	CaseAmbiguousArguments Case = "ambiguous"
	// CaseArgumentsTooLarge: the call is gated (ADR 0024), and its
	// arguments are larger than 64 KiB or nest deeper than 32 levels, more
	// than the owner can be asked to read. They are not echoed.
	CaseArgumentsTooLarge Case = "too_large"
)

// Cases of [KindInternal].
const (
	// CaseCatalog: the catalog could not be read.
	CaseCatalog Case = "catalog"
	// CaseUpstream: upstream failed with an error that is not an
	// *upstream.Error, or returned an input request that is not an
	// elicitation, which its contract rules out.
	CaseUpstream Case = "upstream"
	// CaseNoClient: the request carries no client, the one its client token
	// resolves to, in its TokenInfo. httpd attaches one to every request it
	// lets through, so this is a wiring bug; the request is refused rather
	// than served unscoped, which could reach connectors the token doesn't
	// allow.
	CaseNoClient Case = "no_client"
)

// Cases of [KindCannotAsk].
const (
	// CaseClientTooOld: the client speaks a protocol version before
	// 2026-07-28; Detail is the version.
	CaseClientTooOld Case = "client_too_old"
	// CaseNoElicitation: the client doesn't declare elicitation, or not
	// the mode the question needs.
	CaseNoElicitation Case = "no_elicitation"
)

// Cases of [KindApproval].
const (
	// CaseApprovalPending: the call waits for the owner's answer. The first
	// gated call creates the approval; a repeat gets the same one.
	CaseApprovalPending Case = "pending"
	// CaseApprovalDenied: the owner denied the call. The denial stands until
	// the approval expires.
	CaseApprovalDenied Case = "denied"
	// CaseApprovalExpired: the approval expired, pending, granted, or
	// denied, before this call; it is dropped, so the next call asks again.
	CaseApprovalExpired Case = "expired"
	// CaseQueueFull: the client already has as many approvals as it may
	// hold, so the call was refused without one. None was evicted.
	CaseQueueFull Case = "queue_full"
)

// Error is every gateway-side failure (CODING_STANDARDS: Seam errors). It
// is always handled as error; narrow it with [AsError].
type Error struct {
	Kind Kind
	Case Case
	// Tool is the qualified name call_tool was given, if any.
	Tool string
	// Connector is the connector the failure is about, if any.
	Connector string
	// Configured lists the configured connectors, on errors that name an
	// unknown one.
	Configured []string
	// Detail is the offending value, or the schema failures on
	// CaseSchemaMismatch.
	Detail string
	// Approval is the approval id on a KindApproval error, for the owner's
	// `wayfindr approvals approve <id>`; empty on CaseQueueFull.
	Approval string
	// Err is the cause, if any.
	Err error
}

// Error is technical text for logs: "gateway: <kind> (<case>)[: <tool>][:
// <cause>]", with URLs redacted. What it means to the user comes from
// [Advise].
func (e *Error) Error() string {
	s := "gateway: " + string(e.Kind)

	if e.Case != "" {
		s += " (" + string(e.Case) + ")"
	}

	if e.Tool != "" {
		s += ": " + e.Tool
	}

	if e.Err != nil {
		s += ": " + e.Err.Error()
	}

	return redact.URLQueries(s)
}

// Unwrap returns the cause.
func (e *Error) Unwrap() error { return e.Err }

// AsError narrows err to the gateway [*Error] it carries, if any.
func AsError(err error) (*Error, bool) {
	var e *Error

	if errors.As(err, &e) {
		return e, true
	}

	return nil, false
}

// Advice is a user-facing reading of an error (GLOSSARY: Advice): what went
// wrong, the action that fixes it, and an optional link. It has the shape
// of upstream.Advice, which converts to it.
type Advice struct {
	Summary string
	Action  string
	Link    string
}

// Advise says what e means to the agent and how to fix it. The advice for
// e's Kind applies unless e's Case has its own. Both switches are
// exhaustive, so a new Kind or Case cannot ship without advice.
func Advise(e *Error) Advice {
	if e == nil {
		return Advice{}
	}

	var a Advice
	a.Summary, a.Action = kindAdvice(e)

	if s, act := caseAdvice(e); s != "" {
		a.Summary, a.Action = s, act
	}

	return a
}

const (
	adviseApprove = "Wait for the owner to approve it in Wayfindr, then call it again with the same arguments. " +
		"Changed arguments need a new approval."
	adviseSearch = "Use search_tools to find the tool and its qualified name."
	adviseStdio  = "connect it with `wayfindr stdio`, which can ask clients of every version."
)

func kindAdvice(e *Error) (summary, action string) {
	switch e.Kind {
	case KindInvalidSearch:
		return "The search_tools arguments are invalid.",
			"Pass query, and optionally page (≥ 1), size (" + sizeLimits() + "), connectors, and detail; or pass names alone."
	case KindInvalidName:
		return "call_tool needs a qualified tool name such as linear.create_issue.", adviseSearch
	case KindUnknownTool:
		return "There is no tool called " + e.Tool + ".", adviseSearch
	case KindInvalidArguments:
		return "The arguments do not match " + e.Tool + "'s inputSchema.", "Fix the arguments and call again."
	case KindInternal:
		return "Wayfindr failed to handle the request.", "Try again; if it persists, check the daemon log."
	case KindCannotAsk:
		return e.Tool + " needs your input, but your client can't ask you.", adviseStdio
	case KindInvalidRequestState:
		return "The answer arrived too late or does not belong to this call.", "Call the tool again."
	case KindInvalidScope:
		return "This project's Wayfindr scope (" + config.ScopeFile + ") is invalid, so Wayfindr refused the request.",
			"Tell the user; `wayfindr scope` in the project directory shows what is wrong."
	case KindUnknownProfile:
		return "This client asks for the Wayfindr profile " + e.Detail + ", which the config doesn't define, so Wayfindr refused the request.",
			"Tell the user; `wayfindr scope --profile " + e.Detail + "` shows the profiles the config defines."
	case KindCanceled:
		return "The request was cancelled before Wayfindr finished it.", "Try again."
	case KindApproval:
		return e.Tool + " needs the owner's approval before it runs" + approvalRef(e) + ".", adviseApprove
	}

	return "", "" // not a Kind constant
}

func caseAdvice(e *Error) (summary, action string) {
	switch e.Case {
	case CaseMalformedSearch:
		return "The search_tools arguments have the wrong types.",
			"query and detail are strings, page and size are integers, and connectors and names are arrays of strings."
	case CaseMissingQuery:
		return "search_tools needs a query, or names.",
			"Describe what you want to do in query, in words or keywords; or pass the qualified names you want in names."
	case CaseBadPage:
		return "page must be a whole number of at least 1, not " + e.Detail + ".", "Omit page for the first page."
	case CaseBadSize:
		return "size must be a whole number from " + sizeLimits() + ", not " + e.Detail + ".",
			"Omit size for the default."
	case CaseUnknownConnector:
		return "No connector is called " + e.Connector + ". Configured connectors: " + list(e.Configured) + ".",
			"Use one of those names, or omit connectors to search them all."
	case CaseBadDetail:
		return "detail must be \"full\" or \"brief\", not " + e.Detail + ".", "Omit detail for full definitions."
	case CaseBadNames:
		return "names must list 1 to " + strconv.Itoa(maxNames) + " qualified tool names, none blank.",
			"Pass names such as [\"linear.create_issue\"]; split a longer list over several calls."
	case CaseNamesConflict:
		return "names cannot be combined with " + e.Detail + ".",
			"Pass names alone to get full definitions, or drop names to search."
	case CaseMissingName:
		return "call_tool needs name, the qualified name of the tool to call.", adviseSearch
	case CaseUnqualified:
		return e.Tool + " is not a qualified tool name; names look like <connector>.<tool>, for example linear.create_issue.", adviseSearch
	case CaseNoConnector:
		return "There is no tool called " + e.Tool + ": no connector is called " + e.Connector + ".", adviseSearch
	case CaseNotInCatalog:
		return "There is no tool called " + e.Tool + " in the catalog.",
			adviseSearch + " If the connector is listed there as unavailable, fix that first."
	case CaseHiddenByPolicy:
		return e.Tool + " is hidden by the tools list of [connectors." + e.Connector + "] in your Wayfindr config.",
			"Don't use it. If the agent should, the user can add it to that tools list."
	case CaseHiddenByScope:
		return e.Tool + " is hidden by this project's Wayfindr scope (" + config.ScopeFile + ").",
			"Don't use it. If the agent should, the user can allow it in that file."
	case CaseHiddenByProfile:
		return e.Tool + " is hidden by the Wayfindr profile " + e.Detail + ".",
			"Don't use it. If the agent should, the user can allow it in [profiles." + e.Detail + "] in the config."
	case CaseBadProfile:
		return "This client names its Wayfindr profile more than once, or with a name that isn't valid, so Wayfindr refused the request.",
			"Tell the user to check the profile in the client's Wayfindr settings (--profile or ?profile=)."
	case CaseNoneFound:
		return "None of these tools is in the catalog: " + e.Detail + ".",
			adviseSearch + " Then pass the names it returns."
	case CaseNotObject:
		return "call_tool's arguments must be a JSON object.", "Pass the tool's arguments as an object matching its inputSchema."
	case CaseSchemaMismatch:
		return "The arguments do not match " + e.Tool + "'s inputSchema:\n" + e.Detail,
			"Fix the arguments and call again. Get the inputSchema with search_tools names=[\"" + e.Tool + "\"]."
	case CaseAmbiguousArguments:
		return e.Tool + " needs the owner's approval, and its arguments repeat a key in an object or hold text that isn't valid UTF-8, " +
				"so the owner could approve something other than what runs. Keys that differ only in case or Unicode form count as the same key.",
			"Send each key once per object, and only valid UTF-8 text, then call again."
	case CaseArgumentsTooLarge:
		return e.Tool + " needs the owner's approval, and its arguments are larger than " + strconv.Itoa(maxGatedArgsBytes>>10) +
				" KiB or nest deeper than " + strconv.Itoa(maxGatedArgsDepth) + " levels, more than the owner can review.",
			"Send smaller, flatter arguments, for example by splitting the work over several calls, then call again."
	case CaseCatalog:
		return "Wayfindr could not read its tool catalog.", "Try again; if it persists, check the daemon log."
	case CaseUpstream:
		return "Wayfindr failed while calling " + e.Tool + ".", "Try again; if it persists, check the daemon log."
	case CaseNoClient:
		return "Wayfindr could not tell which client token this request used, so it refused the request.",
			"Tell the user; the daemon log has the details."
	case CaseClientTooOld:
		return e.Tool + " needs your input, but your client's MCP version (" + cmp.Or(e.Detail, "unknown") + ") can't ask you.",
			"Update the client, or " + adviseStdio
	case CaseNoElicitation:
		return e.Tool + " needs your input, but your client does not support elicitation.",
			"Use a client that supports elicitation, or " + adviseStdio
	case CaseApprovalPending:
		return e.Tool + " needs the owner's approval before it runs" + approvalRef(e) + ".", adviseApprove
	case CaseApprovalDenied:
		return "The owner denied " + e.Tool + " with these arguments" + approvalRef(e) + ".",
			"Don't retry it. Tell the user it was denied; a call with other arguments needs its own approval."
	case CaseApprovalExpired:
		return "The owner's approval of " + e.Tool + " with these arguments" + approvalRef(e) + " has expired.",
			"Call it again with the same arguments to ask again, then wait for the owner to approve it in Wayfindr."
	case CaseQueueFull:
		return e.Tool + " needs the owner's approval before it runs, but this client already has " +
				strconv.Itoa(maxApprovals) + " open approvals (waiting, approved, or denied), so Wayfindr refused it.",
			"A slot frees when an approved call is retried with the same arguments, or when an entry expires (within " +
				strconv.Itoa(int(pendingTTL/time.Hour)) + " hours). Don't change the arguments: each variant needs a slot of its own."
	}

	return "", "" // no Case, or not a Case constant
}

// approvalRef is " (approval <id>)" for e's approval id, or "" without
// one.
func approvalRef(e *Error) string {
	if e.Approval == "" {
		return ""
	}

	return " (approval " + e.Approval + ")"
}

func list(names []string) string {
	if len(names) == 0 {
		return "none"
	}

	return strings.Join(names, ", ")
}

// catalogError is the error for a catalog read that failed with err. A read
// that failed once ctx had ended was cut short by the caller giving up,
// KindCanceled, not a catalog failure; ctx decides, since a store reports
// that as the context's error or as its driver's own.
func catalogError(ctx context.Context, err error, tool, connector string) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return &Error{Kind: KindCanceled, Tool: tool, Connector: connector, Err: ctxErr}
	}

	return &Error{Kind: KindInternal, Case: CaseCatalog, Tool: tool, Connector: connector, Err: err}
}

// errorResult renders a failure as an isError result. A gateway or
// upstream *Error renders its advice, an upstream needs_auth error's link
// included (the manager fills it in); anything else is a bug, logged and
// rendered as an internal failure.
func (g *Gateway) errorResult(err error) *mcp.CallToolResult {
	var a Advice

	if e, ok := AsError(err); ok {
		a = Advise(e)

		if e.Kind == KindInternal {
			g.log.Error().Err(err).Str("connector", e.Connector).Str("tool", e.Tool).Msg("gateway request failed")
		}
	} else if e, ok := upstream.AsError(err); ok {
		a = Advice(upstream.Advise(e))
	} else {
		g.log.Error().Err(err).Msg("gateway request failed outside the error contract")
		a = Advise(&Error{Kind: KindInternal})
	}

	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: render(a)}}}
}

// render is advice as the text of an isError result.
func render(a Advice) string {
	var sb strings.Builder
	sb.WriteString(a.Summary)

	if a.Action != "" {
		sb.WriteString("\nTo fix: " + a.Action)
	}

	if a.Link != "" {
		sb.WriteString("\nLink: " + a.Link)
	}

	return sb.String()
}
