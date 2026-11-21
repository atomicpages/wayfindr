package cli

import (
	"fmt"
	"path/filepath"

	"github.com/atomicpages/wayfindr/internal/paths"
)

// pathFlags are the root's --config and --data-dir. They do not resolve
// anything themselves: overlay puts them in the host's environment in place
// of WAYFINDR_CONFIG and WAYFINDR_DATA_DIR, so every command resolves its
// paths through internal/paths as if those were set (design §3), and the
// daemons and service units it starts are given the same values.
type pathFlags struct {
	config  string
	dataDir string
}

// overlay returns env with the flags in place of the variables they
// override, when set. It reads the flags on each lookup, so it is made
// before they are parsed and every command built from it sees them.
func (f *pathFlags) overlay(env paths.Env) paths.Env {
	getenv := env.Getenv

	return paths.Env{
		HomeDir: env.HomeDir,
		Getenv: func(key string) string {
			switch {
			case key == paths.ConfigEnvVar && f.config != "":
				return f.config
			case key == paths.DataDirEnvVar && f.dataDir != "":
				return f.dataDir
			case getenv == nil:
				return ""
			default:
				return getenv(key)
			}
		},
	}
}

// absolute makes relative flag values absolute against the working
// directory: the daemon and the service that are given them run elsewhere.
func (f *pathFlags) absolute(workDir func() (string, error)) error {
	for _, v := range []*string{&f.config, &f.dataDir} {
		if *v == "" || filepath.IsAbs(*v) {
			continue
		}

		wd, err := workDir()

		if err != nil {
			return fmt.Errorf("resolve %s: %w", *v, err)
		}

		*v = filepath.Join(wd, *v)
	}

	return nil
}
