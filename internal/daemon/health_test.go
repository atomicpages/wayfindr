package daemon

import (
	"testing"

	"github.com/google/go-cmp/cmp"
)

// remember is a pure decision table (CODING_STANDARDS: Tests), so it is
// tested directly; warnings_test.go covers its effect through the running
// daemon's log.
func TestRememberLogsEachWarningOncePerTransition(t *testing.T) {
	stuck := warning{connector: "jira", code: "stuck"}
	expiring := warning{connector: "linear", code: "refresh_expiring"}
	note := warning{connector: "slack", code: "no_refresh", note: true}
	logged := map[warningKey]bool{}

	for _, step := range []struct {
		name   string
		found  []warning
		unread map[string]bool
		want   []warning
	}{
		{name: "both start to apply; a note is never logged", found: []warning{stuck, expiring, note}, want: []warning{stuck, expiring}},
		{name: "both still apply", found: []warning{stuck, expiring}},
		{name: "linear's credential cannot be read: its warning is kept", found: []warning{stuck}, unread: map[string]bool{"linear": true}},
		{name: "linear's read again, the warning not cleared", found: []warning{stuck, expiring}},
		{name: "jira clears", found: []warning{expiring}},
		{name: "jira comes back", found: []warning{stuck, expiring}, want: []warning{stuck}},
	} {
		var fresh []warning
		fresh, logged = remember(logged, step.found, step.unread)

		if diff := cmp.Diff(step.want, fresh, cmp.AllowUnexported(warning{})); diff != "" {
			t.Errorf("%s: logged (-want +got):\n%s", step.name, diff)
		}
	}
}
