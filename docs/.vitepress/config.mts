import { defineConfig } from "vitepress";
import llmstxt, {
  copyOrDownloadAsMarkdownButtons,
} from "vitepress-plugin-llms";

// Kept out of the site and out of llms.txt: engineering docs and this
// folder's own notes for contributors.
const excluded = ["internal/**", "CONTEXT.md"];

export default defineConfig({
  lang: "en-US",
  title: "Wayfindr",
  description:
    "An MCP gateway: every MCP server behind one daemon and two tools.",
  base: "/wayfindr/",
  head: [
    [
      "link",
      { rel: "icon", type: "image/svg+xml", href: "/wayfindr/favicon.svg" },
    ],
  ],
  srcExclude: excluded,
  cleanUrls: true,
  lastUpdated: true,
  vite: {
    plugins: [
      llmstxt({
        domain: "https://atomicpages.github.io",
        ignoreFiles: excluded,
      }),
    ],
  },
  markdown: {
    config(md) {
      md.use(copyOrDownloadAsMarkdownButtons);
    },
  },

  themeConfig: {
    logo: {
      light: "/logo.svg",
      dark: "/logo-dark.svg",
      alt: "Wayfindr",
    },
    siteTitle: false,
    nav: [
      { text: "Guide", link: "/guide/introduction" },
      { text: "Reference", link: "/reference/cli" },
      { text: "Architecture", link: "/architecture/how-it-works" },
    ],

    sidebar: {
      "/guide/": [
        {
          text: "Getting Started",
          items: [
            { text: "Introduction", link: "/guide/introduction" },
            { text: "Installation", link: "/guide/installation" },
            { text: "Quick Start", link: "/guide/quick-start" },
          ],
        },
        {
          text: "Connect Clients",
          items: [
            { text: "Clients", link: "/guide/clients" },
            { text: "Cloud Agents", link: "/guide/cloud-agents" },
          ],
        },
        {
          text: "Connectors",
          items: [
            {
              text: "Adding and Fixing Connectors",
              link: "/guide/connectors",
            },
            { text: "Authentication", link: "/guide/authentication" },
            {
              text: "Enterprise Authentication",
              link: "/guide/authentication-enterprise",
            },
            {
              text: "Unattended Grants",
              link: "/guide/authentication-unattended",
            },
            { text: "Secrets", link: "/guide/secrets" },
          ],
        },
        {
          text: "Control Access",
          items: [
            { text: "Overview", link: "/guide/access/" },
            { text: "Tools Lists", link: "/guide/access/tools-lists" },
            { text: "Project Scopes", link: "/guide/access/project-scopes" },
            { text: "Profiles", link: "/guide/access/profiles" },
            {
              text: "Scoped Client Tokens",
              link: "/guide/access/scoped-tokens",
            },
          ],
        },
        {
          text: "Search",
          items: [
            { text: "How Search Works", link: "/guide/search" },
            { text: "Embeddings", link: "/guide/embeddings" },
          ],
        },
        {
          text: "Operations",
          items: [
            { text: "Running the Daemon", link: "/guide/running" },
            { text: "Monitoring", link: "/guide/monitoring" },
            { text: "Web Console", link: "/guide/console" },
            { text: "Storage", link: "/guide/storage" },
            { text: "Security", link: "/guide/security" },
            { text: "Troubleshooting", link: "/guide/troubleshooting" },
            {
              text: "Troubleshooting OAuth and TLS",
              link: "/guide/troubleshooting-oauth",
            },
          ],
        },
      ],
      "/reference/": [
        {
          text: "Reference",
          items: [
            { text: "CLI", link: "/reference/cli" },
            { text: "Configuration", link: "/reference/configuration" },
            { text: "Meta-Tools", link: "/reference/meta-tools" },
            {
              text: "Paths and Environment",
              link: "/reference/paths-and-environment",
            },
            { text: "HTTP Endpoints", link: "/reference/http" },
            { text: "Compatibility", link: "/reference/compatibility" },
            { text: "Telemetry", link: "/reference/telemetry" },
          ],
        },
      ],
      "/architecture/": architectureSidebar(),
      "/adr/": architectureSidebar(),
    },

    socialLinks: [
      { icon: "github", link: "https://github.com/atomicpages/wayfindr" },
    ],

    editLink: {
      pattern: "https://github.com/atomicpages/wayfindr/edit/main/docs/:path",
      text: "Edit this page on GitHub",
    },

    search: {
      provider: "local",
    },

    footer: {
      message: "Released under the Apache 2.0 License.",
      copyright: "Copyright © 2026 Dennis Thompson",
    },
  },
});

function architectureSidebar() {
  return [
    {
      text: "Architecture",
      items: [{ text: "How It Works", link: "/architecture/how-it-works" }],
    },
    {
      text: "Decisions",
      items: [
        { text: "Overview", link: "/adr/" },
        { text: "0001 Two meta-tools", link: "/adr/0001-two-meta-tools" },
        { text: "0002 Go single binary", link: "/adr/0002-go-single-binary" },
        { text: "0003 Single daemon", link: "/adr/0003-single-daemon" },
        {
          text: "0004 Lazy single-flight refresh",
          link: "/adr/0004-lazy-single-flight-refresh",
        },
        {
          text: "0005 Store owns hybrid search",
          link: "/adr/0005-store-owns-hybrid-search",
        },
        { text: "0006 TOML config and secrets", link: "/adr/0006-toml-config" },
        {
          text: "0007 OAuth discovery and registration",
          link: "/adr/0007-oauth-discovery-and-registration",
        },
        { text: "0008 Relay elicitation", link: "/adr/0008-relay-elicitation" },
        {
          text: "0009 Tool policy in the gateway",
          link: "/adr/0009-tool-policy-in-the-gateway",
        },
        { text: "0010 Project scopes", link: "/adr/0010-project-scopes" },
        { text: "0011 Named profiles", link: "/adr/0011-named-profiles" },
        {
          text: "0012 Scoped client tokens",
          link: "/adr/0012-scoped-client-tokens",
        },
        {
          text: "0013 Stateless MCP server",
          link: "/adr/0013-stateless-mcp-server",
        },
        { text: "0014 Call log", link: "/adr/0014-call-log" },
        {
          text: "0015 Credential health warnings",
          link: "/adr/0015-credential-health-warnings",
        },
        { text: "0016 Local web console", link: "/adr/0016-local-web-console" },
        {
          text: "0017 Console adds connectors",
          link: "/adr/0017-console-adds-connectors",
        },
        {
          text: "0018 Grants, client auth, sender constraints",
          link: "/adr/0018-grants-client-authentication-sender-constraints",
        },
        {
          text: "0019 Connector logos from theSVG",
          link: "/adr/0019-connector-logos-from-thesvg",
        },
        {
          text: "0020 Opt-in usage telemetry",
          link: "/adr/0020-opt-in-usage-telemetry",
        },
      ],
    },
  ];
}
