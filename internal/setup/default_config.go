package setup

import "strings"

// DefaultConfig is the wayfindr.toml Init writes when there is none. It sets
// only what design §4 defaults anyway, so it parses to the same Config as an
// empty file; storage.dsn is left out so it follows the data directory.
const DefaultConfig = `# Wayfindr configuration. Reference: https://atomicpages.github.io/wayfindr/reference/configuration
# The daemon reloads this file on change. Unknown keys are errors.

[server]
listen = "127.0.0.1:7777"   # loopback only

[storage]
driver = "sqlite"           # dsn defaults to <data>/wayfindr.db

[index]
refresh_interval = "1h"     # how often HTTP connectors are re-polled

# Optional: an OpenAI-compatible embeddings endpoint for semantic search.
# Without it, search is fuzzy-only.
#
# [embedding]
# base_url   = "http://127.0.0.1:11434/v1"
# model      = "qwen3-embedding:0.6b"
# dimensions = 1024

# Optional: send the maintainers an anonymous daily usage report: counts and
# versions, never connector or tool names. Off by default; DO_NOT_TRACK=1
# keeps it off. What it holds: https://atomicpages.github.io/wayfindr/reference/telemetry
#
# [telemetry]
# enabled = true

# Add connectors with ` + "`wayfindr connector add`" + `, or one table each by hand,
# then run ` + "`wayfindr auth login <name>`" + ` for OAuth:
#
# [connectors.linear]
# url  = "https://mcp.linear.app/mcp"
# auth = { type = "oauth" }
#
# [connectors.filesystem]
# command = "npx"
# args    = ["-y", "@modelcontextprotocol/server-filesystem", "/path/to/dir"]
`

// telemetryOff and telemetryOn are the [telemetry] lines of DefaultConfig as
// shipped (commented out) and as written when the user opts in.
const (
	telemetryOff = "# [telemetry]\n# enabled = true\n"
	telemetryOn  = "[telemetry]\nenabled = true\n"
)

// defaultConfigWithTelemetry is DefaultConfig with the usage report on.
func defaultConfigWithTelemetry() string {
	return strings.Replace(DefaultConfig, telemetryOff, telemetryOn, 1)
}
