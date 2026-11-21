# Coding Standards

These rules apply to all Go code in this repo. `make lint` enforces formatting
and the linters in `.golangci.yml`. Reviewers check the rest by hand.

Architecture rules are in design §2, `docs/internal/design/2-package-map.md`
(package map, one-way dependencies). Use domain terms from `GLOSSARY.md` in
names, log fields, and test names.

## Go style

- Follow Effective Go and the Go Code Review Comments.
- Everything lives under `internal/`. There is no public Go API.
- Write doc comments for exported identifiers. Comment _why_, not _what_.
- Prefer the standard library. Add a dependency only if design §1
  (`docs/internal/design/1-stack.md`) lists it or an ADR justifies it.

## Formatting/whitespace

- Let the code breathe
- New lines before/after `if`, `for`, and other control structures if there are lines above or below them
- Always add a new line before `return` if there if something above it

## Errors

- Wrap with `%w` and add context: `fmt.Errorf("open store %s: %w", path, err)`.
  Inside a package, plain wrapping is enough.
- Handle an error once: either return it or log it, never both.
- Don't panic on recoverable conditions.

### Seam errors: Kind, Case, Advice

Every package seam that callers branch on (for example `upstream`, `auth`,
`gateway`) follows one convention. `auth` was the first to adopt it; other
packages migrate when they are next touched, not in one sweep.

- **One error type.** The package's seam errors are `*Error{Kind, Case, …}`
  (plus whatever fields the domain needs, such as a provider code, a link,
  and the cause `Err`). Functions return them as `error`; never declare a
  `*Error` return type, which turns a nil `*Error` into a non-nil `error`.
  `AsError(err error) (*Error, bool)` narrows with `errors.As`.
- **Closed vocabularies.** `Kind` (the coarse class) and `Case` (the finer
  reason) are named string types with closed `const` sets, owned by the
  package (GLOSSARY: Kind, Case). Callers match on `Kind` and `Case`, never
  on error text.
- **Exhaustive switches.** The `exhaustive` linter is on, with
  `default-signifies-exhaustive: false`: a `switch` on a `Kind`, a `Case`,
  or any other enum-like type must list every value, and a `default:`
  clause does not count. A switch meant to be partial says so with
  `//exhaustive:ignore` and a reason.
- **Advice next to the error.** Each domain has a co-located
  `Advise(*Error) Advice`, where `Advice` is `{Summary, Action, Link string}`
  (GLOSSARY: Advice). The domain decides what its error means and how to fix
  it; presenters such as the gateway only render the advice.
- **Text is technical.** `Error()` is for logs and status: technical, with
  URLs redacted (`internal/redact`). User-facing wording comes from `Advise`.
- **Translate at each seam.** A layer turns the errors of the layer below
  into its own `Kind`/`Case` vocabulary at its seam (an anti-corruption
  layer) instead of passing them through. Packages that may not import each
  other (siblings in design §2, such as `upstream` and `auth`) match by
  behaviour through small local interfaces (for example `AuthCase() string`),
  which `errors.As` finds through wrapping such as net/http's `*url.Error`.
- Sentinel errors (`var ErrX = errors.New(…)`) remain fine inside a package
  and in packages not yet migrated.

## Context

- `ctx context.Context` is the first parameter of every function that does
  network I/O, blocks, or calls something that does.
- A small helper that does only local file I/O through the standard library,
  which cannot honour a `ctx`, may omit it (for example a function that
  creates one file with `os.OpenFile`). Its callers still take `ctx`.
- Never store a `ctx` in a struct.

## State

- No package-level mutable state. Dependencies go in through constructors.
  Read-only tables are functions or constants, not `var`s.
- These package-level `var`s are fine, because nothing assigns to them:
  sentinel errors (`var ErrNotFound = errors.New(…)`), `//go:embed` file
  systems, and compile-time interface checks (`var _ I = (*T)(nil)`).
- There are two exceptions, and each needs a comment:
  - build metadata set with `-ldflags -X`, such as `version` and `commit`
    in `internal/cli`;
  - library switches that can only be set globally (for example
    `cobra.EnableTraverseRunHooks`). Set them once, in `init()`.

## Logging

- Use zerolog only. `internal/cli` creates the logger (JSON on stderr, level
  from `--log-level`) and puts it in the command context. Code below the CLI
  receives a `zerolog.Logger` through its constructor. It never uses the
  global `log` package.
- Field names are `snake_case` and use glossary nouns: `connector`, `tool`,
  `client`, `user_id`, `protocol_version`, `state`, `duration_ms`. Log errors
  with `.Err(err)`.
- Messages are short, lowercase, static strings. Put variable data in fields,
  not in `Msgf`.
- **Redaction (design §9).** Never log header values, tokens, authorization
  codes, secret values, or URL query strings. Log URLs without the query, and
  log header names only.

## Tests

- Use the stdlib `testing` package, with `github.com/google/go-cmp/cmp` for
  structural comparison. No testify.
- Test at agreed seams (the package's public interface), never internals.
  Expected values come from the spec or from literals, never from recomputing
  them the way the code does.
- Exception: an unexported pure function that encodes a decision table from
  the design (input in, decision out, no state or I/O) is its own seam. Test
  it directly with a table when that table is the clearest readable spec of
  the rules, for example `foldPlural` and `tokenize` in
  `internal/store/sqlite` (design §7.5). Say so in a comment on the test, and
  still cover the rules' effect at the package's public interface.
- Use table-driven tests with `t.Run` when cases share a shape.
- Use the fakes in `internal/testutil` (`fakeupstream`, `fakeas`) rather than
  hand-rolled mocks of internal collaborators.
- Tests must pass under `make race`.

## Commits

- One kata issue per commit:
  `<type>(<pkg>): <summary> (wayfindr-xxx)`, where type is `feat`, `fix`,
  `perf`, `refactor`, `test`, `docs`, `chore`, or `build`, and `xxx` is the
  issue's short ref (`kata show xxx`).
