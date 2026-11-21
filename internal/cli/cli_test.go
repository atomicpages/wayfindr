package cli

import (
	"bytes"
	"runtime"
	"strings"
	"testing"
)

func TestVersionPrintsVersionCommitAndGoVersion(t *testing.T) {
	version, commit = "1.2.3", "abc1234"
	t.Cleanup(func() { version, commit = "dev", "unknown" })

	var stdout, stderr bytes.Buffer
	code := Run([]string{"version"}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}

	want := "wayfindr 1.2.3 (commit abc1234, " + runtime.Version() + ")\n"

	if got := stdout.String(); got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
}

func TestRootHelpListsCommandsByGroup(t *testing.T) {
	var stdout, stderr bytes.Buffer

	if code := Run([]string{"--help"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}

	help := stdout.String()

	for _, heading := range []string{"\nDaemon:\n", "\nInspect:\n", "\nManage:\n", "\nPlumbing:\n"} {
		if !strings.Contains(help, heading) {
			t.Errorf("help has no %q heading:\n%s", strings.TrimSpace(heading), help)
		}
	}

	// A command left out of every group would show up here.
	if strings.Contains(help, "Additional Commands:") {
		t.Errorf("help lists ungrouped commands:\n%s", help)
	}
}

func TestValidLogLevelIsAccepted(t *testing.T) {
	var stdout, stderr bytes.Buffer

	if code := Run([]string{"--log-level", "debug", "version"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}
}

func TestInvalidLogLevelFails(t *testing.T) {
	for _, level := range []string{"bogus", "5", ""} {
		t.Run(level, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := Run([]string{"--log-level", level, "version"}, &stdout, &stderr)

			if code == 0 {
				t.Fatalf("exit code = 0, want non-zero; stdout: %s", stdout.String())
			}

			if msg := stderr.String(); !strings.Contains(msg, "--log-level") || !strings.Contains(msg, `"`+level+`"`) {
				t.Errorf("stderr = %q, want it to name --log-level and the bad value", msg)
			}

			if stdout.Len() != 0 {
				t.Errorf("stdout = %q, want empty: the command must not run", stdout.String())
			}
		})
	}
}

func TestUnknownSubcommandOfGroupFails(t *testing.T) {
	for _, group := range []string{"connector", "secret", "service", "token"} {
		t.Run(group, func(t *testing.T) {
			var stdout, stderr bytes.Buffer

			if code := Run([]string{group, "bogus"}, &stdout, &stderr); code != 1 {
				t.Fatalf("exit code = %d, want 1; stdout: %s", code, stdout.String())
			}

			want := `unknown command "bogus" for "wayfindr ` + group + `"`
			if msg := stderr.String(); !strings.Contains(msg, want) {
				t.Errorf("stderr = %q, want it to contain %q", msg, want)
			}
		})
	}
}

func TestUnknownSubcommandOfGroupSuggests(t *testing.T) {
	for _, tt := range []struct{ typo, want string }{
		{"ad", "add"},    // a prefix
		{"lsit", "list"}, // letters swapped
		{"remvoe", "remove"},
	} {
		t.Run(tt.typo, func(t *testing.T) {
			var stdout, stderr bytes.Buffer

			if code := Run([]string{"connector", tt.typo}, &stdout, &stderr); code != 1 {
				t.Fatalf("exit code = %d, want 1", code)
			}

			if msg := stderr.String(); !strings.Contains(msg, "Did you mean this?\n\t"+tt.want) {
				t.Errorf("stderr = %q, want a suggestion of %s", msg, tt.want)
			}
		})
	}
}

func TestGroupWithoutSubcommandPrintsHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer

	if code := Run([]string{"connector"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}

	if !strings.Contains(stdout.String(), "Available Commands:") {
		t.Errorf("stdout = %q, want the group's help", stdout.String())
	}
}
