package embed_test

import (
	"testing"

	"github.com/atomicpages/wayfindr/internal/embed"
)

// The expected hashes were computed outside Go, e.g.
//
//	printf '%s' '25:https://api.openai.com/v1,22:text-embedding-3-small,4:1536,' | shasum -a 256
func TestModelID(t *testing.T) {
	const (
		openai1536 = "ca7ddf93a4304edd6af3c5186087b6ff02b1287ba95d6f4f08e8614a93da2ff7"
		openai512  = "25fc3d33efe1e1dce705f39a8b931b5bf8aa2c6f7a674278cd344c276e02fb0a"
		ollama1024 = "455faa2d69f82610033987afde69775d85cfca41fc21bb738d8b9ea67d8ce5f5"
	)
	tests := []struct {
		name       string
		baseURL    string
		model      string
		dimensions int
		want       string
	}{
		{"openai", "https://api.openai.com/v1", "text-embedding-3-small", 1536, openai1536},
		{"dimensions change the identity", "https://api.openai.com/v1", "text-embedding-3-small", 512, openai512},
		{"ollama", "http://localhost:11434/v1", "qwen3-embedding", 1024, ollama1024},
		{"trailing slash is ignored", "https://api.openai.com/v1/", "text-embedding-3-small", 1536, openai1536},
		{"several trailing slashes are ignored", "https://api.openai.com/v1//", "text-embedding-3-small", 1536, openai1536},
		{"scheme and host case is ignored", "HTTPS://API.OpenAI.com/v1", "text-embedding-3-small", 1536, openai1536},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := embed.ModelID(tt.baseURL, tt.model, tt.dimensions); got != tt.want {
				t.Errorf("ModelID(%q, %q, %d)\n got %s\nwant %s", tt.baseURL, tt.model, tt.dimensions, got, tt.want)
			}
		})
	}
}

func TestModelIDSeparatesFields(t *testing.T) {
	// Plain concatenation would make these two collide.
	a := embed.ModelID("http://h/v1", "ab", 1)
	b := embed.ModelID("http://h/v1a", "b", 1)

	if a == b {
		t.Errorf("ModelID collided for different (base_url, model) splits: %s", a)
	}
}
