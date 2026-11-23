package daemon

import (
	"context"
	"errors"
	"strings"

	"github.com/atomicpages/wayfindr/internal/admin"
	"github.com/atomicpages/wayfindr/internal/config"
)

// setPublicURL serves admin.Options.SetPublicURL: it writes server.public_url
// to the daemon's own config file with config.SetPublicURL, as an add writes
// a connector, and then reloads the file at once rather than wait for the
// watcher (ADR 0021). An empty url removes the key. It writes through
// writeSetting, so it holds the writer's lock and never races an add.
//
// The key is informational, so nothing but the running config's copy of it
// changes on the reload, and no restart is needed.
func (a *configWriter) setPublicURL(ctx context.Context, publicURL string) error {
	return a.writeSetting(ctx, settingWrite{
		edit: func(src []byte) (config.Edited, error) {
			return config.SetPublicURL(src, publicURL, a.opts)
		},
		refused:        publicURLError,
		changedSummary: "The config file changed while the public URL was being set. Try again.",
		written: func() {
			a.log.Info().Bool("set", publicURL != "").Msg("public url written to the config file")
		},
		applied:    func() bool { return a.config().PublicURL == publicURL },
		notApplied: "server.public_url is in the file but the reload did not apply it",
	})
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
			Advice: admin.Advice{Summary: "The public URL " + strings.Join(issues, "; ") + "."}}
	}

	ce, ok := config.AsError(err)

	if !ok {
		return err
	}

	return &admin.ConfigError{Kind: configKind(ce.Kind), Case: configCase(ce.Case),
		Advice: admin.Advice{Summary: "The config file can't be changed safely: " + config.Reason(ce) + ". Set server.public_url in the file by hand."}}
}
