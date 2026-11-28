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
| A connector   | Every tool its upstream serves now, its class, and whether its tools list exposes it.               | Open a tool to read its definition as the upstream sends it. **Try it** on an exposed tool runs one real call from a form ([Testing a tool](./testing-tools)). Sign in again, or sign out. |
| Calls         | The call log, newest first, with each result's size, filtered by connector or by calls that did not end OK.            | Open a call that failed to read its error.                                               |
| Search        | What `search_tools` returns to a client for a query, with each tool's word and meaning ranks.       |                                                                                          |
| Client tokens | Your scoped client tokens and OAuth grants (marked `oauth` with the client's host), their connectors, which are remote, and when each was last used. | Create a token (shown once), revoke a token or a grant.                          |
| Approvals     | The calls of remote clients that wait for you: client, tool, class, state, age, and every argument. | Approve or deny a call, revoke an approval ([Approving calls](#approving-calls)).       |
| Cloud agents  | The remote tokens and OAuth grants you made for cloud agents, their connectors, and when each was last used. | Connect Grok Bot, Muse, or ChatGPT step by step, test the connection, revoke a token or grant. |

A connector whose name matches a brand (`gmail`, `github-work`) shows that
brand's logo, which your browser loads from [theSVG](https://thesvg.org) on
`cdn.jsdelivr.net`; other connectors, and every connector while you are offline,
show their first letter. See
[ADR 0019](../adr/0019-connector-logos-from-thesvg) for why.

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

## Connecting a cloud agent

The **Cloud agents** page walks you through connecting an agent that runs in the
cloud, such as Grok Bot, Muse, or ChatGPT. For Grok Bot and Muse, you pick the
agent and the connectors it may use, save the public `https` address your tunnel
(ngrok, Cloudflare Tunnel, or Tailscale Funnel) gives you, and create a remote
token, which is shown once. **Test connection** then checks the whole path from
the internet to the daemon, and the page ends with text to copy into the agent.
Keep the page open until the agent is set up: Wayfindr keeps only a hash of the
token, so leaving the page loses it. Revoke a token from the same page's list. The
[Cloud agents guide](./cloud-agents) covers the tunnel and each agent in full.

**ChatGPT** has no token. Its path asks for the public address, then turns on
sign-in (OAuth) with one button, which saves `server.oauth.enabled` in your config
file and arms a connection. The last step shows what to enter in ChatGPT, a
**Connect a client** button, and, once armed, "Ready for ChatGPT until" a time 10
minutes ahead. The arm is what lets a sign-in through. You can **Test the
connection** and turn sign-in off from the same step. While sign-in is on, the
first step shows **Connect a client** on the ChatGPT row too, so you can connect
again without going through the steps.

When ChatGPT sends your browser to Wayfindr, it lands on the console's **approve
page**, on this computer. It shows the client and its host, the host your browser
returns to, when the request started, and the IP address your tunnel reported,
labelled "reported by your tunnel". It warns you to approve only if you clicked
Connect in the client a few minutes ago, and to choose Deny if someone sent you the
link. You keep or change the name, tick the connectors, optionally list the calls
that need your approval (one entry per line, [as for a token](./access/scoped-tokens#approval-gates)),
and select **Approve** or **Deny**; your browser then goes back to the client. A plain page with a
**Continue** button may come first, because your browser arrives from another site
and the console doesn't know you yet: select **Continue**. If it comes back, you
aren't signed in: run `wayfindr ui`, then select **Continue** again. See
[ChatGPT](./cloud-agents#chatgpt) for the steps, and
[Security](./security#cloud-agents-that-sign-in) for what the sign-in opens.

For Grok Bot, use a personal Bot, not a Team Bot: everyone on a team uses a Team
Bot's token. For Muse, paste the token only into Muse's secure credential
prompt, never into the chat. See
[Remote tokens](./access/scoped-tokens#remote-tokens) for what one can do, and
[ADR 0021](../adr/0021-remote-client-tokens) for why remote tokens exist.

## Approving calls

A remote client can have some of its calls held until you approve them. You
name those calls with approval entries when the client is created, such as
`@destructive` for every destructive tool
([the entries](./access/scoped-tokens#approval-gates)). The console takes them in
one place only: the approve page for an OAuth connection, such as ChatGPT's. Its
form for a remote token has no approval field. Create that token with
`wayfindr token create --remote --approval <entry>` instead. Entries are fixed
at creation.

When such a client makes a gated call, Wayfindr refuses it and the call waits on
the **Approvals** page. The sidebar shows how many wait, and hides the
count when none do.

Each row shows the client, the tool and its class, the size of the arguments,
and the arguments. The arguments are always shown in full, never cut and never
in a scroll box, and unredacted: a secret an agent puts in one is shown to you.
They are shown as they decode, which is how the approval reads them: an escape
such as `\u0072` shows as the `r` it stands for, numbers stay as sent, and keys
are shown in sorted order (the order doesn't change the call). Arguments nested
very deeply, or that would take far more room laid out than as sent, are shown
on one line.
Characters you can't see or that a terminal would obey, such as escape
sequences, zero-width and other blank characters, and spaces other than the
plain one, are shown as `\uXXXX` escapes, or `\U000XXXXX` above U+FFFF, and
bytes that aren't valid UTF-8 as `\xXX`. So is each plain space after the
second in a row, and each combining mark after the second on one character, so
nothing can push text out of view. The client and tool names, the class, and
the id get the same escapes. A size far larger than the arguments you can see
means something is hiding; read them again before you approve.

| State    | Buttons             | What it means                                                                                  |
| -------- | ------------------- | ---------------------------------------------------------------------------------------------- |
| Waiting  | Approve, Deny       | The client's call was refused and waits for you. It expires 24 hours after it was made.        |
| Approved | Revoke              | The client's next identical call runs once. It expires 1 hour after you approved it.           |
| Denied   | Approve             | Matching calls are refused until the entry expires. You can still approve it.                  |

An approval covers one call with exactly the arguments you saw. After you
approve, the agent has to make the same call again, and that call runs and uses
the approval up. A call with any other argument is a new entry. **Revoke** is a
deny: it withdraws an approval before the agent uses it.

The page refreshes every 10 seconds, except while your pointer or focus is on
the list and for 5 seconds after you tap, click, scroll, or type. An entry leaves the list when it expires
or when the agent uses its approval up. If you answer an entry that is already
gone, or whose id now names another call because Wayfindr restarted, nothing
changes and the console shows the daemon's message. Reload the page and read
the call again.

Only you can answer: the console answers on this computer's loopback address,
which a remote client cannot reach. `wayfindr approvals` lists and answers the
same entries in a terminal.

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
route. A request through a tunnel never reaches it, whatever token it carries
(see [Loopback only](./security#loopback-only)). Your client token never reaches the browser: `wayfindr ui` trades it for
a one-time code, and the browser trades the code for a session cookie that only
the console's pages can use. Every form also carries a token tied to your
session. See [`/ui/`](/reference/http#ui) for the details.
