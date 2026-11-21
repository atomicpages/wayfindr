// Package daemonlock is the daemon lock: an advisory OS lock on a file in
// the data directory that the running daemon holds, so a second daemon on
// that data directory refuses to start rather than share its store (ADR
// 0003, design §7.8). The OS drops the lock when the holder's process
// exits, crashed or not, so it is never stale. The file names the holder,
// so a second daemon and the shim can say which daemon runs.
//
// It is a leaf: the standard library and golang.org/x/sys only.
package daemonlock

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

const (
	// acquireWait is how long Acquire retries a lock it finds held. A
	// Probe holds the lock for a moment, and must not make a daemon that
	// starts then refuse; a daemon holds it for as long as it runs.
	acquireWait = 250 * time.Millisecond
	// retryInterval is the pause between Acquire's attempts.
	retryInterval = 10 * time.Millisecond
)

// Holder is the daemon that holds the lock, as it wrote itself into the
// file.
type Holder struct {
	// PID is the holder's process id.
	PID int `json:"pid"`
	// Listen is the server.listen the holder was started with.
	Listen string `json:"listen"`
}

// HeldError is Acquire's error when another daemon holds the lock. Holder
// is the zero Holder when the holder has not written itself in yet.
type HeldError struct {
	Path   string
	Holder Holder
}

func (e *HeldError) Error() string {
	if e.Holder.Listen == "" {
		return fmt.Sprintf("daemon already running: another process holds %s", e.Path)
	}

	return fmt.Sprintf("daemon already running at %s (pid %d): it holds %s", e.Holder.Listen, e.Holder.PID, e.Path)
}

// Lock is a held daemon lock.
type Lock struct {
	f *os.File
}

// Acquire takes the lock at path for h, creating the file and its
// directory (0700) if missing, and writes h into it. When another process
// holds it, Acquire returns a *HeldError naming that holder.
func Acquire(ctx context.Context, path string, h Holder) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("daemon lock: %w", err)
	}

	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec // the path is the data directory's lock file

	if err != nil {
		return nil, fmt.Errorf("daemon lock: %w", err)
	}

	if err := lockRetrying(ctx, f, path); err != nil {
		_ = f.Close()

		return nil, err
	}

	// The holder writes itself in only once it holds the lock, so a
	// daemon that is refused never clobbers the holder's name.
	data, err := json.Marshal(h)

	if err == nil {
		err = f.Truncate(0)
	}

	if err == nil {
		_, err = f.WriteAt(data, 0)
	}

	if err != nil {
		_ = f.Close()

		return nil, fmt.Errorf("daemon lock %s: write holder: %w", path, err)
	}

	return &Lock{f: f}, nil
}

// lockRetrying takes the exclusive lock on f, retrying for acquireWait.
func lockRetrying(ctx context.Context, f *os.File, path string) error {
	deadline := time.Now().Add(acquireWait)

	for {
		ok, err := tryLock(f, true)

		if err != nil {
			return fmt.Errorf("daemon lock %s: %w", path, err)
		}

		if ok {
			return nil
		}

		if time.Now().After(deadline) {
			return &HeldError{Path: path, Holder: readHolder(path)}
		}

		t := time.NewTimer(retryInterval)

		select {
		case <-ctx.Done():
			t.Stop()

			return ctx.Err()
		case <-t.C:
		}
	}
}

// Release drops the lock. The file stays: removing it would let a daemon
// lock a new file while another still holds the old one.
func (l *Lock) Release() error {
	if err := errors.Join(unlock(l.f), l.f.Close()); err != nil {
		return fmt.Errorf("release daemon lock: %w", err)
	}

	return nil
}

// Probe reports the daemon that holds the lock at path, or false when none
// does. It takes a shared lock for a moment to find out, which Acquire
// waits out.
func Probe(path string) (Holder, bool, error) {
	f, err := os.Open(path) //nolint:gosec // the path is the data directory's lock file

	if errors.Is(err, fs.ErrNotExist) {
		return Holder{}, false, nil
	}

	if err != nil {
		return Holder{}, false, fmt.Errorf("probe daemon lock: %w", err)
	}

	defer func() { _ = f.Close() }()
	ok, err := tryLock(f, false)

	if err != nil {
		return Holder{}, false, fmt.Errorf("probe daemon lock %s: %w", path, err)
	}

	if ok {
		// Best effort: closing f drops it too.
		_ = unlock(f)

		return Holder{}, false, nil
	}

	return readHolder(path), true, nil
}

// readHolder reads the holder from the file at path: the zero Holder if
// it has not written itself in yet.
func readHolder(path string) Holder {
	var h Holder
	data, err := os.ReadFile(path) //nolint:gosec // the path is the data directory's lock file

	if err != nil || json.Unmarshal(data, &h) != nil {
		return Holder{}
	}

	return h
}
