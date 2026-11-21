# VitePress documentation site

The public docs for Wayfindr, built with VitePress 2 (alpha) and published to
GitHub Pages at <https://atomicpages.github.io/wayfindr/>.
`vitepress-plugin-llms` also writes `llms.txt` and `llms-full.txt`.

## Structure

```
docs/
  .vitepress/
    config.mts       Site config: nav, sidebars, search, llms.txt plugin
    theme/index.ts   Default theme + the copy/download-as-Markdown buttons
    theme/HomeWordmark.vue  Theme-aware logo above the home-page headline
    theme/style.css  Navigation logo sizing
  index.md           Home page (hero layout)
  guide/             Task guides: getting started, connecting, search, operations
  reference/         CLI, configuration, meta-tools, paths, HTTP, compatibility
  architecture/      How Wayfindr works, for users and contributors
  adr/               Architecture decision records (published as-is)
  public/            Served as-is at the site root: install.sh and install.ps1,
                     the one-line installers, plus logo.svg, logo-dark.svg,
                     and the adaptive favicon.svg. README.md also uses these
                     logo files. They go live on merge to main.
  internal/          Engineering docs: design.md (index) and design/,
                     compat.md, releasing.md, observability.md, plans/
                     (gitignored, local only), agents/. NOT published
  CONTEXT.md         This file. NOT published
```

`internal/**` and `CONTEXT.md` are in both `srcExclude` and the llms plugin's
`ignoreFiles`. Keep the two lists the same (`excluded` in `config.mts`).
Published pages must not link into `internal/`. Link to the file on GitHub
instead.

## Commands

- `bun run docs:dev`: dev server with hot reload
- `bun run docs:build`: build to `docs/.vitepress/dist/`. Fails on dead links
- `bun run docs:preview`: serve the built site

`.github/workflows/docs.yml` builds on every PR to `main` and deploys on push to
`main`.

## Content rules

- Write for people who run Wayfindr, not for people building it. Use second
  person, short sentences, and plain words, matching the README.
- Use the terms in `GLOSSARY.md` exactly: connector, client, upstream, catalog,
  auth link, connector state, and so on.
- Lead with a working example. Use tables for flags, keys, states, and errors.
  Use `::: tip` and `::: warning` sparingly.
- Write every admonition (`::: tip`, `::: warning`, `::: info`, `::: danger`,
  `::: details`, and any other `:::` container) in block form. The opener is
  `::: <type>` alone on its line, with the body on the lines after it and the
  closing `:::` alone on its own line:

  ```md
  ::: warning
  Body text starts on the next line.
  :::
  ```

  Never put text on the opener line (`::: warning Text...`) or before the
  closing `:::` (`text. :::`). Before you finish a page, check it with:
  `grep -rnE '^::: *(warning|tip|info|danger|details|note)[ ].+|[^ ]:::\s*$' docs --exclude-dir=internal --exclude-dir=.vitepress`.
  Don't edit `docs/.vitepress/dist/`; it is build output.
- Every command, flag, key, default, message, and state must match the code or
  the built binary (`bin/wayfindr <cmd> --help`). `internal/design/` can be out
  of date. When they disagree, the code wins, and the design doc gets a fix too.
- Don't name internal Go packages outside `architecture/` and `adr/`.
- Link between pages with relative paths and no `.md` extension
  (`cleanUrls: true`).
- ADRs are history. Don't rewrite a record's decision. When it changes, add a
  new record or amend the old one:
  - In the record's text, where the decision changed, add "(amended YYYY-MM-DD,
    wayfindr-xxxx)" and say what changed. Use the date the change shipped and
    the issue in that commit's subject. A record that was wrong from the start
    gets "(corrected YYYY-MM-DD, wayfindr-xxxx)", dated when it was corrected.
    An amendment from before any code shipped has only its date.
  - The Status line lists every amendment and correction, oldest first:
    `Status: Accepted (YYYY-MM-DD). Amended:` and then one item per change,
    `- YYYY-MM-DD (wayfindr-xxxx): what changed`, linking the record that made
    it, if any.
  - A record that changes an older one says so on its Status line ("Amends the
    client-auth decision of [0003](./0003-single-daemon)"), and the older record
    lists it as an amendment.
  - A record written down after its decision shipped is accepted on the date it
    shipped, and its Status line names the commits.
