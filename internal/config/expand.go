package config

import (
	"errors"
	"strconv"
	"strings"
)

// expandHome replaces a leading "~" or "~/" with the home directory in the
// fields that hold local paths: a sqlite storage.dsn, a stdio connector's
// command and args, and the files of a url connector's tls table and of its
// auth assertion (design §4). "~user/…" is left as written, as is anything
// not starting with ~, ${…} templates included. A value that
// needs the home directory when none can be resolved is an issue at its key.
func expandHome(c *Config, v *validator, lookup func() (string, error)) {
	h := &home{lookup: lookup}

	if c.Storage.Driver == DriverSQLite {
		c.Storage.DSN = h.expand(v, "storage.dsn", c.Storage.DSN)
	}

	for _, name := range sortedKeys(c.Connectors) {
		cn := c.Connectors[name]

		if cn.IsStdio() {
			cn.Command = h.expand(v, path("connectors", name, "command"), cn.Command)

			for i, a := range cn.Args {
				at := path("connectors", name, "args") + "[" + strconv.Itoa(i) + "]"
				cn.Args[i] = Template(h.expand(v, at, string(a)))
			}
		} else {
			if t := cn.TLS; t != nil {
				t.CertFile = h.expand(v, path("connectors", name, "tls", "cert_file"), t.CertFile)
				t.KeyFile = h.expand(v, path("connectors", name, "tls", "key_file"), t.KeyFile)
				t.CAFile = h.expand(v, path("connectors", name, "tls", "ca_file"), t.CAFile)
			}

			if cn.Auth != nil && cn.Auth.Assertion != nil {
				as := cn.Auth.Assertion
				as.File = h.expand(v, path("connectors", name, "auth", "assertion", "file"), as.File)
			}
		}

		c.Connectors[name] = cn
	}
}

// home looks the home directory up at most once, and only when a value
// needs it, so a config without ~ loads on a host that has none.
type home struct {
	lookup func() (string, error)
	done   bool
	dir    string
	err    error
}

func (h *home) get() (string, error) {
	if h.done {
		return h.dir, h.err
	}

	h.done = true

	if h.lookup == nil {
		h.err = errors.New("no home directory is known")

		return h.dir, h.err
	}

	h.dir, h.err = h.lookup()

	if h.err == nil && h.dir == "" {
		h.err = errors.New("the home directory is empty")
	}

	return h.dir, h.err
}

// expand returns s with a leading "~" or "~/" replaced by the home
// directory. The rest of s is kept byte for byte.
func (h *home) expand(v *validator, at, s string) string {
	if s != "~" && !strings.HasPrefix(s, "~/") {
		return s
	}

	dir, err := h.get()

	if err != nil {
		v.add(at, "cannot expand ~: %v", err)

		return s
	}

	if s == "~" {
		return dir
	}

	return strings.TrimRight(dir, `/\`) + s[1:]
}
