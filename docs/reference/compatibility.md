# Compatibility

What Wayfindr speaks on each side, which clients it has been checked with, and
how it does on the MCP conformance suite.

## MCP protocol versions

| Version    | Clients → Wayfindr | Wayfindr → upstreams |
| ---------- | ------------------ | -------------------- |
| 2026-07-28 | Supported          | Supported            |
| 2025-11-25 | Supported          | Supported            |
| 2025-06-18 | Supported          | Supported            |

The two sides negotiate **separately**. A client on 2025-06-18 can call tools on
an upstream that speaks 2026-07-28, and the other way round; Wayfindr translates
between them. With the stdio shim there is a third, independent negotiation
between the shim and your client.

These three versions are the ones Wayfindr is built and tested for. The MCP
library it uses also accepts 2025-03-26 and 2024-11-05, so older clients and
servers usually connect, but they aren't tested. A client that asks for a
version the daemon doesn't know is answered with 2025-11-25.

### Client side

- `/mcp` is stateless Streamable HTTP: no `Mcp-Session-Id`, no server-to-client
  stream. See [HTTP Endpoints](./http#mcp).
- The server declares only `tools` (without `listChanged`) and no `logging`.
  `prompts/*`, `resources/*`, and `completion/*` answer `-32601`; see
  [Meta-Tools](./meta-tools#other-mcp-methods).
- Because the tool list never changes (it is always `search_tools` and
  `call_tool`), clients never need a `list_changed` notification. New upstream
  tools show up in search results as soon as they are synced.

### Upstream side

| Transport         | Chosen by                                  | Notes                                                                                                     |
| ----------------- | ------------------------------------------ | --------------------------------------------------------------------------------------------------------- |
| stdio             | `command` set                              | Wayfindr spawns the command and talks MCP over its stdin and stdout.                                      |
| Streamable HTTP   | `url` set                                  | The default for `url` connectors.                                                                         |
| HTTP+SSE (legacy) | `transport = "sse"`, or automatic fallback | Wayfindr falls back to it when the Streamable HTTP `initialize` request gets a 4xx other than 401 or 403, and no request of the connect got a 401 or 403. |

Upstream auth: OAuth 2.1 / OIDC with discovery (protected-resource metadata,
authorization-server metadata, and the 2025-03-26 fallback), Dynamic Client
Registration, a pre-registered client, or a Client ID Metadata Document; API
keys in headers; mutual TLS; or none. See [Upstream OAuth](#upstream-oauth) for
every grant and method, and [Authentication](../guide/authentication).

### Upstream OAuth

What Wayfindr supports as an OAuth client of each upstream's authorization
server:

| Area                  | Supported                                                                                                                                                                                    | Not supported                                                                                                                   |
| --------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------- |
| Grants                | Authorization code with PKCE (S256), device code (RFC 8628), client credentials, JWT bearer (RFC 7523) with a workload token or a self-signed assertion; refresh tokens                      | Implicit and password (removed in OAuth 2.1), SAML 2.0 bearer, UMA, token exchange (RFC 8693), Enterprise-Managed Authorization |
| Client registration   | A pre-registered `client_id`, Client ID Metadata Documents, Dynamic Client Registration (RFC 7591, with `application_type`)                                                                  | Hosting a client metadata document for you                                                                                      |
| Client authentication | `none`, `client_secret_basic`, `client_secret_post`, `client_secret_jwt`, `private_key_jwt` (ES256, ES384, RS256, PS256, EdDSA), `tls_client_auth`, `self_signed_tls_client_auth` (RFC 8705) |                                                                                                                                 |
| Sender constraints    | Bearer tokens; DPoP (RFC 9449) with ES256 proofs and nonces, chosen automatically when the server asks; certificate-bound tokens (RFC 8705)                                                  | DPoP proofs signed with any algorithm but ES256                                                                                 |
| TLS                   | A private CA added to the system's, a client certificate (file or secret key) on both the MCP server's and the authorization server's connections, mTLS endpoint aliases                     |                                                                                                                                 |
| Discovery and checks  | Protected resource metadata (RFC 9728), authorization server metadata (RFC 8414), resource indicators (RFC 8707), issuer checks (RFC 9207), step-up on `insufficient_scope`                  |                                                                                                                                 |
| Revocation            | Forgetting a credential locally (`wayfindr auth logout`)                                                                                                                                     | Revoking a token at the provider (RFC 7009)                                                                                     |

See [Authentication](../guide/authentication) for how to set each one up.

### Questions from upstreams (elicitation)

Some tools ask you something in the middle of a call: which workspace, whether
to go ahead, or to open a link to connect an account. Wayfindr passes the
question to your client, then sends your answer back to the upstream. Whether
your client can show the question depends on how it connects:

| Client connects                           | Questions                                                                          |
| ----------------------------------------- | ---------------------------------------------------------------------------------- |
| With `wayfindr stdio`, any MCP version    | Asked. Wayfindr asks the client directly or with `input_required`, as it supports. |
| Over HTTP at 2026-07-28, with elicitation | Asked, as an `input_required` result of `call_tool`.                               |
| Over HTTP at 2025-11-25 or 2025-06-18     | Not asked. The call fails with an error that points at `wayfindr stdio`.           |
| Over HTTP without elicitation             | Not asked. The call fails with an error saying so.                                 |

Through `wayfindr stdio`, a client that can't show the question (it declares no
elicitation, or not the kind the question needs) gets:

```
This tool needs your input, but your client could not be asked (client does not support elicitation).
To fix: Use a client that supports elicitation.
```

It works with upstreams at every version. You have 10 minutes to answer a
question; after that, the upstream is told you cancelled. Some limits apply:

- An upstream at 2025-06-18 or 2025-11-25 can only ask while it has a single
  call from Wayfindr in progress. If it asks during another call, Wayfindr can't
  tell which call the question is for, so it fails the call and says why.
- Sampling, where an upstream asks your client's model to write something, is
  not supported. The call returns an error saying so.

## Clients

Wayfindr works with any MCP client that speaks Streamable HTTP with a bearer
token, or stdio. `wayfindr init` prints the setup for each client below, and
[Clients](../guide/clients) has the command or config entry for each, stdio and
HTTP.

| Client                   | HTTP (`/mcp` with bearer) | stdio (`wayfindr stdio`) | Checked with     |
| ------------------------ | ------------------------- | ------------------------ | ---------------- |
| Claude Code              | Yes                       | Yes                      | 2.1.326 (partly) |
| Codex                    | Yes                       | Yes                      | 0.169.0 (partly) |
| OpenCode                 | Yes                       | Yes                      | not yet          |
| Hermes Agent             | Yes                       | Yes                      | not yet          |
| Gemini CLI               | Yes, with `httpUrl`       | Yes                      | not yet          |
| GitHub Copilot CLI       | Yes                       | Yes                      | not yet          |
| VS Code                  | Yes                       | Yes                      | not yet          |
| Cursor                   | Yes                       | Yes                      | not yet          |
| Zed                      | No; use stdio             | Yes                      | not yet          |
| Windsurf (Devin Desktop) | Yes                       | Yes                      | not yet          |
| goose                    | Yes                       | Yes                      | not yet          |

Zed sends remote server header values as written, without expanding environment
variables ([zed#60459](https://github.com/zed-industries/zed/issues/60459)), so
an HTTP entry would have to contain the token. Use the stdio shim with Zed.

### Cloud agents

A cloud agent runs in a vendor's cloud, so it can't use stdio or reach your
loopback address. It connects to `/mcp` over HTTPS through a tunnel you run, with
a [remote client token](../guide/access/scoped-tokens#remote-tokens) as its
bearer token. The [Cloud agents guide](../guide/cloud-agents) sets it up.

| Cloud agent   | Through a tunnel with a remote token | What it needs                                                                                                   | Checked with |
| ------------- | ------------------------------------ | --------------------------------------------------------------------------------------------------------------- | ------------ |
| Grok Bot      | Yes                                  | Version 0.79 or later: a custom connector with an `Authorization` header. Use a personal Bot, not a Team Bot.   | not yet      |
| Muse (Meta)   | Yes                                  | A custom connector made in chat; paste the token into Muse's secure credential prompt.                          | not yet      |
| ChatGPT (web) | Not yet                              | Its connectors take OAuth or no auth, with no header field, so it needs an OAuth sign-in Wayfindr doesn't have. | n/a          |

Wayfindr serves these agents as clients, the same as any other, and adds nothing
for them to the tools it exposes. An agent may ask you to confirm each
`call_tool` call, because Wayfindr can't tell it which upstream tools only read.
The agent's own settings decide that.

### Manual checks

Each client and transport pair is checked by hand against this list. A client
that passes all of it is "checked"; one that passes only some is "partly".

| Check                  | Passes when                                                                                                       |
| ---------------------- | ----------------------------------------------------------------------------------------------------------------- |
| Install                | The client shows `wayfindr` as connected, with no auth or protocol error.                                         |
| Exactly two tools      | The tool list is `search_tools` and `call_tool`.                                                                  |
| Search                 | A search returns a matching tool with its schema, and an `unavailable` entry for an unauthorized OAuth connector. |
| Call                   | `call_tool` returns the upstream tool's result.                                                                   |
| `needs_auth` link      | The agent shows the advice and the auth link, and the link opens the Continue page.                               |
| Daemon restart         | After the daemon restarts mid-session, calls work again without restarting the client.                            |
| Shim starts the daemon | stdio only: with no daemon running, connecting the client starts one.                                             |

Only Claude Code 2.1.326 and Codex 0.169.0 have been checked so far, on macOS on
2026-11-16, over both transports. Install, exactly two tools, search, call, and a
[profile](../guide/access/profiles) pass; so do a
[scoped client token](../guide/access/scoped-tokens) over HTTP and the shim
starting the daemon over stdio. The `needs_auth` link, the daemon restart, and
tool questions are not checked yet, so those pairs are "partly checked".

- Claude Code negotiates 2026-07-28.
- Codex negotiates 2025-06-18, so over HTTP it isn't asked
  [tool questions](#questions-from-upstreams-elicitation); use `wayfindr stdio`
  if your tools ask them. In `codex exec`, `call_tool` needs an approval
  setting; see [Clients](../guide/clients#codex).

Results go in the table above as each client is checked. If a client misbehaves with Wayfindr, please
[open an issue](https://github.com/atomicpages/wayfindr/issues) with the client,
its version, the transport, and the protocol version it negotiated. The daemon
doesn't log the protocol version; your client's debug log has it (for example
`claude --debug-file <path>`, or Codex with `RUST_LOG=rmcp=info`).

### Things to know

- **Stateless sessions.** A client that insists on `Mcp-Session-Id` would fail
  against `/mcp`. None of the clients above is known to; if one does, use
  `wayfindr stdio` with it and report it.
- **Undeclared capabilities.** A client that calls `prompts/list` or
  `resources/list` without checking the server's capabilities gets a `-32601`
  error. Well-behaved clients don't.
- **Permission prompts.** Clients see only `call_tool`, not the upstream tool
  behind it, so a per-tool "always allow" covers every upstream tool at once.
  Use a
  [tools list](../guide/access/tools-lists#your-client-s-permission-prompt) to
  keep tools away from agents instead. Each upstream tool's `annotations` are
  passed through in `search_tools` results.

## Conformance suite

Wayfindr's `/mcp` is tested with the official
[MCP conformance suite](https://github.com/modelcontextprotocol/conformance)
(`@modelcontextprotocol/conformance` 0.2.0-alpha.12, `--suite all`) at each
supported version. Every result matches the expected baseline.

| Version    | Passed | Failed | Warnings |
| ---------- | ------ | ------ | -------- |
| 2025-06-18 | 30     | 22     | 0        |
| 2025-11-25 | 36     | 25     | 4        |
| 2026-07-28 | 88     | 40     | 4        |

Every protocol scenario passes. The failures and warnings are expected and none
is about protocol conformance: they call fixture tools or prompts and resources
that only the suite's reference server has, and Wayfindr exposes only
`search_tools` and `call_tool`.

The same suite also tests Wayfindr as the client of a test MCP server and
authorization server, at 2025-11-25 and 2026-07-28. It scores 15 of 18
scenarios at 2025-11-25 and 27 of 32 at 2026-07-28. Enterprise-Managed
Authorization fails, as it isn't supported.

The expected results, one check at a time, are in
[`scripts/conformance-baseline/`](https://github.com/atomicpages/wayfindr/tree/main/scripts/conformance-baseline).
Contributors run the suite with `make conformance` and `make conformance-client`;
[the contributor notes](https://github.com/atomicpages/wayfindr/blob/main/docs/internal/compat.md)
say how.
