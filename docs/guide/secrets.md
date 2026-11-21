# Secrets

Keep API keys and client secrets out of `wayfindr.toml`. Reference them instead:

```toml
[connectors.exa]
url     = "https://mcp.exa.ai/mcp"
headers = { "x-api-key" = "${secret:exa_api_key}" }

[connectors.github]
url     = "https://api.githubcopilot.com/mcp/"
headers = { Authorization = "Bearer ${env:GITHUB_TOKEN}" }
```

Then store the secret:

```sh
wayfindr secret set exa_api_key
```

```text
Value for secret exa_api_key:
Secret exa_api_key set.
exa is ready.
```

`${secret:name}` reads a secret that Wayfindr stores encrypted. `${env:NAME}`
reads an environment variable of the daemon. The config file keeps the
reference, never the value, so you can commit it to your dotfiles.

`wayfindr connector add` does both steps for you. `--secret-header` (HTTP) and
`--secret-env` (stdio) prompt for the value, store it as the secret
`<connector>_<key>`, and write the reference:

```sh
wayfindr connector add exa --url https://mcp.exa.ai/mcp --secret-header x-api-key
```

```text
Value for header x-api-key:
Stored secret exa_x-api-key.
Added connector exa.
exa is ready (3 tools).
```

The name is lowercased, characters a secret name can't hold become `_`, and it
is cut to 63 characters. Two connectors can make the same name (`google` with
`DRIVE_TOKEN` and `google_drive` with `TOKEN` both make `google_drive_token`),
so a name another connector or `[embedding]` already uses is refused before you
are asked for the value: storing it would change what that one sends. Pick
another connector name or key. See
[`wayfindr connector add`](../reference/cli#wayfindr-connector-add).

## Secret names

A new secret's name follows the same rule as a connector's,
`[a-z0-9][a-z0-9_-]{0,62}`: lowercase letters, digits, `_`, and `-`, at most 63
characters. `wayfindr secret set GitHub.Token` is refused with
`secret name "GitHub.Token" must match [a-z0-9][a-z0-9_-]{0,62}`.

Older releases also allowed capitals and dots. Those names keep working: a
secret already stored under one can still be set, listed, and removed, a config
that refers to one still loads, and you can set a secret your config already
refers to by such a name. Pick a new name by the rule above.

## References

| Reference        | Reads                                    | Name must match              |
| ---------------- | ---------------------------------------- | ---------------------------- |
| `${env:NAME}`    | the daemon's environment variable `NAME` | `[A-Za-z_][A-Za-z0-9_]*`     |
| `${secret:name}` | the secret stored as `name`              | `[A-Za-z0-9][A-Za-z0-9_.-]*` |

A `${secret:…}` reference still accepts the wider rule older releases used, so
existing configs load. See [Secret names](#secret-names).

A reference can sit inside a longer value, and a value can hold more than one:
`"Bearer ${secret:token}"` and `"https://${env:HOST}/mcp"` both work.

References work in a connector's `url`, `headers` values, `args`, and `env`
values, in `auth.client_secret`, `auth.signing_key.key`, `auth.assertion.value`,
and `tls.key`, and in `embedding.api_key`. Anywhere else, `${…}` is plain text.
In those keys, every `${` must start a valid reference, so a shell-style
`${HOME}` is a config error: write `${env:HOME}`. The full rules are in
[Templates](/reference/configuration#templates).

A secret can sit anywhere in a connector's `url`, its path included, as in a
per-user URL such as `https://mcp.example.com/s/${secret:mcp_key}/mcp`. Errors,
logs, and `wayfindr status` show the reference, never its value:
`Post "https://mcp.example.com/s/${secret:mcp_key}/mcp": connection refused`.
The database doesn't hold it either: the URL a token is bound to is stored as a
SHA-256 digest, and Wayfindr doesn't cache OAuth metadata that names the URL.

### When references are resolved

The daemon checks reference syntax when it loads the config but resolves the
values only when it connects to the connector, every time it connects. So a new
secret takes effect without a reload or a restart: connectors waiting on it
reconnect at once, and others pick it up on their next connect. A secret that
`embedding.api_key` refers to applies at once too: the daemon builds its
embedding client again with the new value (see
[Embeddings](./embeddings#when-the-provider-is-down)).

A missing value puts the connector in `auth_error`, and `wayfindr status` names
the reference:

```text
exa: The connector's config refers to secret:exa_api_key, but no such secret is stored.
  Store the secret, or fix the reference in the connector's config.
```

```text
search: The connector's config refers to env:SEARCH_API_KEY, but the daemon's environment does not set it.
  Set the variable in the daemon's environment and restart it, or fix the reference.
```

::: warning
`${env:…}` reads the environment of the daemon, not of your shell or
your client. A daemon started by the shim inherits the environment of the client
that started it. A daemon run as a service by launchd or systemd sees none of
your shell's variables. Changing a variable means restarting the daemon. Prefer
`${secret:…}` for anything the daemon must always find.
:::

### Imported values

[`wayfindr connector import`](./connectors#which-values-become-secrets) stores
the credentials it reads from another client's config as secrets, and turns a
`${VAR}` your shell doesn't set into `${env:VAR}`, to which the warning above
applies.

## `wayfindr secret set`

```sh
wayfindr secret set <name>
```

The command reads the value from stdin and sends it to the running daemon, which
encrypts and stores it. Run it again to replace a value.

| stdin is       | Behavior                                                   |
| -------------- | ---------------------------------------------------------- |
| a terminal     | Prompts `Value for secret <name>:` and reads without echo. |
| a pipe or file | Reads all of it, then drops one trailing newline.          |

```sh
# From a password manager, without the value touching your shell history
op read "op://Private/Exa/api key" | wayfindr secret set exa_api_key
```

It refuses a name that doesn't match the [grammar](#references), an empty value,
and a value over about 64 KiB. The daemon must be running; otherwise the command
fails with:

```text
Error: daemon is not running on 127.0.0.1:7777: start it with `wayfindr start`, or run it as a service with `wayfindr service install`
```

After storing the value, the daemon reconnects the connectors that were waiting
on it and the command reports each one:

```text
Secret exa_api_key set.
exa is ready.
```

If no connector was waiting on it:

```text
Secret exa_api_key set. Connectors that use it pick it up on their next connect.
```

## Listing secrets

```sh
wayfindr secret list
```

```text
NAME          USED BY
exa_api_key   connectors.exa
embed_key     embedding.api_key
old_token     -
```

Each stored secret's name, and what in the daemon's running config refers to
it: a connector (`connectors.<name>`) or `[embedding] api_key`
(`embedding.api_key`). `-` means nothing does, so the secret is safe to delete.
Values are never shown. `--json` prints the same as JSON. The daemon must be
running.

## Deleting a secret

```sh
wayfindr secret remove exa_api_key
```

```text
Deleted secret exa_api_key.
```

Delete only a secret nothing refers to: a connector that still refers to it goes
to `auth_error` the next time it needs the value. The daemon must be running.

You rarely need it. `wayfindr connector remove` deletes the secrets the removed
connector used that nothing else in the config uses, and
`wayfindr connector edit` deletes the ones an edit leaves unused. Both say which
secrets they deleted. See
[Removing a connector](./connectors#removing-a-connector).

No command shows a stored secret's value.

## The master key

Secrets, OAuth tokens, and the clients Wayfindr registers are encrypted in the
store under one master key. `wayfindr init` creates it in the OS keychain when
one is available, and in `<data>/master.key` otherwise. Where the daemon looks
for it, in what order, the mode `master.key` must have (the daemon refuses one
that group or others can read: `chmod 600` it), and how to generate a key for
`WAYFINDR_SECRET_KEY` are in
[Master key](/reference/paths-and-environment#master-key).

::: warning
Back the key up, but not together with the database. If the key is
lost or changed, the daemon can't open the stored secrets and tokens: you would
set every secret and authorize every OAuth connector again. An OAuth connector
then shows
`The stored credential cannot be read, for example because the master key changed.`
`init` refuses to create a new key when the store already holds encrypted data,
so it can't silently replace a lost one.
:::

A daemon run as a service never sees `WAYFINDR_SECRET_KEY`, since Wayfindr
doesn't write secrets into service units. Use the keychain or the key file
there. See [Running the daemon](./running) and [Security](./security).
