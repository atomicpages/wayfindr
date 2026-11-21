// Package telemetry is the opt-in usage report (ADR 0020, GLOSSARY: Usage
// report): once a day the daemon sends the collector counts and allowlisted
// names that say how Wayfindr is used, never anything a user typed. It owns
// the report's schema, the allowlists, the proof-of-work the collector
// checks, and the Reporter that counts usage and sends it.
//
// Only the daemon imports it. Nothing is counted, no install ID is written,
// and no request is made unless telemetry.enabled is true, DO_NOT_TRACK is
// unset, the binary is a release build, and it has an endpoint.
package telemetry

import (
	"math"
	"time"

	"github.com/atomicpages/wayfindr/internal/config"
)

const (
	// Schema is the report's schema version. A change to the report's
	// fields, their meaning, or the proof-of-work bumps it.
	Schema = 1

	// maxCount caps every usage count, so a long window never exceeds what
	// the collector accepts.
	maxCount = 10_000_000
	// maxWindowHours caps window_hours at a year.
	maxWindowHours = 8760
)

// Report is the JSON body of one usage report (schema 1). Its fields are
// the contract with the collector: docs/reference/telemetry.md lists them,
// and testdata/report-v1.json is the golden copy.
type Report struct {
	Schema    int    `json:"schema"`
	InstallID string `json:"install_id"`
	// WindowHours is how many hours the usage counts cover: since the last
	// report the collector accepted, or since telemetry was turned on.
	WindowHours int    `json:"window_hours"`
	Version     string `json:"version"`
	Commit      string `json:"commit"`
	Go          string `json:"go"`
	OS          string `json:"os"`
	Arch        string `json:"arch"`
	Store       string `json:"store"`
	Embedding   bool   `json:"embedding"`
	// EmbeddingProvider and EmbeddingModel are allowlisted names, or
	// "other"; empty, and left out, when Embedding is false.
	EmbeddingProvider string     `json:"embedding_provider,omitempty"`
	EmbeddingModel    string     `json:"embedding_model,omitempty"`
	Connectors        Connectors `json:"connectors"`
	Profiles          int        `json:"profiles"`
	Searches          int64      `json:"searches"`
	Calls             Calls      `json:"calls"`
	// Clients counts call_tool calls by allowlisted client name, or
	// "other".
	Clients map[string]int64 `json:"clients"`
	PoW     PoW              `json:"pow"`
}

// Connectors counts the configured connectors twice: by transport and by
// how they authenticate. Both add up to the number of connectors.
type Connectors struct {
	Transport Transports `json:"transport"`
	Auth      Auths      `json:"auth"`
}

// Transports counts connectors by transport.
type Transports struct {
	Stdio      int `json:"stdio"`
	Streamable int `json:"streamable"`
	SSE        int `json:"sse"`
}

// Auths counts connectors by how they authenticate.
type Auths struct {
	// API is a url connector with a headers table and no OAuth.
	API int `json:"api"`
	// None is everything else, stdio connectors included.
	None  int `json:"none"`
	OAuth int `json:"oauth"`
}

// Calls counts call_tool calls by result (store.CallResult).
type Calls struct {
	OK            int64 `json:"ok"`
	ToolError     int64 `json:"tool_error"`
	Rejected      int64 `json:"rejected"`
	Failed        int64 `json:"failed"`
	InputRequired int64 `json:"input_required"`
}

// PoW is the report's proof-of-work: Nonce solves Difficulty for the
// install ID on Date, the sender's UTC date.
type PoW struct {
	Date  string `json:"date"`
	Nonce string `json:"nonce"`
}

// describe fills the report's fields that come from the config.
func describe(r *Report, cfg *config.Config) {
	r.Store = cfg.Storage.Driver
	r.Profiles = len(cfg.Profiles)

	if e := cfg.Embedding; e != nil {
		r.Embedding = true
		r.EmbeddingProvider = embeddingProvider(e.BaseURL)
		r.EmbeddingModel = embeddingModel(e.Model)
	}

	for _, c := range cfg.Connectors {
		stdio := c.IsStdio()

		switch {
		case stdio:
			r.Connectors.Transport.Stdio++
		case c.Transport == config.TransportSSE:
			r.Connectors.Transport.SSE++
		default:
			r.Connectors.Transport.Streamable++
		}

		switch {
		case c.UsesOAuth():
			r.Connectors.Auth.OAuth++
		case !stdio && len(c.Headers) > 0:
			r.Connectors.Auth.API++
		default:
			r.Connectors.Auth.None++
		}
	}
}

// windowHours is the hours from start to now, rounded up, in [1, a year].
func windowHours(start, now time.Time) int {
	h := math.Ceil(now.Sub(start).Hours())

	return int(min(max(h, 1), maxWindowHours))
}

// capped bounds a usage count at what the collector accepts.
func capped(n int64) int64 { return min(n, maxCount) }
