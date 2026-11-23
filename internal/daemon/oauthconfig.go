package daemon

import (
	"context"
	"errors"
	"strings"

	"github.com/atomicpages/wayfindr/internal/admin"
	"github.com/atomicpages/wayfindr/internal/config"
)

// setOAuthEnabled serves admin.Options.SetOAuthEnabled: it writes
// server.oauth.enabled to the daemon's own config file with
// config.SetOAuthEnabled, as setPublicURL writes the public URL, and then
// reloads the file at once rather than wait for the watcher (ADR 0022). It
// writes through writeSetting, so it holds the writer's lock and never races
// an add or a public URL change.
func (a *configWriter) setOAuthEnabled(ctx context.Context, enabled bool) error {
	return a.writeSetting(ctx, settingWrite{
		edit: func(src []byte) (config.Edited, error) {
			return config.SetOAuthEnabled(src, enabled, a.opts)
		},
		refused:        oauthEnabledError,
		changedSummary: "The config file changed while sign-in for cloud agents was being turned on or off. Try again.",
		written: func() {
			a.log.Info().Bool("enabled", enabled).Msg("oauth setting written to the config file")
		},
		applied:    func() bool { return a.config().OAuth.Enabled == enabled },
		notApplied: "server.oauth.enabled is in the file but the reload did not apply it",
	})
}

// oauthEnabledError words a refused config.SetOAuthEnabled for the console:
// the config's validation issues, or the editor's Kind and Case.
// config.Advise is worded for the CLI, so the console uses config.Reason
// instead.
func oauthEnabledError(err error) error {
	if ve := (*config.ValidationError)(nil); errors.As(err, &ve) {
		var issues []string

		for _, is := range ve.Issues {
			issues = append(issues, is.Path+" "+is.Message)
		}

		return &admin.ConfigError{Kind: admin.ConfigKindInvalid, Case: admin.ConfigCaseInvalidOAuth,
			Advice: admin.Advice{Summary: "Sign-in for cloud agents can't be turned on: " + strings.Join(issues, "; ") + "."}}
	}

	ce, ok := config.AsError(err)

	if !ok {
		return err
	}

	return &admin.ConfigError{Kind: configKind(ce.Kind), Case: configCase(ce.Case),
		Advice: admin.Advice{Summary: "The config file can't be changed safely: " + config.Reason(ce) + ". Set server.oauth.enabled in the file by hand."}}
}
