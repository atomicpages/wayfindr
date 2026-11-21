package service

import (
	"context"
	"encoding/xml"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/atomicpages/wayfindr/internal/paths"
)

// launchd manages the agent in the user's GUI domain, gui/<uid>, with the
// launchctl subcommands of macOS 10.11 and later (bootstrap, bootout).
type launchd struct {
	uid int
	run Runner
}

// domain is the user's GUI login session; target is the agent in it.
func (l launchd) domain() string { return fmt.Sprintf("gui/%d", l.uid) }
func (l launchd) target() string { return l.domain() + "/" + Label }

// loaded reports whether the agent is bootstrapped. launchctl print fails
// (exit 113) for an unknown service; any other failure also reads as not
// loaded, and the bootstrap that follows reports the real problem.
func (l launchd) loaded(ctx context.Context) bool {
	return l.run(ctx, "launchctl", "print", l.target()) == nil
}

// load boots out a running agent, since bootstrap refuses one that is
// already loaded, then enables the agent (clearing a `launchctl disable`
// that would make bootstrap fail) and bootstraps the new plist.
func (l launchd) load(ctx context.Context, unitFile string) error {
	if l.loaded(ctx) {
		if err := l.run(ctx, "launchctl", "bootout", l.target()); err != nil {
			return err
		}
	}

	if err := l.run(ctx, "launchctl", "enable", l.target()); err != nil {
		return err
	}

	return l.run(ctx, "launchctl", "bootstrap", l.domain(), unitFile)
}

func (l launchd) remove(ctx context.Context, unitFile string) error {
	if l.loaded(ctx) {
		if err := l.run(ctx, "launchctl", "bootout", l.target()); err != nil {
			return err
		}
	}

	return removeUnit(unitFile)
}

func (launchd) unitFile(env paths.Env) (string, error) {
	home, err := env.HomeDir()

	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}

	return filepath.Join(home, "Library", "LaunchAgents", Label+".plist"), nil
}

// render writes the plist. KeepAlive restarts the daemon only when it
// exits unsuccessfully, so a clean shutdown stays down; RunAtLoad starts it
// at login. stdout and stderr go to the log too, which catches a daemon
// that fails before it opens --log-file.
func (launchd) render(u unit) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<!-- Written by ` + "`wayfindr service install`" + `, which replaces it; ` + "`wayfindr service uninstall`" + ` removes it. -->
<plist version="1.0">
<dict>
`)
	key(&b, 1, "Label")
	str(&b, 1, Label)
	key(&b, 1, "ProgramArguments")
	b.WriteString("\t<array>\n")

	for _, a := range u.args() {
		str(&b, 2, a)
	}

	b.WriteString("\t</array>\n")

	if len(u.env) > 0 {
		key(&b, 1, "EnvironmentVariables")
		b.WriteString("\t<dict>\n")

		for _, v := range u.env {
			key(&b, 2, v.name)
			str(&b, 2, v.value)
		}

		b.WriteString("\t</dict>\n")
	}

	key(&b, 1, "RunAtLoad")
	b.WriteString("\t<true/>\n")
	key(&b, 1, "KeepAlive")
	b.WriteString("\t<dict>\n")
	key(&b, 2, "SuccessfulExit")
	b.WriteString("\t\t<false/>\n\t</dict>\n")
	key(&b, 1, "StandardOutPath")
	str(&b, 1, u.logFile)
	key(&b, 1, "StandardErrorPath")
	str(&b, 1, u.logFile)
	b.WriteString("</dict>\n</plist>\n")

	return b.String()
}

func key(b *strings.Builder, indent int, k string) { element(b, indent, "key", k) }

func str(b *strings.Builder, indent int, s string) { element(b, indent, "string", s) }

func element(b *strings.Builder, indent int, tag, text string) {
	b.WriteString(strings.Repeat("\t", indent) + "<" + tag + ">")
	_ = xml.EscapeText(b, []byte(text)) // a strings.Builder never fails
	b.WriteString("</" + tag + ">\n")
}
