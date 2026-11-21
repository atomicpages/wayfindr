# Clients

A client is anything that connects to Wayfindr over MCP: Claude Code, Codex,
OpenCode, Cursor, or another harness. Run `wayfindr init` first: the shim can't
start without the client token it writes. Then most harnesses can add Wayfindr
with one command that uses the stdio shim:

| Harness            | Command                                                                                     |
| ------------------ | ------------------------------------------------------------------------------------------- |
| Claude Code        | `claude mcp add --scope user --transport stdio wayfindr -- wayfindr stdio`                  |
| Codex              | `codex mcp add wayfindr -- wayfindr stdio`                                                  |
| OpenCode           | `opencode mcp add --global wayfindr -- wayfindr stdio`                                      |
| Hermes Agent       | `hermes mcp add wayfindr --command wayfindr --args stdio`                                   |
| Gemini CLI         | `gemini mcp add --scope user wayfindr wayfindr stdio`                                       |
| GitHub Copilot CLI | `copilot mcp add wayfindr -- wayfindr stdio`                                                |
| VS Code            | `code --add-mcp '{"name":"wayfindr","type":"stdio","command":"wayfindr","args":["stdio"]}'` |

Each command writes to the harness's own user config, so Wayfindr is available
in every project. For [Cursor](#cursor), [Zed](#zed),
[Windsurf](#windsurf-devin-desktop), and [goose](#goose), add an entry to a
config file instead. `wayfindr init` prints all of these. The sections below
have the details, and HTTP versions where the harness supports them.

Every client sees the same two tools, `search_tools` and `call_tool`, and the
same connectors. You configure connectors once, in Wayfindr, not in each client.
To give a client or a project less, see
[Control what agents can use](./access/).

## Two ways to connect

|                | stdio shim                                                                | HTTP                                                  |
| -------------- | ------------------------------------------------------------------------- | ----------------------------------------------------- |
| Client runs    | `wayfindr stdio`                                                          | nothing; it connects to `http://127.0.0.1:7777/mcp`   |
| Daemon         | started on demand if it isn't running                                     | must already be running                               |
| Client token   | read by the shim from `WAYFINDR_CLIENT_TOKEN`, else `<data>/client-token` | sent by the client as `Authorization: Bearer <token>` |
| Needs          | `wayfindr` on the client's PATH                                           | `WAYFINDR_CLIENT_TOKEN` set where the client runs     |
| Tool questions | asked at every MCP version                                                | asked only at MCP 2026-07-28                          |
| Project scope  | the project's `.wayfindr.toml` applies; a profile with `--profile <name>` | a profile with `?profile=<name>` on the URL           |

Use the **stdio shim** unless you have a reason not to. It reads the client
token itself, so nothing has to be in the client's environment, and it starts
the daemon in the background when it isn't running. A daemon started this way
logs to `wayfindr.log` in the data directory. It also passes on
[questions a tool asks you](#when-a-tool-asks-a-question) whatever MCP version
your client speaks.

Use **HTTP** when the daemon already runs as a service
([`wayfindr service install`](./running#as-a-login-service)) and your client
passes your shell's environment through, or when a client only speaks Streamable
HTTP.

Configure one Wayfindr entry per client, stdio or HTTP, not both.

### Setting up HTTP

Start the daemon (`wayfindr start`), then export the
[client token](../reference/paths-and-environment#client-token) where the client
runs, as `init` prints:

```sh
export WAYFINDR_CLIENT_TOKEN="$(cat ~/.local/share/wayfindr/client-token)"
```

Every HTTP example on this page reads the token from `WAYFINDR_CLIENT_TOKEN` in
the harness's environment. None of them contains the token itself.

Set it in the environment the client actually starts in. A desktop app launched
from the Dock or a launcher doesn't see variables exported in your shell
profile; use the stdio shim for those.

The examples use the default address, `127.0.0.1:7777`. If you change
`[server] listen`, see
[Changing the listen address](./running#changing-the-listen-address).

## Claude Code

::: code-group

```sh [stdio]
claude mcp add --scope user --transport stdio wayfindr -- wayfindr stdio
```

```json [HTTP: .mcp.json]
{
  "mcpServers": {
    "wayfindr": {
      "type": "http",
      "url": "http://127.0.0.1:7777/mcp",
      "headers": {
        "Authorization": "Bearer ${WAYFINDR_CLIENT_TOKEN}"
      }
    }
  }
}
```

:::

Claude Code expands `${WAYFINDR_CLIENT_TOKEN}` from its environment.

## Codex

::: code-group

```sh [stdio]
codex mcp add wayfindr -- wayfindr stdio
```

```sh [HTTP]
codex mcp add wayfindr --url http://127.0.0.1:7777/mcp --bearer-token-env-var WAYFINDR_CLIENT_TOKEN
```

```toml [By hand: ~/.codex/config.toml]
[mcp_servers.wayfindr]
command = "wayfindr"
args = ["stdio"]

# or, for HTTP:
# url = "http://127.0.0.1:7777/mcp"
# bearer_token_env_var = "WAYFINDR_CLIENT_TOKEN"
```

:::

Both commands write an `[mcp_servers.wayfindr]` table to `~/.codex/config.toml`,
which you can also edit by hand.

Codex asks before it runs `call_tool`, but not `search_tools`, which is marked
read-only. `codex exec` can't ask, so the call fails with
`MCP tool call requires approval, but approval policy is never`. To let
unattended runs call tools, approve `call_tool` in the config:

```toml
[mcp_servers.wayfindr.tools.call_tool]
approval_mode = "approve"
```

This approves every upstream tool at once. To keep some away from the agent, use
a [tools list](./access/tools-lists#your-client-s-permission-prompt).

## OpenCode

::: code-group

```sh [stdio]
opencode mcp add --global wayfindr -- wayfindr stdio
```

```json [HTTP: ~/.config/opencode/opencode.json]
{
  "mcp": {
    "servers": {
      "wayfindr": {
        "type": "remote",
        "url": "http://127.0.0.1:7777/mcp",
        "oauth": false,
        "headers": {
          "Authorization": "Bearer {env:WAYFINDR_CLIENT_TOKEN}"
        }
      }
    }
  }
}
```

:::

Without `--global`, OpenCode writes to the current project's config instead.
`"oauth": false` tells OpenCode not to start an OAuth sign-in with Wayfindr. The
bearer header is the credential.

## Hermes Agent

::: code-group

```sh [stdio]
hermes mcp add wayfindr --command wayfindr --args stdio
```

```yaml [HTTP: ~/.hermes/config.yaml]
mcp_servers:
  wayfindr:
    url: "http://127.0.0.1:7777/mcp"
    headers:
      Authorization: "Bearer ${WAYFINDR_CLIENT_TOKEN}"
```

:::

Hermes resolves `${WAYFINDR_CLIENT_TOKEN}` from `~/.hermes/.env`, then from its
process environment. If it is unset, the connection fails and Hermes names the
missing variable.

## Gemini CLI

::: code-group

```sh [stdio]
gemini mcp add --scope user wayfindr wayfindr stdio
```

```json [stdio: ~/.gemini/settings.json]
{
  "mcpServers": {
    "wayfindr": { "command": "wayfindr", "args": ["stdio"] }
  }
}
```

```json [HTTP: ~/.gemini/settings.json]
{
  "mcpServers": {
    "wayfindr": {
      "httpUrl": "http://127.0.0.1:7777/mcp",
      "headers": {
        "Authorization": "Bearer ${WAYFINDR_CLIENT_TOKEN}"
      }
    }
  }
}
```

:::

Without `--scope user`, Gemini CLI writes to the current project's
`.gemini/settings.json`.

Over HTTP, Gemini CLI resolves `${WAYFINDR_CLIENT_TOKEN}` from its environment
or a `.env` file. If the variable is unset, it sends an empty token and the
daemon answers 401. Use `httpUrl`, not `url`: Gemini CLI treats `url` as legacy
SSE.

## GitHub Copilot CLI

::: code-group

```sh [stdio]
copilot mcp add wayfindr -- wayfindr stdio
```

```sh [HTTP]
copilot mcp add --transport http --header 'Authorization: Bearer ${WAYFINDR_CLIENT_TOKEN}' wayfindr http://127.0.0.1:7777/mcp
```

```json [HTTP: ~/.copilot/mcp-config.json]
{
  "mcpServers": {
    "wayfindr": {
      "type": "http",
      "url": "http://127.0.0.1:7777/mcp",
      "headers": {
        "Authorization": "Bearer ${WAYFINDR_CLIENT_TOKEN}"
      },
      "tools": ["*"]
    }
  }
}
```

:::

In the HTTP command, the single quotes keep your shell from expanding the
variable, so the command writes the reference, not the token, to
`~/.copilot/mcp-config.json`. Copilot CLI expands it from its environment when
it connects. If you edit the file by hand, keep `"tools": ["*"]`: a server in
the user config needs it.

## VS Code

::: code-group

```sh [stdio]
code --add-mcp '{"name":"wayfindr","type":"stdio","command":"wayfindr","args":["stdio"]}'
```

```sh [HTTP]
code --add-mcp '{"name":"wayfindr","type":"http","url":"http://127.0.0.1:7777/mcp","headers":{"Authorization":"Bearer ${env:WAYFINDR_CLIENT_TOKEN}"}}'
```

:::

Each command adds the entry to your user profile's `mcp.json`. VS Code reads
environment variables there as `${env:NAME}`; a bare `${NAME}` is sent as
written. VS Code started from the Dock or a launcher doesn't see variables
exported in your shell, so prefer the stdio shim unless you start it with `code`
from a shell that has the variable.

## Cursor

::: code-group

```json [stdio: ~/.cursor/mcp.json]
{
  "mcpServers": {
    "wayfindr": {
      "type": "stdio",
      "command": "wayfindr",
      "args": ["stdio"]
    }
  }
}
```

```json [HTTP: ~/.cursor/mcp.json]
{
  "mcpServers": {
    "wayfindr": {
      "url": "http://127.0.0.1:7777/mcp",
      "headers": {
        "Authorization": "Bearer ${env:WAYFINDR_CLIENT_TOKEN}"
      }
    }
  }
}
```

:::

## Zed

stdio, in Zed's `settings.json` (`zed: open settings file`):

```json
{
  "context_servers": {
    "wayfindr": {
      "command": "wayfindr",
      "args": ["stdio"]
    }
  }
}
```

Zed's key is `context_servers`, not `mcpServers`. Use the stdio shim with Zed:
Zed sends remote server header values exactly as written and doesn't expand
environment variables in them, so an HTTP entry would need the token itself.

## Windsurf (Devin Desktop)

The config file is `~/.config/devin/mcp_config.json` (on Windows,
`%AppData%\devin\mcp_config.json`). Cascade's **Open MCP config file** action
opens it.

::: code-group

```json [stdio]
{
  "mcpServers": {
    "wayfindr": {
      "command": "wayfindr",
      "args": ["stdio"]
    }
  }
}
```

```json [HTTP]
{
  "mcpServers": {
    "wayfindr": {
      "serverUrl": "http://127.0.0.1:7777/mcp",
      "headers": {
        "Authorization": "Bearer ${env:WAYFINDR_CLIENT_TOKEN}"
      }
    }
  }
}
```

:::

Press the refresh button in Cascade's MCP panel after you edit the file.

## goose

The config file is `~/.config/goose/config.yaml` (on Windows,
`%APPDATA%\Block\goose\config\config.yaml`).

::: code-group

```yaml [stdio]
extensions:
  wayfindr:
    type: stdio
    name: wayfindr
    cmd: wayfindr
    args: [stdio]
    enabled: true
    timeout: 300
```

```yaml [HTTP]
extensions:
  wayfindr:
    type: streamable_http
    name: wayfindr
    uri: http://127.0.0.1:7777/mcp
    env_keys: [WAYFINDR_CLIENT_TOKEN]
    headers:
      Authorization: Bearer ${WAYFINDR_CLIENT_TOKEN}
    enabled: true
    timeout: 300
```

:::

`goose configure` → **Add Extension** → **Command-Line Extension** with the
command `wayfindr stdio` writes the same stdio entry. Over HTTP, goose
substitutes only variables listed in `env_keys`. It looks each one up in its
environment first, then in its own secret store.

## Other clients

Any MCP client works. Give it one of these:

- **stdio:** command `wayfindr`, arguments `stdio`.
- **Streamable HTTP:** URL `http://127.0.0.1:7777/mcp`, with the header
  `Authorization: Bearer <client token>`.

Wayfindr speaks MCP 2026-07-28, 2025-11-25, and 2025-06-18 to clients. It offers
tools only: requests for prompts, resources, and completions get a "method not
found" error. See [Compatibility](/reference/compatibility).

## When a tool asks a question

Some tools ask you something in the middle of a call: which workspace to use,
whether to go ahead, or to open a link to connect an account. Wayfindr passes
the question to your client, and your answer back to the tool. The question
starts with the connector's name, as in
`linear asks: Which team should own this issue?`.

Your client shows it when it supports elicitation and connects with
`wayfindr stdio`, or over HTTP at MCP 2026-07-28. Over HTTP at an older version,
the call fails with an error that points at `wayfindr stdio`.

You have 10 minutes to answer; after that, the tool is told you cancelled.
[Compatibility](../reference/compatibility#questions-from-upstreams-elicitation)
has the details for each kind of client.

## Limiting what a client can use

- To give a client fewer connectors or tools for one project, use a
  [project scope](./access/project-scopes) or a [profile](./access/profiles).
  The client picks these: `wayfindr stdio` reads the project's `.wayfindr.toml`,
  and `wayfindr stdio --profile <name>` or `?profile=<name>` on the HTTP URL
  names a profile.
- To hold a client, such as an unattended job, to some connectors whatever it
  does, give it a [scoped client token](./access/scoped-tokens).
