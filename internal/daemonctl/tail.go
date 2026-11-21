package daemonctl

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

const (
	// followInterval is how often Tail checks the log for new lines. A
	// poll, unlike a file watch, behaves the same on every OS and needs
	// nothing to be torn down.
	followInterval = 250 * time.Millisecond
	// tailChunk is how much Tail reads at a time, backwards from the end,
	// looking for the last lines.
	tailChunk = 64 << 10
)

// Tail writes the log at path to w: the last lines lines of it, or all of
// it when lines is negative. With follow it then keeps writing what is
// appended until ctx ends, starting from the top again if the file
// shrinks (truncated) or is replaced. The error wraps fs.ErrNotExist when
// there is no log.
func Tail(ctx context.Context, path string, w io.Writer, lines int, follow bool) error {
	f, err := os.Open(path) //nolint:gosec // the data directory's log file

	if err != nil {
		return fmt.Errorf("open log: %w", err)
	}

	defer func() { _ = f.Close() }()
	start, err := tailStart(f, lines)

	if err != nil {
		return fmt.Errorf("read log %s: %w", path, err)
	}

	off, err := copyFrom(f, start, w)

	if err != nil || !follow {
		return err
	}

	tick := time.NewTicker(followInterval)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}

		f, off, err = reopenIfReplaced(f, path, off)

		if err != nil {
			return err
		}

		if off, err = copyFrom(f, off, w); err != nil {
			return err
		}
	}
}

// tailStart returns the offset of the first of the last lines lines of f,
// reading backwards from the end so a large log is not read whole. A final
// line without a newline counts as a line.
func tailStart(f *os.File, lines int) (int64, error) {
	if lines < 0 {
		return 0, nil
	}

	info, err := f.Stat()

	if err != nil {
		return 0, err
	}

	end := info.Size()

	if lines == 0 || end == 0 {
		return end, nil
	}

	buf := make([]byte, tailChunk)
	pos := end
	count := 0

	for pos > 0 {
		n := min(int64(len(buf)), pos)
		pos -= n

		if _, err := f.ReadAt(buf[:n], pos); err != nil && !errors.Is(err, io.EOF) {
			return 0, err
		}

		for i := n - 1; i >= 0; i-- {
			// The newline that ends the file starts no line after it.
			if buf[i] != '\n' || pos+i == end-1 {
				continue
			}

			if count++; count == lines {
				return pos + i + 1, nil
			}
		}
	}

	return 0, nil
}

// copyFrom writes f from off to its current end to w and returns the new
// end.
func copyFrom(f *os.File, off int64, w io.Writer) (int64, error) {
	n, err := io.Copy(w, io.NewSectionReader(f, off, 1<<62))

	if err != nil {
		return off, fmt.Errorf("read log: %w", err)
	}

	return off + n, nil
}

// reopenIfReplaced follows the log at path across a truncation (the file
// is shorter than what was read) or a replacement (path names another
// file now): either way the rest is read from the top. A path that is
// gone for now keeps the file that is open.
func reopenIfReplaced(f *os.File, path string, off int64) (*os.File, int64, error) {
	cur, err := f.Stat()

	if err != nil {
		return f, off, fmt.Errorf("read log: %w", err)
	}

	now, err := os.Stat(path)

	if err != nil {
		return f, off, nil //nolint:nilerr // gone for now: keep following the open file
	}

	if os.SameFile(cur, now) {
		if cur.Size() < off {
			return f, 0, nil
		}

		return f, off, nil
	}

	g, err := os.Open(path) //nolint:gosec // the data directory's log file

	if err != nil {
		return f, off, nil //nolint:nilerr // replaced but not readable yet: try again on the next tick
	}

	_ = f.Close()

	return g, 0, nil
}
