//go:build !darwin

package selfupdate

// translated is false off macOS: Rosetta 2 exists only there.
func translated() bool { return false }
