# Storage

Wayfindr keeps its state in one store. SQLite is the default and needs no setup.
Postgres with pgvector is the other choice.

```toml
# The default; you can leave the table out.
[storage]
driver = "sqlite"
# dsn defaults to wayfindr.db in the data directory.
```

```toml
# Postgres
[storage]
driver = "postgres"
dsn    = "postgres://wayfindr:password@localhost:5432/wayfindr"
```

Changing either key takes a restart. The daemon logs
`config change takes effect on restart` and keeps using the store it opened.

## What's in the store

| Data                 | Stored as                                                                                                                      |
| -------------------- | ------------------------------------------------------------------------------------------------------------------------------ |
| The catalog          | Each tool's definition, a hash of its text, and its embedding if any.                                                          |
| Sync records         | Each connector's last sync time and error.                                                                                     |
| OAuth credentials    | Tokens and any client Wayfindr registered. **Encrypted.**                                                                      |
| Secrets              | Values you set with `wayfindr secret set`. **Encrypted.**                                                                      |
| OAuth metadata       | Cached discovery documents from authorization servers.                                                                         |
| The call log         | One row per `call_tool` call. See [Monitoring](./monitoring#what-did-agents-call).                                             |
| Scoped client tokens | Each token's name, connectors, and SHA-256 digest, never the token itself. See [Scoped client tokens](./access/scoped-tokens). |

Credentials and secrets are encrypted with AES-256-GCM under the master key, and
each value is bound to its own row, so a value copied into another row won't
decrypt. The master key is never in the store; see
[Master key](/reference/paths-and-environment#master-key) for where it lives and
[Security](./security) for what it protects.

Not in the store: the config file, the default client token, and the master key.

Only the daemon writes the store, and only one daemon runs per data directory.
See [Running the daemon](./running).

## SQLite

SQLite is built into the binary; there is nothing to install. The database is
`wayfindr.db` in the data directory, `~/.local/share/wayfindr/` by default. Set
`dsn` to another path to move it. A leading `~` expands to your home directory.

Search runs in memory. The daemon loads the catalog when it opens the store and
keeps the in-memory copy current on every write. A catalog of a few thousand
tools takes well under a millisecond to search.

`wayfindr init` creates the database. The daemon runs any migrations a new
version needs when it starts.

## Postgres

Use Postgres when you want the store on a database server you already run and
back up.

The database needs:

- **pgvector.** Wayfindr creates the `vector` and `pg_trgm` extensions on its
  first start. Its role needs permission to create them, or you create them once
  as a superuser:

  ```sql
  CREATE EXTENSION IF NOT EXISTS vector;
  CREATE EXTENSION IF NOT EXISTS pg_trgm;
  ```

- **UTF8 encoding.** Create the database with
  `ENCODING 'UTF8' TEMPLATE template0` if your server's default isn't UTF8. Any
  locale works, `C` included.
- **ICU.** The server must be built with ICU support, as the official
  `pgvector/pgvector` images and most distributions are.
- **A role that can create tables** in the database. Wayfindr runs its
  migrations itself.

A setup that meets all of it:

```sql
CREATE ROLE wayfindr LOGIN PASSWORD 'change-me';
CREATE DATABASE wayfindr OWNER wayfindr ENCODING 'UTF8' TEMPLATE template0;
\c wayfindr
CREATE EXTENSION IF NOT EXISTS vector;
CREATE EXTENSION IF NOT EXISTS pg_trgm;
```

`dsn` is a `postgres://` URL or a keyword/value string
(`host=localhost dbname=wayfindr user=wayfindr`). Error messages never include
it, since it may hold a password. Keep the password out of the file with the
standard `PGPASSWORD` variable or a `~/.pgpass` file, which the Postgres driver
reads.

If the database is not usable, the daemon stops at startup and says why:

```text
Error: open store: open postgres store: unsupported postgres database: encoding is SQL_ASCII, want UTF8; create the database with ENCODING 'UTF8' TEMPLATE template0
```

Search runs in SQL, with the same ranking as SQLite. Embeddings are stored in
pgvector columns without a fixed dimension, so you can change the model without
a migration.

To start on Postgres, write `[storage]` in `~/.config/wayfindr/wayfindr.toml`
(create the directory if it's missing) before you run `wayfindr init`:

```toml
[storage]
driver = "postgres"
dsn    = "postgres://wayfindr:password@localhost:5432/wayfindr"
```

`init` keeps an existing config, opens the store, and runs the migrations. If the
config already exists, edit its `[storage]` table instead and restart the daemon.

## Switching stores

There is no tool to copy data between SQLite and Postgres. When you switch
drivers and restart, the daemon opens an empty store:

- The catalog fills again on its own as each connector syncs.
- OAuth connectors go to `needs_auth`. Run `wayfindr auth login <connector>` for each.
- Secrets must be set again with `wayfindr secret set <name>`. Until then,
  connectors that use them show `auth_error`.

The old store is left as it was, so you can switch back.

## Upgrades and downgrades

Migrations only go forward. A newer binary migrates the store when it starts. If
you then run an older binary against that store, it refuses to start rather than
misread it:

```text
Error: open store: open store /Users/you/.local/share/wayfindr/wayfindr.db: database schema version 4, binary supports up to 3: database schema is newer than this binary
```

Upgrade the binary again to fix it.

See the [configuration reference](/reference/configuration) for both keys.
