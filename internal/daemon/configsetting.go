package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/atomicpages/wayfindr/internal/admin"
	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/newfile"
)

// settingWrite is one edit of a single setting of the config file, for
// [configWriter.writeSetting]: the editor, how its refusals are worded, and
// how to tell that the running config holds the result.
type settingWrite struct {
	// edit is the config package's editor, applied to the file's text.
	edit func(src []byte) (config.Edited, error)
	// refused words a refused edit for the console.
	refused func(err error) error
	// changedSummary is the advice when the file changed under the write.
	changedSummary string
	// written logs that the edit reached the file.
	written func()
	// applied reports whether the running config holds the value.
	applied func() bool
	// notApplied is the error when the reload did not apply the value.
	notApplied string
}

// writeSetting writes one setting to the daemon's own config file and then
// reloads the file at once rather than wait for the watcher. It holds the
// writer's lock so it never races an add or another setting: all rewrite the
// same file. When the file already holds the value (set by hand before the
// watcher reloaded it) nothing is written, but the reload still runs if the
// running config has not caught up, so the answer is what the daemon runs.
func (a *configWriter) writeSetting(ctx context.Context, w settingWrite) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	src, err := os.ReadFile(a.file)

	if err != nil {
		return fmt.Errorf("read config file: %w", err)
	}

	edited, err := w.edit(src)

	if err != nil {
		return w.refused(err)
	}

	switch {
	case edited.Changed:
		if err := newfile.Replace(a.file, src, edited.File, a.file); err != nil {
			if errors.Is(err, newfile.ErrChanged) {
				return &admin.ConfigError{Kind: admin.ConfigKindConflict, Case: admin.ConfigCaseConfigChanged,
					Advice: admin.Advice{Summary: w.changedSummary}}
			}

			return fmt.Errorf("write config file: %w", err)
		}

		w.written()
	case w.applied():
		return nil
	}

	// Here the file holds the value (written just now, or by hand before the
	// watcher reloaded it) but the running config may not: reload.
	a.reload(ctx)

	if !w.applied() {
		// The reload failed; the status says why (config_error).
		return errors.New(w.notApplied)
	}

	return nil
}
