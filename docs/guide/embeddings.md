# Embeddings

Embeddings add semantic matching to [search](./search), so
`send a message to the team` can find a tool called `chat_post_message`. They
are optional. Without them, search is lexical: fuzzy and full-text matching
only.

To turn them on with a local [Ollama](https://ollama.com), pull a model:

```bash
ollama pull qwen3-embedding:0.6b
```

Then add an `[embedding]` table to `wayfindr.toml`:

```toml
[embedding]
base_url   = "http://localhost:11434/v1"
model      = "qwen3-embedding:0.6b"
dimensions = 1024
```

1024 is the model's full size. Ollama also honours a smaller `dimensions`, such
as 512, if you want to store less.

Save the file. You don't need to restart: the daemon reloads the config, embeds
the whole catalog in the background, and uses the embeddings in every search
from then on.

## Is it on?

Run a search with `--explain` and read its first line:

```sh
wayfindr search open a ticket --explain
```

```text
Embeddings: on (qwen3-embedding:0.6b)
```

| Header                     | Meaning                                                                                                              |
| -------------------------- | -------------------------------------------------------------------------------------------------------------------- |
| `on (<model>)`             | This search embedded the query, so the ranking is hybrid.                                                            |
| `off, lexical only`        | No `[embedding]` table is set.                                                                                       |
| `failed (…), lexical only` | The provider failed just now; the search ran lexically. See [When the provider is down](#when-the-provider-is-down). |

While the daemon is still embedding the catalog, tools it hasn't reached show `-`
under VECTOR. [Monitoring](./monitoring#why-a-tool-ranks-where-it-does)
explains the columns.

## Providers

Wayfindr works with any OpenAI-compatible embeddings endpoint. It sends
`POST <base_url>/embeddings` with `model`, `input`, and `dimensions`, so
`base_url` ends at `/v1`, not at `/embeddings`.

### OpenAI

```toml
[embedding]
base_url   = "https://api.openai.com/v1"
model      = "text-embedding-3-small"
dimensions = 1536
api_key    = "${secret:openai_api_key}"
```

Store the key once:

```bash
wayfindr secret set openai_api_key
```

OpenAI's `text-embedding-3-*` models can return shorter vectors. Set
`dimensions = 512`, for example, to store less and search a little faster.
`text-embedding-3-large` returns up to 3072.

### Lemonade

[Lemonade](https://lemonade-server.ai/docs/) runs local models with an
OpenAI-compatible API on port `13305`. After you install it, the server runs in
the background. Pull an embedding model:

```bash
lemonade pull Qwen3-Embedding-0.6B-GGUF
```

```toml
[embedding]
base_url   = "http://localhost:13305/v1"
model      = "Qwen3-Embedding-0.6B-GGUF"
dimensions = 1024
```

Lemonade ignores `dimensions` and returns the model's native size, so set it to
that size. `lemonade status` checks that the server is up. If you set
`LEMONADE_API_KEY` for the server, set `api_key` to the same value.

### Other endpoints

Any server that speaks the same API works: LM Studio, vLLM, LiteLLM, Text
Embeddings Inference, or a hosted provider with an OpenAI-compatible endpoint.
Set `base_url`, `model`, and the vector length the model returns.

## Keys

| Key          | Required | What it does                                                                       |
| ------------ | -------- | ---------------------------------------------------------------------------------- |
| `base_url`   | yes      | The provider's base URL, ending at `/v1`.                                          |
| `model`      | yes      | The model name, as the provider spells it.                                         |
| `dimensions` | yes      | The vector length. Sent with each request, and every vector must have this length. |
| `api_key`    | no       | Sent as `Authorization: Bearer <key>`. Use `${secret:…}` or `${env:…}`.            |
| `timeout`    | no       | How long a search waits for the query's embedding. Default `2s`.                   |

See the [configuration reference](/reference/configuration) for the rest of the
file.

`dimensions` must match what the provider returns. Some providers ignore the
requested length and return the model's native size. If the lengths differ, the
embedding pass fails and the daemon log says so. Set `dimensions` to the model's
native size to fix it.

`timeout` applies only to the query's embedding, the one on the search path.
Embedding the catalog runs in the background, in batches of 64 entries, and
each batch has one minute. A provider that doesn't answer in time fails that
pass, as any provider error does: the entries stay searchable by keyword and are
embedded on a later pass. A config reload that changes `[embedding]` starts one.

## What gets embedded

Each catalog entry is embedded from its qualified name, title, description, and
its top-level parameters' names and descriptions. Wayfindr keeps a hash of that
text. An entry is embedded again only when the text changes, so a sync that
changes nothing costs no provider calls.

Entries are sent in batches of 64. A new connector's tools are embedded right
after its first sync.

Query embeddings are cached, up to 256 recent queries, so an agent that repeats
a search doesn't call the provider again.

## Changing the model

Wayfindr tags every stored vector with the model that made it: the `base_url`,
`model`, and `dimensions` together. Change any of the three and save, and the
daemon embeds the whole catalog again under the new model.

Search keeps working while that runs. A query is only compared with vectors from
the same model, so entries not yet re-embedded are found lexically until their
turn comes.

Removing the `[embedding]` table turns semantic search off at once. The stored
vectors stay, so adding the same table back only embeds the tools that changed
in the meantime.

## When the provider is down

Search never fails because of the provider.

- **During a search.** If the query's embedding fails or takes longer than
  `timeout`, that search runs lexically and returns normally. The next search
  tries the provider again.
- **During the embedding pass.** The pass stops at the first failed batch. The
  entries it didn't reach stay searchable lexically. The next pass picks them
  up: after the next successful sync of any connector, after a restart, after
  you change `[embedding]`, or after you set the `api_key` secret.
- **At startup or reload.** If `api_key` refers to a secret or environment
  variable that isn't set, the daemon logs it and searches lexically. Store the
  secret with `wayfindr secret set` and the daemon picks it up at once, with no
  restart: it builds the embedding client again and embeds the catalog. Setting
  the secret again replaces the key the same way. An `${env:…}` variable needs a
  restart, as it does for connectors.

These show up in the daemon log, not in `wayfindr status`:

| Log message                                                     | Meaning                                                                            |
| --------------------------------------------------------------- | ---------------------------------------------------------------------------------- |
| `catalog embedding pass failed`                                 | A batch failed. The error names the cause.                                         |
| `embedding api_key unresolved; search is lexical only`          | `api_key` refers to a value that isn't set.                                        |
| `embedding api_key secret set; rebuilding the embedding client` | `wayfindr secret set` stored the secret `api_key` refers to. Logged at info level. |
| `query embedding failed, searching lexically`                   | One query fell back. Logged at debug level only.                                   |

To see the debug messages, run the daemon with
`wayfindr start --foreground --log-level debug`. [Running the daemon](./running)
says where the log goes.

::: tip
A local model keeps tool names and descriptions on your machine. With a
hosted provider, Wayfindr sends it the text of every tool in your catalog and
every search query.
:::
