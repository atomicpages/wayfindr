package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/atomicpages/wayfindr/internal/config"
)

const (
	// reloadDebounce is how long the config file must be quiet before a
	// reload (design §7.8): an editor's save is a burst of events, and one
	// reload should follow it.
	reloadDebounce = 250 * time.Millisecond
	// filePoll is how often the watcher checks the file the config
	// resolves to, for a change no event in the config's directory shows.
	filePoll = 2 * time.Second
)

// configWatcher watches the directory of the config file, not the file:
// editors save by writing a temp file and renaming it over the config, which
// replaces the file's inode, and a watch on the directory survives that.
//
// A config file that is a symlink, as stow, chezmoi, and home-manager make,
// is edited at its target, whose events land in another directory. Rather
// than watch that directory, which may be the Nix store (and kqueue holds a
// descriptor for every file in a watched directory), the watcher polls the
// file the config resolves to every filePoll (wayfindr-a2b0). The poll also
// sees a repointed link, which kqueue (macOS, the BSDs) raises no event for.
type configWatcher struct {
	w *fsnotify.Watcher
	// file is the config file's path, as the daemon reads it.
	file string
	// name is the config file's base name; events on other files in the
	// directory are ignored.
	name string
	// last is what the poll last saw of the file the config resolves to.
	last fileState
}

// fileState is the file a config path resolves to, as the poll compares
// it: a repoint changes path, a save changes the size or the time.
type fileState struct {
	path    string
	size    int64
	modTime time.Time
}

// newConfigWatcher starts watching file's directory. Events queue until
// run reads them, so an edit made between this call and run is not lost.
func newConfigWatcher(file string) (*configWatcher, error) {
	w, err := fsnotify.NewWatcher()

	if err != nil {
		return nil, fmt.Errorf("new watcher: %w", err)
	}

	dir := filepath.Dir(file)

	if err := w.Add(dir); err != nil {
		_ = w.Close()

		return nil, fmt.Errorf("watch %s: %w", dir, err)
	}

	return &configWatcher{w: w, file: file, name: filepath.Base(file), last: stateOf(file)}, nil
}

// stateOf resolves file's symlinks and stats the result. A file that
// doesn't resolve or stat is the zero fileState.
func stateOf(file string) fileState {
	resolved, err := filepath.EvalSymlinks(file)

	if err != nil {
		return fileState{}
	}

	fi, err := os.Stat(resolved)

	if err != nil {
		return fileState{}
	}

	return fileState{path: resolved, size: fi.Size(), modTime: fi.ModTime()}
}

// changed reports whether the file the config resolves to is another file,
// or the same one changed, since the poll last looked, and records it.
func (cw *configWatcher) changed() bool {
	now := stateOf(cw.file)

	if now == cw.last {
		return false
	}

	cw.last = now

	return true
}

// run calls reload once the config file has been quiet for reloadDebounce
// after any change to it, until ctx ends. It then closes the watcher.
func (cw *configWatcher) run(ctx context.Context, reload func(context.Context), log func(error)) {
	defer func() {
		if err := cw.w.Close(); err != nil {
			log(err)
		}
	}()
	timer := time.NewTimer(reloadDebounce)
	timer.Stop()
	defer timer.Stop()
	poll := time.NewTicker(filePoll)
	defer poll.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-cw.w.Events:
			if !ok {
				return
			}

			if cw.relevant(ev) {
				timer.Reset(reloadDebounce)
			}
		case err, ok := <-cw.w.Errors:
			if !ok {
				return
			}

			log(err)
		case <-poll.C:
			if cw.changed() {
				timer.Reset(reloadDebounce)
			}
		case <-timer.C:
			// Before the reload reads the file, so the poll doesn't take
			// the change an event already reported for a new one, and an
			// edit made during the reload is still seen.
			cw.changed()
			reload(ctx)
		}
	}
}

// relevant reports whether ev may have changed the config file's content.
// A rename of the config away or its removal counts: the reload then fails
// and the status shows why. A change of mode alone does not count.
func (cw *configWatcher) relevant(ev fsnotify.Event) bool {
	return filepath.Base(ev.Name) == cw.name && ev.Op != fsnotify.Chmod
}

// watchConfig starts watching the config file (design §7.8 step 2). It
// returns nil, having logged why, if the directory can't be watched: the
// daemon then runs without hot reload.
func (d *Daemon) watchConfig() *configWatcher {
	cw, err := newConfigWatcher(d.paths.ConfigFile)

	if err != nil {
		d.log.Error().Err(err).Msg("watch config; edits take effect on restart")

		return nil
	}

	return cw
}

// reloadOnChange runs cw, reloading the config file after each change,
// until the returned stop is called. stop waits for it, including a reload
// it is running. A nil cw does nothing.
func (d *Daemon) reloadOnChange(ctx context.Context, cw *configWatcher) (stop func()) {
	if cw == nil {
		return func() {}
	}

	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		cw.run(ctx, d.reloadFile, func(err error) { d.log.Warn().Err(err).Msg("config watch") })
	}()

	return func() {
		cancel()
		<-done
	}
}

// reloadFile loads the config file with startup's options and reloads it.
// An invalid config is logged and recorded for the status, and the running
// config stays.
func (d *Daemon) reloadFile(ctx context.Context) {
	next, err := config.Load(ctx, d.paths.ConfigFile, d.loadOpts)

	if err != nil {
		if ctx.Err() != nil {
			return
		}

		// config errors name keys and positions, never resolved values.
		d.log.Error().Err(err).Msg("config reload failed; the last good config keeps running")
		d.cfgState.failed(err)

		return
	}

	d.log.Info().Msg("config reloaded")
	d.Reload(ctx, next)
}
