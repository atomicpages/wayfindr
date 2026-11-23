# Cloud Agents

A cloud agent is an AI agent that runs on its vendor's computers, not on yours,
and stays on all the time. Wayfindr can give these agents your connectors (the
MCP servers you added to Wayfindr, such as Gmail or Linear) through a tunnel you
run, with a token that reaches only the connectors you pick.

| Agent       | Works with Wayfindr? | How it signs in                                           |
| ----------- | -------------------- | --------------------------------------------------------- |
| Grok Bot    | Yes                  | An `Authorization` header you add to a connector          |
| Muse (Meta) | Yes                  | A key you give Muse's secure credential prompt            |
| ChatGPT     | Not yet              | It can only sign in with OAuth, which isn't supported yet |

The short path: start a tunnel, open the console's **Cloud agents** page, and
follow its six steps. This page walks through each one, then shows the tunnels
and each agent in detail.

## Why you need a tunnel

Wayfindr runs on your computer and only answers requests that come from your
computer. A cloud agent runs somewhere else, so it can't reach it.

A tunnel fixes that. It is a program on your computer that gives Wayfindr a
public `https` address, and passes what arrives there on to Wayfindr. You run it
and you choose which one. Wayfindr never starts, stops, or configures a tunnel.

## How it stays safe

- **One token for one agent.** You make a _remote token_ for each cloud agent.
  It reaches only the connectors you pick for it. It can't use your other
  connectors, and it can't change Wayfindr's settings.
- **Almost nothing else is reachable.** Through the tunnel, only the MCP address,
  `/mcp` (where agents talk to Wayfindr), answers, and only to a remote token.
  Sign-in callbacks pass too. Your default client token (the one `wayfindr init`
  made, which reaches every connector), the console, and everything else stay on
  your computer, even if someone finds your tunnel's address. The full rule is
  in [Security](./security#loopback-only).
- **You can cut it off.** Revoke the token and the agent is refused from its next
  request. Stop the tunnel and every cloud agent is cut off at once.

The token is the only lock on the tunnel, so treat it like a password: give it to
one agent only, and **never paste it into a chat**. The details are in
[Security](./security#loopback-only) and
[ADR 0021](../adr/0021-remote-client-tokens).

## Before you start

You need:

- Wayfindr running, with at least one connector set up. See the
  [Quick Start](./quick-start). Run `wayfindr start` to start it in the
  background.
- A tunnel that gives Wayfindr a public `https` address. If you don't have one,
  [set one up first](#set-up-a-tunnel).
- The agent's account: a Grok Bot, or Muse.

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
   **Next: connectors**. ChatGPT is listed but greyed out: it says "Not yet".

4. **Step 2, Pick connectors.** Tick only the connectors this agent needs, then
   select **Next: public URL**. The agent can find and call tools from these
   connectors and nothing else. If none are listed, add a connector first.

5. **Step 3, Your public address.** Start your tunnel first, then paste the
   `https` address it gives you, such as `https://your-name.ngrok-free.dev`.
   Select **Save and continue**. Wayfindr saves it in your config file as
   `server.public_url`.

6. **Step 4, Create a token.** Keep the suggested name, such as `grok-bot`, or
   change it. The name shows in the call log, so you can tell which agent made a
   call. Select **Create token**.

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
| "… answered 404. Through a tunnel, Wayfindr answers that when the token is …"                 | This is Wayfindr's own 404. The token isn't a remote token, is wrong, or was revoked; or the URL is wrong. Check that you made the token in this wizard and it is still in the list below, that the address is your tunnel's, and the tunnel points at Wayfindr's port. |
| "… refused the request (HTTP 401 or 403)"                                                     | A login or access check in front of Wayfindr, in the tunnel or a proxy, stops the request before Wayfindr sees the token. Turn it off for this address.                                                                                                                 |
| "… answered, but not like Wayfindr's MCP endpoint", or "answered HTTP …"                      | The address reaches something else. Check that the tunnel points at Wayfindr's port and that Wayfindr is running.                                                                                                                                                       |
| "The token works, but the same request without a token got HTTP …"                            | The test can't confirm the path is safe. The tunnel may not be marking requests as coming from the internet. Make it add a forwarding header, as in [the checklist](#any-other-tunnel-or-proxy).                                                                        |

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

   You can't change a token's connectors or its `--remote` setting later. Revoke
   it and make another. See [Scoped Client Tokens](./access/scoped-tokens).

3. Test it. First, with no token, from any computer:

   ```sh
   curl -i https://your-name.ngrok-free.dev/mcp
   ```

   You want the status line `HTTP/2 404` (or `HTTP/1.1 404`) and the exact body
   `404 page not found`. A `404` with any other body, or with an
   `ngrok-error-code` header, comes from the tunnel, not from Wayfindr. For ngrok
   it means the tunnel is off. If you get `401` with `WWW-Authenticate: Bearer`,
   your tunnel makes outside requests look local, which is
   [the not-safe case](#the-not-safe-warning).

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
   address is wrong. A `401` or `403` is a login in front of Wayfindr.

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
- **Only `/mcp` is exposed, if the tool allows path rules.** Wayfindr already
  refuses everything else through a tunnel, but one less open path is one less
  thing to worry about.

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

ChatGPT isn't supported yet. It can't send an `Authorization` header: a ChatGPT
connector signs in with OAuth or with nothing. Wayfindr doesn't yet act as an
OAuth server, which ChatGPT needs.

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

  Both vendors name these settings for their connected apps in general, and they
  may apply differently to a custom connector. Check your agent's approval
  settings after you add Wayfindr.

- **Give each agent its own token** and only the connectors it needs. If one
  leaks, you revoke one token, and nothing else stops.
- **Hide write tools for a read-only agent.** A remote token can use every tool its
  connectors' [tools list](./access/tools-lists) exposes. Set a list such as
  `tools = ["@read"]` on a connector to allow only the tools the upstream marks
  read-only. A tools list applies to every client, so it limits you too when you
  use that connector yourself.
- **Check what each token does.** `wayfindr token list` and the console's
  **Your cloud agents** list show when each token was last used. `wayfindr calls`
  shows what each one called. Revoke what you don't use, with `wayfindr token revoke <name>` or
  the **Revoke** button.
- **Stop the tunnel to cut every cloud agent off.** It takes effect at once, and
  you can start it again whenever you want.
- **Never share a token.** If one is pasted into a chat, an email, or a bug
  report, revoke it and make another.
