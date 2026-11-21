package config

import (
	"bytes"
	"fmt"
	"os"

	"github.com/BurntSushi/toml"
)

// ListenAddress reads only [server].listen from the config file at path, for
// the CLI's admin client and the stdio shim, which need the daemon's address
// even when the rest of the file is invalid: the daemon keeps running its
// last good config then, and `wayfindr status` is how the user sees why.
//
// Unknown keys, wrong types, and rule violations anywhere but
// [server].listen are ignored, and an absent key is DefaultListen. It fails
// when the file can't be read, when [server].listen is not a loopback
// host:port (the rule Load applies), and on a TOML syntax error anywhere in
// the file: a file that doesn't parse has no trustworthy [server] table.
// serve and reload keep the full validation of Load.
func ListenAddress(path string) (string, error) {
	data, err := os.ReadFile(path) //nolint:gosec // the path is the user's own config file

	if err != nil {
		return "", fmt.Errorf("read config: %w", err)
	}

	var c struct {
		Server Server `toml:"server"`
	}

	if _, err := toml.NewDecoder(bytes.NewReader(data)).Decode(&c); err != nil {
		return "", fmt.Errorf("read listen address from config %s: %w", path, err)
	}

	if c.Server.Listen == "" {
		c.Server.Listen = DefaultListen
	}

	var v validator
	v.server(c.Server)

	if err := v.err(); err != nil {
		return "", fmt.Errorf("read listen address from config %s: %w", path, err)
	}

	return c.Server.Listen, nil
}
