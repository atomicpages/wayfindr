# Cloud Agents

A cloud agent is an AI agent that runs on its vendor's computers, not on yours,
and stays on all the time. Wayfindr can give these agents your connectors (the
MCP servers you added to Wayfindr, such as Gmail or Linear) through a tunnel you
run. Each agent reaches only the connectors you pick for it.

| Agent       | Works with Wayfindr? | How it signs in                                                                              |
| ----------- | -------------------- | -------------------------------------------------------------------------------------------- |
| Grok Bot    | Yes                  | An `Authorization` header you add to a connector                                             |
| Muse (Meta) | Yes                  | A key you give Muse's secure credential prompt                                               |
| ChatGPT     | Yes                  | OAuth: you approve it once, in a browser on the computer that runs Wayfindr ([steps](#chatgpt)) |

The short path for Grok Bot and Muse: start a tunnel, open the console's **Cloud
agents** page, and follow its six steps. This page walks through each one, then
shows the tunnels and each agent in detail. ChatGPT has no token, so it has
[its own steps](#chatgpt).

## Why you need a tunnel

Wayfindr runs on your computer and only answers requests that come from your
computer. A cloud agent runs somewhere else, so it can't reach it.

A tunnel fixes that. It is a program on your computer that gives Wayfindr a
public `https` address, and passes what arrives there on to Wayfindr. You run it
and you choose which one. Wayfindr never starts, stops, or configures a tunnel.

## How it stays safe

- **One token for one agent.** You make a _remote token_ for each cloud agent
  that sends a header. It reaches only the connectors you pick for it. It can't
  use your other connectors, and it can't change Wayfindr's settings. ChatGPT
  gets the same limits from an _OAuth grant_ you approve instead.
- **Almost nothing else is reachable.** Through the tunnel, only the MCP address,
  `/mcp` (where agents talk to Wayfindr), answers, and only to a remote token or
  an OAuth grant. Sign-in callbacks pass too. Your default client token (the one
  `wayfindr init` made, which reaches every connector), the console, and
  everything else stay on your computer, even if someone finds your tunnel's
  address. While you have ChatGPT's sign-in on, a few more addresses answer, and
  only those. The full rule is in [Security](./security#loopback-only).
- **You can hold its risky calls.** When you make a remote token or approve an
  OAuth grant, you can name calls that wait for you. The agent is refused until
  you approve them in the console or with `wayfindr approvals`, which a cloud
  agent can't reach. A local agent can't be held this way: it can read your
  default client token and approve its own calls, so only remote tokens and
  grants take approval entries. See [Approval gates](./access/scoped-tokens#approval-gates).
- **You can cut it off.** Revoke the token or the grant and the agent is refused
  from its next request. Stop the tunnel and every cloud agent is cut off at
  once.

The token is the only lock on the tunnel, so treat it like a password: give it to
one agent only, and **never paste it into a chat**. The details are in
[Security](./security#loopback-only),
[ADR 0021](../adr/0021-remote-client-tokens), and, for ChatGPT's sign-in,
[ADR 0022](../adr/0022-authorization-server-for-remote-clients).

## Before you start

You need:

- Wayfindr running, with at least one connector set up. See the
  [Quick Start](./quick-start). Run `wayfindr start` to start it in the
  background.
- A tunnel that gives Wayfindr a public `https` address. If you don't have one,
  [set one up first](#set-up-a-tunnel).
- The agent's account: a Grok Bot, Muse, or a ChatGPT plan that can add a custom
  MCP server.

## Connect an agent from the console

The console walks you through it, and tests the whole path before you give
anything to the agent.

1. Open the console:

   ```sh
   wayfindr ui
   ```

   It opens your browser, already signed in. See [Web Console](./console).

2. Select **Cloud agents** in the left menu. A progress bar shows the six steps.

3. **Step 1, Pick your agent.** Choose **Grok Bot** or **Muse**, then select
   **Next: connectors**. For ChatGPT, which has no token, follow
   [its own steps](#chatgpt) instead.

4. **Step 2, Pick connectors.** Tick only the connectors this agent needs, then
   select **Next: public URL**. The agent can find and call tools from these
   connectors and nothing else. If none are listed, add a connector first.

5. **Step 3, Your public address.** Start your tunnel first, then paste the
   `https` address it gives you, such as `https://your-name.ngrok-free.dev`.
   Select **Save and continue**. Wayfindr saves it in your config file as
   `server.public_url`.

6. **Step 4, Create a token.** Keep the suggested name, such as `grok-bot`, or
   change it. The name shows in the call log, so you can tell which agent made a
   call. Select **Create token**. This form has no field for approval entries: to
   hold some of the token's calls for your approval, create it with
   `wayfindr token create --approval` instead; see
   [Do it from the command line](#do-it-from-the-command-line).

7. **Copy the token now.** The page shows the token once. Wayfindr keeps only a
   hash of it, so it can't show it again. You will give the token to the agent
   in the console's step 6: Grok Bot takes it in an `Authorization` header, and
   Muse in its secure credential prompt. **Keep this page open until the agent is set
   up.** If you leave it, revoke the token and make another.

8. **Step 5, Test the connection.** Select **Test connection**. Wayfindr calls
   your public address the way the agent will. Read the result under
   [what each result means](#what-the-test-result-means).

9. **Step 6, Set up your agent.** The page shows exactly what to type into the
   agent, with a **Copy** button for each value. Follow the section for your
   agent: [Grok Bot](#grok-bot) or [Muse](#muse).

The **Your cloud agents** list at the bottom of the page shows every remote token,
its connectors, and when it was last used. Select **Revoke** to cut one off.

### What the test result means

Each result says what happened and what to do. Fix it, then select **Test
connection** again.

| The result says                                                                               | What it means and what to do                                                                                                                                                                                                                                            |
| --------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| **The connection works**: "It works. A cloud agent with this token reaches …"                 | The agent can reach Wayfindr and see your connectors. Go on to step 6.                                                                                                                                                                                                  |
| **Warning: your tunnel makes outside requests look local**: "Not safe: the tunnel or proxy …" | **Danger: don't give the agent this address.** See [the warning below](#the-not-safe-warning).                                                                                                                                                                          |
| "No public URL is set", or "The public URL is not a valid https address"                      | Step 3 isn't saved, or the address isn't an `https` address. Go back to step 3.                                                                                                                                                                                         |
| "Nothing answered at …"                                                                       | The address doesn't exist, or the tunnel is off. Check the address in step 3 and start the tunnel.                                                                                                                                                                      |
| "The tunnel at … answered 404 itself, not Wayfindr"                                           | The tunnel provider answered, and its tunnel isn't running or isn't connected to Wayfindr. ngrok does this when its agent has stopped. Start the tunnel, then test again.                                                                                               |
| "The secure connection to … failed"                                                           | The address has a typo, uses `http` instead of `https`, or the tunnel serves a certificate for another name. Check the address and the tunnel's certificate.                                                                                                            |
| "… did not answer in time"                                                                    | The tunnel or Wayfindr is stopped or stuck. Check that both are running, then test again.                                                                                                                                                                               |
| "… answered with a web page instead of Wayfindr"                                              | A page is in the way: a login page, a warning page, or a tunnel's "offline" page. Turn off the login or warning page, or check that the tunnel is running. See [ngrok's browser warning](#ngrok-s-browser-warning).                                                     |
| "… redirected the request to …"                                                               | The address sends visitors somewhere else. Use the exact address the tunnel serves, with `https` and no redirect.                                                                                                                                                       |
| "… answered 404. With sign-in (OAuth) off, Wayfindr answers that …"                           | This is Wayfindr's own 404. With ChatGPT's sign-in off, the token isn't a remote token, is wrong, or was revoked; or the URL is wrong. With sign-in on, a bad token gets a 401 instead, so a 404 means the request didn't reach Wayfindr's `/mcp`. Check that you made the token in this wizard and it is still in the list below, that the address is your tunnel's, and the tunnel points at Wayfindr's port. |
| "… answered 401: Wayfindr does not accept this token"                                         | Only with ChatGPT's sign-in on. The token isn't a remote token, is wrong, or was revoked. Check that you made it in this wizard and it is still in the list below.                                                                                                      |
| "… refused the request (HTTP 401 or 403), and not with Wayfindr's own answer"                 | A login or access check in front of Wayfindr, in the tunnel or a proxy, stops the request before Wayfindr sees the token. Turn it off for this address.                                                                                                                 |
| "… answered, but not like Wayfindr's MCP endpoint", or "answered HTTP …"                      | The address reaches something else. Check that the tunnel points at Wayfindr's port and that Wayfindr is running.                                                                                                                                                       |
| "The token works, but the same request without a token got HTTP …"                            | The test can't confirm the path is safe. The tunnel may not be marking requests as coming from the internet. Make it add a forwarding header, as in [the checklist](#any-other-tunnel-or-proxy).                                                                        |

For ChatGPT, **Test the connection** checks Wayfindr's sign-in instead of a token.
It sends no token. The results above that name the address, the tunnel, or a web
page mean the same here. These are the ones only the sign-in test gives:

| The result says                                                                  | What it means and what to do                                                                                                                                                              |
| -------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| "It works. Through the tunnel, a cloud agent finds Wayfindr's sign-in at …"      | Wayfindr's sign-in information is there and names your public address. Go on to ChatGPT.                                                                                                  |
| "Sign-in with OAuth is off …"                                                    | Turn on sign-in, then test again.                                                                                                                                                         |
| "… does not give the sign-in information a cloud agent looks for"                | Wayfindr's sign-in information isn't there. Check that sign-in is on, that the public URL is your tunnel's address, and that the tunnel passes `/.well-known/` and `/oauth/` to Wayfindr. |
| "The sign-in information at … names another address than the public URL"         | Set the public URL to the exact address your tunnel serves, with `https` and the same host name, or point the tunnel at this Wayfindr.                                                    |

For the full list of results, see
[Testing a cloud agent's connection](../reference/http#remote-test).

### The "Not safe" warning

::: danger
If the test says **Warning: your tunnel makes outside requests look local**, with
the message "Not safe: the tunnel or proxy … hides that requests come from the
internet", stop. Do not give the agent this address.
Anyone who learned your default client token could use all your connectors from
the internet, not only the ones you picked.
:::

What it means: Wayfindr tells a request from your own computer apart from one
that came through a tunnel by a header the tunnel adds, such as `X-Forwarded-For`.
Your tunnel adds none, so Wayfindr takes every request from the internet for a
request from your own computer, and the rules for your own computer apply to it.

What to do:

1. Make the tunnel or proxy add a forwarding header, `X-Forwarded-For` or
   `Forwarded`, and keep the public host name. ngrok, Cloudflare Tunnel, and
   Tailscale Funnel do this on their own, so you most often see the warning with
   a proxy you set up yourself. See [the checklist](#any-other-tunnel-or-proxy).
2. Select **Test again**. Go on only when the result is **The connection works**.

## Do it from the command line

This is for people who are comfortable in a terminal. It does the same as the
console.

1. Save your tunnel's address as `server.public_url`. Find your config file with
   `wayfindr config path`, then add this line under the `[server]` line that is
   already there:

   ```toml
   public_url = "https://your-name.ngrok-free.dev"
   ```

   Don't paste a second `[server]` line: a config with two `[server]` tables is
   invalid. Wayfindr applies the change when you save the file. The address is
   for setup text and the console's test, and doesn't let anything in. See
   [`public_url`](../reference/configuration#public-url).

2. Create a remote token with the connectors the agent needs:

   ```sh
   wayfindr token create grok-bot --connector gmail --connector linear --remote
   ```

   ```text
   Created remote client token grok-bot. It can use: gmail, linear.
   wf_s_6kq1…
   This is the only time it is shown; store it where the client can read it.
   ```

   To hold some of the agent's calls until you approve them, add `--approval`
   (repeat it for several entries), such as `--approval @write --approval
   @destructive`. Then answer the calls that wait with
   [`wayfindr approvals`](../reference/cli#wayfindr-approvals) or in the
   console's **Approvals** page. See [Approval gates](./access/scoped-tokens#approval-gates)
   for the entries and what they don't cover.

   You can't change a token's connectors, its `--remote` setting, or its
   approval entries later. Revoke it and make another. See
   [Scoped Client Tokens](./access/scoped-tokens).

3. Test it. First, with no token, from any computer:

   ```sh
   curl -i https://your-name.ngrok-free.dev/mcp
   ```

   You want the status line `HTTP/2 404` (or `HTTP/1.1 404`) and the exact body
   `404 page not found`. A `404` with any other body, or with an
   `ngrok-error-code` header, comes from the tunnel, not from Wayfindr. For ngrok
   it means the tunnel is off. If you get `401` with `WWW-Authenticate: Bearer`
   and nothing after it, your tunnel makes outside requests look local, which is
   [the not-safe case](#the-not-safe-warning). While ChatGPT's sign-in is on, a
   request from the internet gets `401` with
   `WWW-Authenticate: Bearer resource_metadata="https://your-name.ngrok-free.dev/.well-known/oauth-protected-resource/mcp"`
   instead of the `404`. That is right.

   Then, with your token:

   ```sh
   curl -i -X POST https://your-name.ngrok-free.dev/mcp \
     -H "Authorization: Bearer wf_s_…" \
     -H "Content-Type: application/json" \
     -H "Accept: application/json, text/event-stream" \
     -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"curl","version":"0"}}}'
   ```

   You want the status line `HTTP/2 200`, then a body that starts with
   `event: message` and a `data:` line that includes `serverInfo`:

   ```text
   event: message
   data: {"jsonrpc":"2.0","id":1,"result":{"capabilities":{"tools":{}},"protocolVersion":"2025-06-18","serverInfo":{"name":"wayfindr","version":"…"}}}
   ```

   A `404` means the token isn't a remote token, is wrong, or was revoked, or the
   address is wrong. With ChatGPT's sign-in on, a wrong or revoked token gets
   `401` with `error="invalid_token"` in `WWW-Authenticate`. Any other `401`, or
   a `403`, is a login in front of Wayfindr.

Then set up the agent: [Grok Bot](#grok-bot) or [Muse](#muse). Use
`<your public URL>/mcp` as its server address.

## Set up a tunnel

Pick one. All three give you a stable `https` address and add the forwarding
header Wayfindr needs.

| Tunnel            | Good for                                                       | Cost                                |
| ----------------- | -------------------------------------------------------------- | ----------------------------------- |
| ngrok             | The quickest start. No domain of your own.                     | The free plan has one fixed address |
| Cloudflare Tunnel | A domain you already own, and a tunnel that runs as a service. | Needs a domain on Cloudflare        |
| Tailscale Funnel  | You already use Tailscale. The address ends in `.ts.net`.      | Needs a Tailscale account           |

Any of them needs the port Wayfindr listens on. It is `7777` unless you changed
`server.listen`. To check, run `wayfindr ui`: the address it prints, such as
`http://127.0.0.1:7777/ui/`, ends in the port.

### ngrok

1. Create a free account at <https://dashboard.ngrok.com/signup>.

2. Install ngrok.

   - **macOS:** `brew install ngrok`
   - **Windows:** `winget install ngrok -s msstore`, or get it from the
     Microsoft Store, or download it from <https://ngrok.com/download/windows>.
   - **Linux:** `snap install ngrok`, or follow the steps for your system at
     <https://ngrok.com/download/linux>.

3. Add your authtoken. Open
   <https://dashboard.ngrok.com/get-started/your-authtoken>, copy the token, and
   run:

   ```sh
   ngrok config add-authtoken <your-ngrok-authtoken>
   ```

4. Find your free domain. Every account has one, which ngrok names for you. In
   the ngrok dashboard, open **Domains**
   (<https://dashboard.ngrok.com/domains>) and copy the name, such as
   `your-name.ngrok-free.dev`. It doesn't change when you restart ngrok, which
   is why you can save it in your agent.

5. Start the tunnel, in a terminal window you leave open. Use the port you found
   above:

   ```sh
   ngrok http 127.0.0.1:7777
   ```

   On a free account, ngrok serves your free domain by default, so you don't
   have to name it. To name it anyway, add `--url=https://your-name.ngrok-free.dev`
   after `http`. ngrok shows a `Forwarding` line with your address. Use that
   address in the console's step 3.

6. Keep it running. The tunnel works only while this command runs. If you close
   the window or restart your computer, start the command again. The address
   stays the same.

#### ngrok's browser warning

On the free plan, ngrok shows a warning page ("You are about to visit …") before
a site, to deter phishing. It does this for browser-like requests that fetch a
page (a `GET`, such as opening the address in a browser). A person clicks
**Visit** and goes on. An MCP agent's requests are `POST`s, which pass without
the page, and so does the console's test, which also sends a `POST`.

You can still send a header to be safe, in case an agent or a later ngrok change
takes the page for the connector. Add the header `ngrok-skip-browser-warning`
to the agent's requests. Any value works, such as `1`. Grok Bot's connector form
has an **HTTP headers** field, so add it there as a second header, next to
`Authorization`. Muse has no such field.

If an agent still reports a web page or a reply that isn't valid MCP, and
you'd rather not rely on the header:

- **Use a paid ngrok plan.** Paid plans don't show the page.
- **Use another tunnel.** Cloudflare Tunnel and Tailscale Funnel don't show ngrok's page.

### Cloudflare Tunnel

You need a domain that is on Cloudflare. Use a _named_ tunnel. Don't use a quick
tunnel (`cloudflared tunnel --url …`): it gives a new random address each time it
starts, and an agent that saved the old address stops working.

1. In the Cloudflare dashboard, open **Networking**, then **Tunnels**, and select
   **Create a tunnel**.
2. Name the tunnel and select **Create Tunnel**.
3. Choose your operating system. Cloudflare shows an install command. Run it in a
   terminal on your computer. Wait for the tunnel to show as connected, then
   select **Continue**.
4. Open the tunnel, go to its **Routes** tab, and select **Add route**, then
   **Published application**.
5. Choose a subdomain and your **Domain**, such as `wayfindr.example.com`.
6. In **Service URL**, enter `http://127.0.0.1:7777` (your port). Select
   **Add route**.
7. Use `https://wayfindr.example.com` as your public address.

The tunnel runs as a service that starts with your computer.

Cloudflare's dashboard labels change now and then. If a label above isn't there,
look in Cloudflare's guide, [Create a remotely-managed tunnel](https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/get-started/create-remote-tunnel/).

### Tailscale Funnel

You need Tailscale on your computer, with MagicDNS and HTTPS turned on for your
tailnet.

1. Run:

   ```sh
   tailscale funnel 7777
   ```

   The first time, Tailscale opens a page where you approve turning Funnel on.

2. Tailscale prints an address like `https://your-computer.your-tailnet.ts.net`.
   Use it as your public address.

3. Keep the command running. Press Ctrl+C and the tunnel stops. To keep Funnel on
   after you close the terminal, see `tailscale funnel --help`.

### Any other tunnel or proxy

Kong, Caddy, nginx, frp, pinggy, and the like work if they pass this checklist.
The console's test checks most of it.

- **A stable `https` address.** The name must stay the same, because the agent
  saves it. A name that changes on every start breaks the agent.
- **It forwards to `127.0.0.1:<port>`**, Wayfindr's port, and keeps the public
  host name. Don't rewrite it to `127.0.0.1`.
- **It passes the `Authorization` header through unchanged.** Don't let the proxy
  replace or strip it.
- **It adds a forwarding header**, `X-Forwarded-For` or `Forwarded`. This is the
  one most often missed. A plain proxy adds none, so Wayfindr takes outside
  requests for local ones, and the test says
  [Not safe](#the-not-safe-warning). Turn on the proxy's "forwarded headers"
  option, or add the header by hand. In nginx, put this line in the `location`
  block:

  ```nginx
  proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
  ```

  Caddy's `reverse_proxy` adds it for you.

- **No login or warning page in front.** An agent can't sign in to a proxy, or
  click through a page.
- **Only the paths Wayfindr serves are exposed, if the tool allows path rules.**
  Wayfindr already refuses everything else through a tunnel, but one less open
  path is one less thing to worry about. For Grok Bot and Muse that is `/mcp`.
  For ChatGPT it is also `/.well-known/` and `/oauth/`.

Run **Test connection** when you're done. It tells you which item failed.

## Grok Bot

::: warning
Use a personal Bot, not a Team Bot. On a Team Bot, everyone on your team who talks
to the Bot uses your token. A coworker who asks it to summarise their unread email
would read yours.
:::

::: info
These steps follow xAI's documentation and haven't been tested on a live Grok
Bot yet. Wayfindr has been tested through a tunnel with an MCP client that sends
the same requests, and Muse has connected the same way. If a step differs, please
[open an issue](https://github.com/atomicpages/wayfindr/issues).
:::

Grok Bot's connected apps are account-wide: every Bot you run can use them. Only
connect Wayfindr in an account where that's what you want.

The console's step 6 shows these values. In Grok Bot:

1. Open **Connect apps** from the sidebar.
2. Choose **Add a custom connector** at the bottom of the page.
3. Fill in the form:

   | Field        | What to enter                                                                   |
   | ------------ | ------------------------------------------------------------------------------- |
   | Name         | `Wayfindr`                                                                      |
   | Type         | **Remote HTTPS**                                                                |
   | Server URL   | Your public address with `/mcp`, such as `https://your-name.ngrok-free.dev/mcp` |
   | HTTP headers | Name `Authorization`, value `Bearer wf_s_…`                                     |

   Type the word `Bearer`, a space, then your token. If you use ngrok's free
   plan, add a second header, `ngrok-skip-browser-warning` with the value `1`;
   see [ngrok's browser warning](#ngrok-s-browser-warning).

4. Choose **Add plugin** to save it.

Leave **Client ID** empty: Wayfindr's remote tokens don't use OAuth.

Grok Bot's screens change between versions, so the labels may differ a little in
yours. If your team's administrator limits connectors, Grok Bot may refuse to save
the connector, or show it as **Disabled by team admin**; ask your administrator.

## Muse

Muse has no settings page for MCP servers. You ask it in chat to set up a custom
connector.

1. In the console's step 6, select **Copy message**.
2. Open a chat with Muse and send the message. It asks Muse to create a custom
   connector for a remote MCP server at your public address, using an
   `Authorization: Bearer` header, and to ask you for the key through its secure
   credential prompt. The message holds no token.
3. When Muse shows its secure credential prompt, paste the token there.

::: warning
Paste the token only into Muse's secure credential prompt, never into the chat.
Anything you type in the chat stays in your conversation history. If Muse doesn't
offer a secure prompt, stop and don't paste the token.
:::

Meta doesn't review custom connectors, so only add one for a server you run, as
here. Muse's screens and wording change often, so if it asks for something this
page doesn't mention, answer with the same values: your public address with
`/mcp`, and a bearer token. If Muse says it can't add MCP servers, tell it to
build a custom connector that talks to the server as an MCP client, then send the
message again.

## ChatGPT

ChatGPT's custom connector form has no field for a header, so it can't use a
remote token. It signs in with OAuth instead. With sign-in on, Wayfindr answers
ChatGPT's sign-in requests through your tunnel, and you approve ChatGPT once, in
your browser, on the computer that runs Wayfindr. You pick the connectors it may
use at that moment. ChatGPT's always-on dots use the same connector, so there is
nothing more to set up for them.

::: info
These steps follow OpenAI's documentation for custom MCP servers, which changes
often. If a screen differs, look for the same names, or
[open an issue](https://github.com/atomicpages/wayfindr/issues).
:::

You need:

- A tunnel, as in [Before you start](#before-you-start). It must pass every path
  to Wayfindr, because ChatGPT also looks for Wayfindr's sign-in addresses.
- A ChatGPT plan that can add a custom MCP server: on ChatGPT on the web, a
  **Plugins** menu with an option to add one.
- A browser on the computer that runs Wayfindr, to approve the sign-in.

1. Open the console with `wayfindr ui` and select **Cloud agents**.
2. **Pick your agent.** Choose **ChatGPT** and select **Next: connectors**. The
   wizard skips the connectors step and creates no token: you pick the connectors
   when you approve.
3. **Your public address.** Start your tunnel, paste its `https` address, and
   select **Save and continue**, as for the other agents.
4. **Turn on sign-in.** The page says what turning it on opens through the tunnel:
   Wayfindr's sign-in information, and its sign-in and token addresses. Select the
   button to turn on sign-in (OAuth). Wayfindr saves it in your config file as
   `server.oauth.enabled` and arms a connection.
5. **Connect.** The page shows what to enter in ChatGPT and, once armed, "Ready
   for ChatGPT until" a time 10 minutes ahead. An arm is what lets a sign-in
   through: without it, Wayfindr refuses every sign-in request. If the time
   passes, select **Connect a client** to arm again. Select **Test the
   connection** if you like. See
   [what the test result means](#what-the-test-result-means).
6. In ChatGPT on the web, open **Plugins**, select **+**, then **Add custom MCP
   server**, and fill in the form:

   | Field          | What to enter                                                                   |
   | -------------- | ------------------------------------------------------------------------------- |
   | Server URL     | Your public address with `/mcp`, such as `https://your-name.ngrok-free.dev/mcp` |
   | Authentication | **OAuth**                                                                       |

   There is nothing to paste: ChatGPT identifies itself to Wayfindr by its own
   address, so leave any client ID or secret empty.

7. ChatGPT sends your browser to Wayfindr's approve page, on this computer. On
   ngrok's free plan, your browser may first show ngrok's "You are about to
   visit" page: select **Visit Site**. See
   [ngrok's browser warning](#ngrok-s-browser-warning).
8. Check the page. It names the client and its host, the host your browser returns
   to, when the request started, and the IP address your tunnel reported for the
   browser, labelled "reported by your tunnel". Keep the suggested name, such as
   `chatgpt`, or change it: it shows in the call log and in `wayfindr token list`.
   Tick only the connectors ChatGPT needs. Under **Calls that need your
   approval**, you can list calls that wait for you, one per line, such as
   `@destructive` or `github.delete_*`; leave it empty for none. Then select
   **Approve**. Your browser goes back to ChatGPT, which is now connected.

To connect again, for another ChatGPT account or after you changed the public
URL, select **Connect a client** on the **Cloud agents** page. It is on the
ChatGPT row as long as sign-in is on.

### The approve page

The page says: "Approve only if you clicked Connect in ChatGPT in the last few
minutes. If someone sent you this link, or this page opened in a tab you didn't
open, choose Deny."

::: warning
Anyone can start a sign-in to your public address from their own ChatGPT account
and send you the link. If you approve it, their account gets the connectors you
tick. Approve only a request you started a minute ago. If you didn't, or the
start time or the IP address isn't what you expect, select **Deny**.
:::

Three things keep that narrow, and none rules it out. A sign-in is let through
only in the 10 minutes after you select **Connect a client**, and when you open the
approve page for your own request, every other waiting request is dropped. And
only one request is expected in those minutes, yours: if more than one arrived,
the page can't approve any of them. See
[Security](./security#cloud-agents-that-sign-in) for the whole picture.

#### When more than one request arrived

A web page you have open can open the approve page in a new tab with a
stranger's request, while you are connecting. It looks like your own: the plain
page with a **Continue** button. So Wayfindr counts the requests that arrived
after you selected **Connect a client**. When there was more than one, the approve
page shows no **Approve** button and says, for example:

> 2 connection requests arrived after you selected Connect a client. Only one was
> expected, so this can't be approved. Choose Deny, then select Connect a client
> and try again in ChatGPT.

Select **Deny**, then **Connect a client** on the **Cloud agents** page, and add
the connector in ChatGPT again. A stranger's request costs you that retry, never
a grant. It also happens if ChatGPT itself sent two requests, such as when you
selected its connect button twice.

You approve only from a browser on the computer that runs Wayfindr. The approve
page is part of the console, which answers only on that computer's loopback
address, so it never opens from your phone or another computer, even when ChatGPT
is running there. Start the sign-in from a browser on the Wayfindr computer.

The first page may be a plain one with a **Continue** button, because your browser
arrives from another site and the console doesn't know you yet. It says:
"Continue only if you just selected Connect in an app. If this tab opened without
you opening it, or from a link someone sent you, close it instead." Select
**Continue**. If it comes back, you aren't signed in: run `wayfindr ui`, then
select **Continue** again. The request waits until the time the Cloud agents page
showed after you selected **Connect a client**. If the page says the request
expired or was already answered, start connecting again in ChatGPT.

### Cutting ChatGPT off

- `wayfindr token list` shows the grant with the kind `oauth` and ChatGPT's host,
  next to your tokens. `wayfindr token revoke <name>` deletes it, and ChatGPT is
  refused from its next request. The console's lists have a **Revoke** button for
  it too.
- ChatGPT's access lasts an hour and renews on its own. You do nothing.
- Turning sign-in off, changing the public URL, or removing ChatGPT from
  `server.oauth.clients` suspends the grant: ChatGPT is refused until you undo the
  change, and then it works again. It also ends a **Connect a client** in progress:
  a request waiting for approval is dropped, so select **Connect a client** again
  to connect.

### Not supported

OpenAI's Secure MCP Tunnel is not supported. Wayfindr works through a tunnel you
run, the same one for every cloud agent.

## Good habits

- **Expect questions before calls.** By default, an agent asks you before each
  `call_tool`, because it can't tell a read from a write. You see the tool name,
  such as `gmail.send_message`, and its arguments in the prompt. Keep them on
  until you trust the agent. To change how often an agent asks, use the agent's
  own settings.
  - In Grok Bot, an approval card offers **Allow once**, **Always allow**, and
    **Deny**. Auto-review and **Custom Rules** are under **Settings**, then
    **General**, in the **Bot** group.
  - In Muse, open **Settings**, then **Permissions**. **Ask for some actions** and
    **Always ask** set how often it asks, and you can set permissions for each
    connector.

  - ChatGPT can ask you to confirm a custom connector's tool calls. Keep that on
    until you trust it.

  The vendors name these settings for their connected apps in general, and they
  may apply differently to a custom connector. Check your agent's approval
  settings after you add Wayfindr.

- **Give each agent its own token** and only the connectors it needs. If one
  leaks, you revoke one token, and nothing else stops. For ChatGPT, tick only the
  connectors it needs when you approve it.
- **Hide write tools for a read-only agent.** A remote token can use every tool its
  connectors' [tools list](./access/tools-lists) exposes. Set a list such as
  `tools = ["@read"]` on a connector to allow only the tools the upstream marks
  read-only. A tools list applies to every client, so it limits you too when you
  use that connector yourself.
- **Hold risky calls for your approval.** Give a remote token `--approval
  @write --approval @destructive`, or approve ChatGPT's connection with those
  entries, and the agent can't run those tools until you have read the
  arguments and approved them. An approval covers one call with exactly those
  arguments, and lasts 1 hour. The agent has to make the same call again to use
  it; if it changes any argument, that is a new call and waits for a new
  approval. A pending call waits 24 hours for you. A client can have 8 calls in
  the queue at once, and past that its new calls are refused until some are
  used or expire; a repeat of a call already in the queue still gets its
  answer. Restarting Wayfindr drops the queue, denials included, so the agent's
  next try waits again. Wayfindr gives the agent no way to answer for you.
- **Check what each token does.** `wayfindr token list` and the console's
  **Your cloud agents** list show when each token was last used. `wayfindr calls`
  shows what each one called. Revoke what you don't use, with `wayfindr token revoke <name>` or
  the **Revoke** button.
- **Stop the tunnel to cut every cloud agent off.** It takes effect at once, and
  you can start it again whenever you want.
- **Never share a token.** If one is pasted into a chat, an email, or a bug
  report, revoke it and make another.
