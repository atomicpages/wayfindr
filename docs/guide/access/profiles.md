# Profiles

A profile is a [project scope](./project-scopes) with a name, kept in your
config instead of the project. A client picks it by name. Use one when your
client connects to Wayfindr over HTTP, without `wayfindr stdio`, or when you
want limits that a repository doesn't carry.

```toml
# ~/.config/wayfindr/wayfindr.toml
[profiles.acme]
connectors = ["github", "linear"]

[profiles.acme.tools]
github = ["@read"]            # ANDed with [connectors.github].tools
```

The keys are the same as in a `.wayfindr.toml`: `connectors` and one
[tools list](./tools-lists) per connector. Profile names follow the
[connector-name rule](/reference/configuration#connector-names). See
[`[profiles.<name>]`](/reference/configuration#profiles-name) for every rule.

A profile only narrows, within what the client's token allows, and the client
picks it and could leave it off; see [how the layers combine](./#how-they-combine).
A connector the token doesn't allow stays out of reach whatever the profile says.

## Pick a profile for a project

The project picks the profile by name, in the client's own per-project config. A
profile is only a name there: what it allows stays in your config.

::: code-group

```sh [Claude Code]
claude mcp add --scope local wayfindr -- wayfindr stdio --profile acme
```

```toml [Codex: .codex/config.toml]
[mcp_servers.wayfindr]
command = "wayfindr"
args = ["stdio", "--profile", "acme"]
```

```json [Cursor over HTTP: .cursor/mcp.json]
{
  "mcpServers": {
    "wayfindr": {
      "url": "http://127.0.0.1:7777/mcp?profile=acme",
      "headers": {
        "Authorization": "Bearer ${env:WAYFINDR_CLIENT_TOKEN}"
      }
    }
  }
}
```

:::

In Claude Code, `--scope local` applies to this project only; use
`--scope project` instead to write the project's `.mcp.json`, which you can
commit. Either entry takes precedence over a user-wide `wayfindr` entry of the
same name.

Any other client works the same way: add `--profile <name>` after
`wayfindr stdio`, or `?profile=<name>` to the HTTP URL. Name the profile once. A
client that sends it twice, such as `--profile` and `?profile=` together, is
refused.

## With a scope file

A profile and a `.wayfindr.toml` both apply when a client names a profile and
runs `wayfindr stdio` in a project with a scope file. A tool must pass your
config's tools list, the profile, and the file.

Check what an agent gets:

```sh
cd ~/src/acme-api
wayfindr scope --profile acme
```

```
Profile: acme
Project scope: /Users/you/src/acme-api/.wayfindr.toml

CONNECTOR  IN SCOPE  PROFILE TOOLS  SCOPE TOOLS
github     yes       @read          @read, create_pull_request
gmail      no        -              -
linear     yes       as configured  as configured
```

`wayfindr scope --profile` reads the profile from the running daemon. An unknown
name is an error that lists the profiles your config defines. See the
[CLI reference](/reference/cli#wayfindr-scope).

## When a profile applies

- **On every request.** The daemon looks the name up in its running config each
  time. After you edit a profile, the daemon reloads the config and the next
  request uses it. No client has to reconnect.
- **Only with a valid config.** A profile that names a connector your config
  doesn't have is a config error, unlike in a scope file. The daemon keeps its
  last good config until you fix it; `wayfindr status` shows the error.
- **Fails closed.** If the client names a profile your config doesn't define,
  Wayfindr refuses every request from it, rather than give it everything.
  `wayfindr stdio --profile` with a name that isn't valid doesn't start.

## What agents see

`search_tools` doesn't return what a profile hides, and naming a connector
outside it in `connectors` gets `No connector is called …`. `call_tool` refuses
a hidden tool with
`github.create_pull_request is hidden by the Wayfindr profile acme.`, and
`wayfindr calls` shows the refusal as `rejected: hidden_by_profile`. The full
messages are in [what agents see](./#what-agents-see).
