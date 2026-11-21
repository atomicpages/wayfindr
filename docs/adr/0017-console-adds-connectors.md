# 0017 — The daemon adds connectors for the web console

Status: Accepted (2026-11-12). Amends the config writers of
[0006](./0006-toml-config) and the "never edits the config" rule of
[0016](./0016-local-web-console).

## Context

The web console ([0016](./0016-local-web-console)) shows every connector and can
sign one in, but adding one still means switching to a terminal for
`wayfindr connector add`. Adding a connector is the first thing a new user does,
and the step where a form helps most: which sign-in a server needs, which values
are secrets, how a command's arguments split.

Until now only two things wrote `wayfindr.toml`: the user, and the CLI, which
reads the file, appends a table with the config editor, and writes it back
atomically, refusing if the file changed meanwhile ([0006](./0006-toml-config)).
The console reads everything through the admin API and can't write a file the
CLI found on its own.

A command (stdio) connector runs a program on the user's machine each time
Wayfindr connects. [0010](./0010-project-scopes) keeps project files from
defining connectors for exactly that reason. A browser form that can add one
turns any hole in the console's defenses from "can call tools" into "can run
code".

## Decision

- The admin API gets `POST /admin/connectors`, and the daemon becomes a writer
  of its own config file. It does what `connector add` does, with the same
  building blocks: check the new table against the file first, ask a URL
  connector's server whether it uses OAuth, store secret values sealed and write
  `${secret:…}` references to them, then replace the file atomically, refusing
  if it changed since it was read. It writes the file it loaded, not a path a
  client works out, and reloads it at once rather than waiting for the file
  watcher. Everything else in the file stays byte for byte, as with the CLI.
- The console has an **Add connector** form for both kinds. A URL connector is
  added when the form is sent.
- A command connector is not. The console first shows a review page with the
  exact command, each argument, and the environment (secret values masked).
  Nothing is written or run until the user selects **Add and run**, which posts
  a token that is single-use, valid for 5 minutes, and bound to that browser
  session, on top of the session cookie, the form token, and the same-origin
  check every console form has. The values entered wait in the daemon's memory,
  never in the page.
- The console still never edits or removes a connector, and never imports one
  from another client: those stay CLI commands.

## Consequences

- A new user can set up a connector without the CLI, and sees how it came out
  (ready, needs a sign-in, or why not) on the same screen.
- Three writers share the config file: the user, the CLI, and the daemon. Each
  write is a check-and-replace, so a write that would lose another's edit is
  refused instead ("the config file changed"), and the user tries again. The
  check and the rename are not one step, so two writes landing in the same
  instant can still lose one; the window is as small as the file system allows.
- Adding a command connector from the browser takes two deliberate steps. A page
  that could forge one form post still couldn't add a command, since the confirm
  token appears only in the review page, which another origin can't read.
- The console's sign-in choices are the CLI's: detect OAuth, OAuth, an API key
  in a header, or none. Anything else, such as `authorize_params` or a tools
  list, is set with `wayfindr connector edit` afterwards.
