//go:build !darwin && !linux

package cli

// discardTyped does nothing here: Windows consoles have no tcflush that
// x/sys/unix offers, so typed-ahead input still answers the question.
func discardTyped(int) error { return nil }
