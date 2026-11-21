package service

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/atomicpages/wayfindr/internal/paths"
)

// systemd manages the unit in the user's service manager (systemctl --user).
type systemd struct {
	run Runner
}

func (s systemd) systemctl(ctx context.Context, args ...string) error {
	return s.run(ctx, "systemctl", append([]string{"--user"}, args...)...)
}

// load makes systemd read the new unit, enables it for future logins, and
// restarts it, which starts it if it is stopped and picks up a replaced
// unit if it is running (enable --now would leave a running one as is).
func (s systemd) load(ctx context.Context, _ string) error {
	if err := s.systemctl(ctx, "daemon-reload"); err != nil {
		return err
	}

	if err := s.systemctl(ctx, "enable", UnitName); err != nil {
		return err
	}

	return s.systemctl(ctx, "restart", UnitName)
}

func (s systemd) remove(ctx context.Context, unitFile string) error {
	if err := s.systemctl(ctx, "disable", "--now", UnitName); err != nil {
		return err
	}

	if err := removeUnit(unitFile); err != nil {
		return err
	}

	return s.systemctl(ctx, "daemon-reload")
}

// unitFile is in $XDG_CONFIG_HOME/systemd/user, else ~/.config/systemd/user:
// the user unit directory systemd reads for that user.
func (systemd) unitFile(env paths.Env) (string, error) {
	base := env.Getenv("XDG_CONFIG_HOME")

	if !filepath.IsAbs(base) {
		home, err := env.HomeDir()

		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}

		base = filepath.Join(home, ".config")
	}

	return filepath.Join(base, "systemd", "user", UnitName), nil
}

// render writes the unit. Restart=on-failure restarts the daemon only when
// it fails, so a clean shutdown stays down; WantedBy=default.target starts
// it when the user's service manager starts. stdout and stderr stay with
// the journal, which catches a daemon that fails before it opens --log-file.
func (systemd) render(u unit) string {
	var b strings.Builder
	b.WriteString("# Written by `wayfindr service install`, which replaces it;\n")
	b.WriteString("# `wayfindr service uninstall` removes it.\n")
	b.WriteString("[Unit]\nDescription=Wayfindr MCP gateway\n\n[Service]\n")
	args := u.args()
	quoted := make([]string, len(args))

	for i, a := range args {
		quoted[i] = execArg(a)
	}

	b.WriteString("ExecStart=" + strings.Join(quoted, " ") + "\n")

	for _, v := range u.env {
		b.WriteString("Environment=" + envAssignment(v) + "\n")
	}

	b.WriteString("Restart=on-failure\nRestartSec=5\n\n[Install]\nWantedBy=default.target\n")

	return b.String()
}

// execArg quotes one ExecStart word for systemd: % and $ are doubled so
// systemd expands neither specifiers nor variables, and a word with any
// character outside a conservative safe set is double-quoted with C-style
// escapes.
func execArg(s string) string {
	escaped := strings.NewReplacer(`%`, `%%`, `$`, `$$`, `\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s)

	if s != "" && strings.IndexFunc(s, unsafeInExec) < 0 {
		return escaped
	}

	return `"` + escaped + `"`
}

// envAssignment quotes one Environment= assignment. systemd expands
// specifiers there, so % is doubled, but not variables, so $ stays as is.
func envAssignment(v envVar) string {
	return `"` + strings.NewReplacer(`%`, `%%`, `\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(v.name+"="+v.value) + `"`
}

func unsafeInExec(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return false
	case strings.ContainsRune("/._-+=:,@%$~", r):
		return false
	default:
		return true
	}
}
