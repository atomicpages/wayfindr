package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/atomicpages/wayfindr/internal/admin"
	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/newfile"
)

// setPublicURL serves admin.Options.SetPublicURL: it writes server.public_url
// to the daemon's own config file with config.SetPublicURL, as an add writes
// a connector, and then reloads the file at once rather than wait for the
// watcher (ADR 0021). An empty url removes the key. It holds the writer's lock
// so it never races an add: both rewrite the same file.
//
// The key is informational, so nothing but the running config's copy of it
// changes on the reload, and no restart is needed.
func (a *configWriter) setPublicURL(ctx context.Context, publicURL string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	src, err := os.ReadFile(a.file)

	if err != nil {
		return fmt.Errorf("read config file: %w", err)
	}

	edited, err := config.SetPublicURL(src, publicURL, a.opts)

	if err != nil {
		return publicURLError(err)
	}

	switch {
	case edited.Changed:
		if err := newfile.Replace(a.file, src, edited.File, a.file); err != nil {
			if errors.Is(err, newfile.ErrChanged) {
				return &admin.ConfigError{Kind: admin.ConfigKindConflict, Case: admin.ConfigCaseConfigChanged,
					Advice: admin.Advice{Summary: "The config file changed while the public URL was being set. Try again."}}
			}

			return fmt.Errorf("write config file: %w", err)
		}

		a.log.Info().Bool("set", publicURL != "").Msg("public url written to the config file")
	case a.config().PublicURL == publicURL:
		return nil
	}

	// Here the file holds publicURL (written just now, or by hand before the
	// watcher reloaded it) but the running config may not: reload, so the
	// answer is the value the daemon runs.
	a.reload(ctx)

	if got := a.config().PublicURL; got != publicURL {
		// The reload failed; the status says why (config_error).
		return errors.New("server.public_url is in the file but the reload did not apply it")
	}

	return nil
}

// publicURLError words a refused config.SetPublicURL for the console: the
// URL's own validation issues, or the editor's Kind and Case. config.Advise
// is worded for the CLI, so the console uses config.Reason instead.
func publicURLError(err error) error {
	if ve := (*config.ValidationError)(nil); errors.As(err, &ve) {
		var issues []string

		for _, is := range ve.Issues {
			issues = append(issues, is.Message)
		}

		return &admin.ConfigError{Kind: admin.ConfigKindInvalid, Case: admin.ConfigCaseInvalidPublicURL,
			Advice: admin.Advice{Summary: "The public URL is not valid: " + strings.Join(issues, "; ") + "."}}
	}

	ce, ok := config.AsError(err)

	if !ok {
		return err
	}

	return &admin.ConfigError{Kind: configKind(ce.Kind), Case: configCase(ce.Case),
		Advice: admin.Advice{Summary: "The config file can't be changed safely: " + config.Reason(ce) + ". Set server.public_url in the file by hand."}}
}
