# Web Console

The web console shows the running daemon in your browser: every connector at
once, what agents called, what search returns, and your client tokens. It runs
inside the daemon, on the same loopback address, and needs nothing else
installed.

```sh
wayfindr ui
```

```text
Opened the console at http://127.0.0.1:7777/ui/.
```

The command signs your browser in with a one-time link. To open the console
somewhere else, such as another browser, print the link instead:

```sh
wayfindr ui --no-open
```

The link works once, for one minute. After that the browser stays signed in
until it is idle for 12 hours or the daemon stops. Run `wayfindr ui` again to
sign in again.

## What it shows

| Page          | What you see                                                                                        | What you can do                                                                          |
| ------------- | --------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------- |
| Connectors    | Each connector's state, its advice when it needs you, tools in search, last sync, and token expiry. | Add a connector, sign one in.                                                            |
| A connector   | Every tool its upstream serves now, its class, and whether its tools list exposes it.               | Open a tool to read its definition as the upstream sends it. Sign in again, or sign out. |
| Calls         | The call log, newest first, filtered by connector or by calls that did not end OK.                  | Open a call that failed to read its error.                                               |
| Search        | What `search_tools` returns to a client for a query, with each tool's word and meaning ranks.       |                                                                                          |
| Client tokens | Your scoped client tokens, their connectors, and when each was last used.                           | Create a token (shown once), revoke one.                                                 |

A connector whose name matches a brand (`gmail`, `github-work`) shows that
brand's logo, which your browser loads from [theSVG](https://thesvg.org) on
`cdn.jsdelivr.net`; other connectors, and every connector while you are offline,
show their first letter. See
[ADR 0019](/adr/0019-connector-logos-from-thesvg) for why.

The connectors page refreshes every 10 seconds. After you select **Sign in**,
finish the sign-in in the tab that opens; the connector's row turns `Ready` on
the next refresh. For a connector with `grant = "device_code"`, the tab shows a
code and a link to the provider's page, where you enter it; see
[Signing in without a browser on this machine](./authentication#signing-in-without-a-browser-on-this-machine-device-code).

The console shows what the CLI shows: it reads the same answers from the daemon
as [`wayfindr status`](/reference/cli#wayfindr-status),
[`calls`](/reference/cli#wayfindr-calls),
[`search --explain`](/reference/cli#wayfindr-search),
[`connector tools`](/reference/cli#wayfindr-connector-tools), and
[`token list`](/reference/cli#wayfindr-token-list).

## Adding a connector

Select **Add connector** on the connectors page. Pick the kind, give it a name,
and fill in how to reach it:

| Kind          | You enter                                                                                           |
| ------------- | --------------------------------------------------------------------------------------------------- |
| Remote URL    | The server's URL and how it signs in: detect automatically, OAuth, an API key in a header, or none. |
| Local command | The command, its arguments (one per line), plain environment variables, and secret variables.       |

The daemon adds the connector's table to the end of your config file, the way
[`wayfindr connector add`](/reference/cli#wayfindr-connector-add) does, and
leaves the rest of the file as it was. Header and variable values you mark as
secret are stored encrypted, and the file gets a `${secret:…}` reference
instead. The connector then connects, and its page opens. When it uses OAuth,
select **Sign in** there to finish.

**Local commands are reviewed first.** A command connector runs a program on
your computer, as you, each time Wayfindr connects. So the console doesn't add
one straight from the form: it shows the exact command, each argument, and the
environment (secret values masked) and waits for you to select **Add and run**.
The review lasts 5 minutes. **Back** returns to the form.

If the file changed while the connector was being added, for example because you
saved it in an editor, nothing is written and the form says so. Add the
connector again.

## What it doesn't do

The console never edits or removes a connector, and doesn't import connectors
from other clients. Use
[`wayfindr connector edit`](/reference/cli#wayfindr-connector-edit),
[`connector remove`](/reference/cli#wayfindr-connector-remove), and
[`connector import`](/reference/cli#wayfindr-connector-import) for those, and
the CLI for tools lists, profiles, and secrets. When a connector needs a secret,
its row shows the daemon's advice, such as the
[`wayfindr secret set`](/reference/cli#wayfindr-secret-set) command to run.

## Security

The console is served only on the daemon's loopback address, behind the same
[Host and Origin checks](/reference/http#host-and-origin-checks) as every other
route. Your client token never reaches the browser: `wayfindr ui` trades it for
a one-time code, and the browser trades the code for a session cookie that only
the console's pages can use. Every form also carries a token tied to your
session. See [`/ui/`](/reference/http#ui) for the details.
