---
layout: home

hero:
  text: One door to every tool
  tagline:
    An MCP gateway that holds the credentials for all your MCP servers and gives
    your agent two tools instead of hundreds.
  actions:
    - theme: brand
      text: Install
      link: /guide/installation
    - theme: alt
      text: Quick Start
      link: /guide/quick-start
    - theme: alt
      text: View on GitHub
      link: https://github.com/atomicpages/wayfindr

features:
  - title: Two tools, not hundreds
    details:
      Your agent sees search_tools and call_tool. It finds the tool it needs
      when it needs it, so connecting fifty servers doesn't fill its context
      with tool definitions.
    link: /guide/search
    linkText: How search works
  - title: Credentials in one place
    details: OAuth with discovery and Dynamic Client Registration, your own
      pre-registered app, or CIMD. API keys in headers. Tokens refresh on demand
      and never reach your agent.
    link: /guide/authentication
    linkText: Authentication
  - title: Every kind of MCP server
    details:
      Connect stdio servers, Streamable HTTP, and legacy HTTP+SSE, old and new
      protocol versions alike. Questions a tool asks you mid-call reach your
      client.
    link: /guide/connectors
    linkText: Connectors
  - title: Import your existing servers
    details:
      Already set up MCP servers in Claude Code, Cursor, Codex, or VS Code? One
      command moves them into Wayfindr, with their keys stored as secrets.
    link: /guide/connectors#importing-from-a-client
    linkText: Importing from a client
  - title: Control what agents reach
    details:
      Hide tools with a tools list, narrow a project with a scope file or a
      profile, and hold an unattended agent to some connectors with a scoped
      client token.
    link: /guide/access/
    linkText: Control access
  - title: See every call
    details:
      The call log shows which client called which tool, how long it took, and
      how it ended. Never the arguments or results.
    link: /guide/monitoring#what-did-agents-call
    linkText: Monitoring
  - title: Hybrid search
    details:
      Fuzzy and full-text matching out of the box. Add any OpenAI-compatible
      embeddings endpoint, such as a local Ollama, for semantic search.
    link: /guide/embeddings
    linkText: Embeddings
  - title: Works with your harness
    details:
      Claude Code, Codex, Cursor, and any other MCP client, over a stdio shim
      that starts the daemon for you or over local HTTP.
    link: /guide/clients
    linkText: Clients
  - title: One binary, one file
    details:
      A single static Go binary and one TOML config that reloads when you save
      it. SQLite by default, Postgres with pgvector if you want it.
    link: /reference/configuration
    linkText: Configuration
---

## Install

```sh
curl -fsSL https://atomicpages.github.io/wayfindr/install.sh | sh
```

On Windows, use PowerShell and the [other installer](/guide/installation). Then
follow the [quick start](/guide/quick-start).
