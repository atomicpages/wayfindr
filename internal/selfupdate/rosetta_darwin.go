//go:build darwin

package selfupdate

import "golang.org/x/sys/unix"

// translated reports whether this process runs under Rosetta 2. The sysctl
// is missing on Intel Macs, which counts as not translated.
func translated() bool {
	v, err := unix.SysctlUint32("sysctl.proc_translated")

	return err == nil && v == 1
}
