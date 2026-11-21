package telemetry

import (
	"net"
	"net/url"
	"strings"
)

// other is the name a report gives anything not on an allowlist, so a name
// the user chose, or a private host or model, never leaves the machine.
const other = "other"

// clientName maps the clientInfo name a client asserts to an allowlisted
// key, or "other". A name matches a key written with hyphens, exactly or as
// its prefix: "cursor-vscode" is cursor, "codex-mcp-client" is codex.
func clientName(raw string) string {
	n := strings.ToLower(strings.TrimSpace(raw))
	n = strings.NewReplacer("_", "-", " ", "-").Replace(n)

	for _, c := range [...]struct{ prefix, key string }{
		{"claude-code", "claude_code"},
		{"cursor", "cursor"},
		{"codex", "codex"},
		{"opencode", "opencode"},
		{"omp", "omp"},
		{"hermes", "hermes"},
	} {
		if n == c.prefix || strings.HasPrefix(n, c.prefix+"-") {
			return c.key
		}
	}

	return other
}

// embeddingProvider names the provider of an [embedding] base_url: a
// public provider by its host, "local" for a loopback or private address,
// and "other" for any other host, which may be the user's own.
func embeddingProvider(baseURL string) string {
	u, err := url.Parse(baseURL)

	if err != nil {
		return other
	}

	host := strings.ToLower(u.Hostname())

	switch host {
	case "api.openai.com":
		return "openai"
	case "api.voyageai.com":
		return "voyage"
	case "api.mistral.ai":
		return "mistral"
	case "openrouter.ai":
		return "openrouter"
	case "api.together.xyz":
		return "together"
	case "localhost":
		return "local"
	}

	if strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") {
		return "local"
	}

	if ip := net.ParseIP(host); ip != nil && (ip.IsLoopback() || ip.IsPrivate()) {
		return "local"
	}

	return other
}

// embeddingModel names an [embedding] model if it is a public model on the
// allowlist, and "other" otherwise, so a private fine-tune's name is never
// sent. An organization prefix ("BAAI/") and an Ollama tag (":latest") are
// dropped before the match.
func embeddingModel(raw string) string {
	m := strings.ToLower(strings.TrimSpace(raw))

	if i := strings.LastIndexByte(m, '/'); i >= 0 {
		m = m[i+1:]
	}

	if i := strings.IndexByte(m, ':'); i >= 0 {
		m = m[:i]
	}

	switch m {
	case "text-embedding-3-small", "text-embedding-3-large", "text-embedding-ada-002",
		"voyage-3", "voyage-3-large", "voyage-3-lite", "voyage-3.5", "voyage-3.5-lite", "voyage-code-3",
		"mistral-embed",
		"nomic-embed-text", "mxbai-embed-large", "all-minilm", "snowflake-arctic-embed",
		"bge-m3", "bge-large-en-v1.5", "bge-small-en-v1.5",
		"embeddinggemma", "qwen3-embedding":
		return m
	}

	return other
}
