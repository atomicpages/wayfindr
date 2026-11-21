# Connectors

A connector is one upstream MCP server. This page covers the life of one: add
it, or import it from a client you already use; fix it when a setting is wrong;
remove it when you're done.

```sh
wayfindr connector add fs -- npx -y @modelcontextprotocol/server-filesystem ~/code
wayfindr connector add deepwiki --url https://mcp.deepwiki.com/mcp
wayfindr connector add exa --url https://mcp.exa.ai/mcp --secret-header x-api-key
wayfindr connector import claude-code
```

## Adding a connector

Each `wayfindr connector add` writes a `[connectors.<name>]` table to
`wayfindr.toml`, keeping your comments and layout, then reports the connector
once it connects. It asks an HTTP server whether it uses OAuth, and stores
`--secret-header` values as [secrets](./secrets). Every flag is in the
[CLI reference](../reference/cli#wayfindr-connector-add).

The tables are plain TOML, and you can write or edit them by hand. A connector
has exactly one of `command` (stdio) or `url` (HTTP), and each transport accepts
only its own keys. This page covers the common cases; every key is in the
[configuration reference](/reference/configuration).

The daemon watches the config file and applies each save within a moment, so you
don't restart anything, whether a command or your editor changed it.
[Reloading](/reference/configuration#reloading) lists what each change does.

For an OAuth server, see [Authentication](./authentication). To limit which of a
connector's tools agents can use, see [Tools lists](./access/tools-lists). For a
server on a private CA or one that takes a client certificate, see
[Private CAs and client certificates](#private-cas-and-client-certificates).

### Names and qualified tool names

The table key is the connector's name and its stable identifier. It must match
`[a-z0-9][a-z0-9_-]{0,62}`: lowercase letters, digits, `_` and `-`, at most 63
characters, starting with a letter or digit. No dots and no capitals.

Clients address every tool by its qualified name, `<connector>.<tool>`. The
`create_issue` tool of the `linear` connector is `linear.create_issue`. Pick
short, stable names: they show up in every search result and every call.

### stdio

```toml
[connectors.fs]
command = "npx"
args    = ["-y", "@modelcontextprotocol/server-filesystem", "~/code"]
env     = { LOG_LEVEL = "warn" }
```

| Key       | Meaning                                                     |
| --------- | ----------------------------------------------------------- |
| `command` | The program to run. Looked up on the daemon's `PATH`.       |
| `args`    | Its arguments.                                              |
| `env`     | Extra environment variables for it, on top of the daemon's. |

Wayfindr starts the process to sync its tools when the daemon starts or the
connector is added, talks MCP to it over stdin and stdout, and logs its stderr
at debug level. A process left unused for 10 minutes is stopped and started
again by the next call; the connector stays `ready` in between, and each new
process re-syncs the tools.

The process inherits the daemon's environment, except Wayfindr's own
`WAYFINDR_*` variables, with `env` on top. When the daemon runs as a service or
was started by the shim, that environment may not be your shell's. If a command
isn't found, use its absolute path.

A leading `~` in `command` and `args` expands to your home directory (see
[Home directory](/reference/configuration#home-directory)). Values in `args` and
`env` can hold [`${env:NAME}` and `${secret:name}` references](./secrets).

### Streamable HTTP

```toml
[connectors.exa]
url     = "https://mcp.exa.ai/mcp"
headers = { "x-api-key" = "${secret:exa_api_key}" }
```

| Key       | Meaning                                                       |
| --------- | ------------------------------------------------------------- |
| `url`     | The server's MCP endpoint. An absolute `http` or `https` URL. |
| `headers` | Headers sent with every request. This is where API keys go.   |
| `auth`    | An OAuth table. See [Authentication](./authentication).       |

`url` and header values can hold `${env:…}` and `${secret:…}` references. HTTP
connectors are re-synced every `refresh_interval` (default `1h`, in the
`[index]` table), so new upstream tools show up without a restart.

### Legacy HTTP+SSE

Older servers speak the HTTP+SSE transport from before Streamable HTTP. You
usually don't need to do anything: when a `url` connector's `initialize` request
is answered with a 4xx other than 401 or 403, Wayfindr retries with SSE and
keeps using SSE for that connector until its config changes.

To skip the attempt, force SSE:

```toml
[connectors.legacy]
url       = "https://example.com/sse"
transport = "sse"
```

`transport` accepts only `"sse"`, and only on `url` connectors.

### Timeouts

`timeout` bounds one `call_tool` on the connector. It is a Go duration string
and defaults to `"2m"`.

```toml
[connectors.slow-reports]
url     = "https://reports.example.com/mcp"
timeout = "10m"
```

A call that runs out of time comes back to the agent as an error result.

## Private CAs and client certificates

A `[tls]` table gives an HTTP connector its TLS trust and its **client
certificate**. It applies to every connection the connector makes: to its MCP
server, and, for an OAuth connector, to its authorization server for discovery,
registration, sign-in, and refresh. See
[`[connectors.<name>.tls]`](/reference/configuration#connectors-name-tls) for
every key. `connector add` and `connector edit` write it with `--ca-file`,
`--cert-file`, and `--key-file`.

### A server on a private CA

A company server whose certificate comes from an internal CA fails with
`tls_untrusted` (or, for the MCP server itself, `unreachable`) until you trust
that CA. Point `ca_file` at its PEM certificate; the system's CAs stay trusted
too:

```toml
[connectors.corp]
url  = "https://mcp.corp.example.com/mcp"
auth = { type = "oauth" }
tls  = { ca_file = "~/corp/ca.pem" }
```

`wayfindr connector add corp --url … --ca-file ~/corp/ca.pem` writes the table,
and its check of whether the server uses OAuth trusts the CA too.

### A server that only takes mutual TLS

Some internal servers need no OAuth: they let in any client with a certificate
their CA issued. Give the connector its certificate and key, and no `[auth]`
table:

```toml
[connectors.metrics]
url = "https://metrics-mcp.corp.example.com/mcp"

[connectors.metrics.tls]
cert_file = "~/corp/client.pem"
key_file  = "~/corp/client.key"
ca_file   = "~/corp/ca.pem"
```

To keep the key out of a file, store it as a secret
(`wayfindr secret set corp_tls_key < client.key`) and set
`key = "${secret:corp_tls_key}"` instead of `key_file`.

An OAuth provider can also take the certificate as the OAuth client's credential,
or issue tokens bound to it. Both are in
[Enterprise identity providers](./authentication-enterprise#the-client-certificate-as-the-oauth-client-s-credential).

### Renewing a certificate

Replace `cert_file` and its key on disk: Wayfindr presents the new pair from the
next connection, with no reload. It warns 7 days before the certificate expires
(`certificate_expiring`), in `wayfindr status`, the daemon log, and, within a
day, to your agent, whether the connector uses OAuth or not. `ca_file` is read
when the connector's client is built, so after replacing it in place, save the
config file or restart the daemon. Editing the `[tls]` table reconnects the
connector and cancels a sign-in in progress, whose token would be bound to the
old certificate.

## Importing from a client

If a client already runs MCP servers, `wayfindr connector import` adds them as
connectors in one edit. It reads the client's files and never changes them. It
works with `claude-code`, `claude-desktop`, `cursor`, `codex`, and `vscode`.

Start with a dry run. It shows what would be added and changes nothing:

```sh
wayfindr connector import claude-code --dry-run
```

```text
Read ~/.claude.json.
Not read: ~/src/app/.mcp.json (in this project; pass --include-project to read it)

  github    https://api.githubcopilot.com/mcp/  (user, ~/.claude.json)
            header Authorization → secret github_authorization
            note: header Authorization stores the value of GITHUB_TOKEN from this shell
  linear    https://mcp.linear.app/mcp  (user, ~/.claude.json)
            OAuth: asked once you confirm

Skipped:
  notes     a connector called notes already exists
  wayfindr  this is Wayfindr itself

Run again without --dry-run to add 2 connectors.
```

A dry run asks no server anything. Then run it again without `--dry-run`. It
asks once, then asks each HTTP server whether it uses OAuth, stores the secrets,
writes the connectors, and reports each one as `connector add` does. Authorize
the OAuth ones with `wayfindr auth login <name>`.

Project files, such as Claude Code's `.mcp.json`, Cursor's `.cursor/mcp.json`,
and VS Code's `.vscode/mcp.json`, come with the repository you're in, so
whoever wrote it chose their servers and the commands they run. Import reads
them only with `--include-project`, and only from the directory you run it in,
not its parents. Pass it only in a project you trust, and check the commands of
`project` entries before you confirm.

### Which values become secrets

Import never writes a credential from a client's config into `wayfindr.toml`:

| In the client's `env` or `headers`         | In the connector                                               |
| ------------------------------------------ | -------------------------------------------------------------- |
| A literal value                            | The secret `<connector>_<key>`, such as `github_authorization` |
| `${VAR}`, set in the shell you import from | Its value, stored as a secret the same way                     |
| `${VAR}`, not set in that shell            | `${env:VAR}`, which the daemon's environment must set          |
| A variable you name with `--plain-env VAR` | Plain text, for harmless values such as a log level            |

`${VAR}` in `args` or a `url` becomes `${env:VAR}`. Arguments and URLs are
otherwise copied as written, so if one holds a credential, move it to a
[secret](./secrets) yourself. The full mapping, the skipped entries, and the
files each client is read from are in the
[CLI reference](/reference/cli#wayfindr-connector-import).

### Remove the servers from the client

Once the servers run through Wayfindr, remove them from the client so they don't
also run there. The command prints how for each one:

| Client         | Remove an imported server                                                      |
| -------------- | ------------------------------------------------------------------------------ |
| Claude Code    | `claude mcp remove <name> -s user` (or `-s project`, `-s local`, as printed)   |
| Codex          | `codex mcp remove <name>`; for a project's `.codex/config.toml`, edit the file |
| Claude Desktop | Delete the entry from `claude_desktop_config.json`                             |
| Cursor         | Delete the entry from `~/.cursor/mcp.json` or `.cursor/mcp.json`               |
| VS Code        | Delete the entry from `mcp.json`                                               |

Then add Wayfindr itself to the client, as in [Clients](./clients).

## Listing connectors

```sh
wayfindr connector list
```

```text
NAME      KIND   TARGET                                                          AUTH
deepwiki  http   https://mcp.deepwiki.com/mcp                                    -
exa       http   https://mcp.exa.ai/mcp                                          header (secret)
fs        stdio  npx -y @modelcontextprotocol/server-filesystem /Users/you/code  -
```

`list` (or `ls`) reads only the config file, so it works with the daemon
stopped. `wayfindr status` shows how each connector is doing.

## Fixing a connector

You don't remove a connector to fix it. Run `wayfindr status` to see what's
wrong; [Connector states](./troubleshooting#connector-states) says what each
state means and what fixes it.

- **Sign-in failed.** Run `wayfindr auth login <name>` again. The connector stays
  configured, so there is nothing to remove first.
- **A wrong or missing setting.** Run `wayfindr connector edit <name>` with the
  flag for that setting. Only that key changes; the other keys, the secrets, and
  the stored sign-in stay.

```sh
wayfindr connector edit gmail --authorize-param prompt=consent
wayfindr connector edit gmail --client-secret
wayfindr connector edit slow-reports --timeout 10m
wayfindr connector edit exa --unset headers.X-Team
wayfindr connector edit linear --url https://mcp.linear.app/mcp
wayfindr connector edit linear --unset auth
```

`--client-secret`, `--secret-header`, and `--secret-env` prompt for the new
value and replace the stored secret, which is the fix for a wrong client secret
too. A changed `scopes`, `authorize_params`, or `redirect_uri` applies at the
next sign-in, and `edit` offers to authorize. A changed `client_id` makes the
connector `needs_auth`, because the stored token belongs to the old client; see
[Changing a connector's OAuth client](./authentication#changing-a-connector-s-oauth-client).
`--unset auth` turns OAuth off: it removes the `auth` table and deletes the
stored sign-in.

A changed `url` makes an OAuth connector `needs_auth` too, because its token is
bound to the old URL; run `wayfindr auth login <name>`. Unlike `add`, `edit` doesn't
ask the server whether it uses OAuth. If `add` couldn't reach the old URL and
wrote no auth table, pass `--oauth` with `--url`.
[`connector edit`](/reference/cli#wayfindr-connector-edit) lists every flag.

`edit` and the other `connector` commands change only that one table. They
refuse an edit that could change anything else, such as connectors written as an
inline `connectors = { … }` table, and leave the file alone. Edit the table by
hand in that case.

### A config with errors

An invalid save doesn't take the daemon down. It keeps running the last good
config and `wayfindr status` shows why the new one was rejected, with every
problem at once:

```text
Config error: the daemon keeps running its last good config.
  load config /Users/you/.config/wayfindr/wayfindr.toml: invalid config: connectors.My_Server: connector name must match [a-z0-9][a-z0-9_-]{0,62}; connectors.demo.args[1]: bad reference at offset 0: "${HOME}" is not ${env:NAME} or ${secret:name}
```

Unknown keys are errors too, so a typo in a key name is caught rather than
ignored. Fix the file and save; the next valid save clears the error.

## Removing a connector

```sh
wayfindr connector remove gmail
```

```text
Removing gmail will:
  remove [connectors.gmail] from ~/.config/wayfindr/wayfindr.toml
  delete its OAuth credential, if one is stored
  delete its secret gmail_client_secret, if it is stored
Remove gmail? [y/N] y
Removed gmail. Deleted its OAuth credential and its secret gmail_client_secret.
```

`remove` says what it will do and asks first; the answer defaults to no.
`--dry-run` only says it, and `--yes` skips the question, which a script needs
because off a terminal there is no one to ask. `rm` is short for `remove`.

`remove` deletes the table, its subtables, and the comment lines directly above
them, and nothing else in the file. Then it cleans up what the connector owned:

- its stored OAuth credential;
- each secret it referenced that nothing left in the config uses. A secret
  another connector or `[embedding]` still uses is kept, and the output says
  which one uses it.

Removing a table by hand, or commenting it out, is different: the daemon drops
the connector but keeps its credential, so putting it back needs no new sign-in.
See [Orphaned credentials](./authentication#orphaned-credentials) to clean those
up, and [`wayfindr secret list`](./secrets#listing-secrets) and
[`wayfindr secret remove`](./secrets#deleting-a-secret) for secrets you no
longer use.

Renaming a connector is a remove plus an add: the new name needs its own
`wayfindr auth login`.
