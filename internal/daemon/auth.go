package daemon

import (
	"context"

	"github.com/atomicpages/wayfindr/internal/admin"
	"github.com/atomicpages/wayfindr/internal/auth"
	"github.com/atomicpages/wayfindr/internal/upstream"
)

// The adapters in this file give the admin API the daemon's modules in
// admin's own types. They only convert: the recovery module (recovery)
// decides what follows a fix, and auth what a flow's state means.

// adminUpstreams adapts the manager and the recovery module to
// admin.Upstreams: the status comes from the manager, and a secret set
// goes through the recovery module, so its retried rows show the tools
// their connectors synced. A secret set first rebuilds the embedder when
// [embedding]'s api_key refers to the secret (embedding).
type adminUpstreams struct {
	manager  *upstream.Manager
	recovery *recovery
	// embedding is Daemon.embeddingSecretSet, bound once the daemon
	// exists, before Serve.
	embedding func(ctx context.Context, name string)
}

func (u *adminUpstreams) Status() []upstream.ConnectorStatus { return u.manager.Status() }

func (u *adminUpstreams) SecretChanged(ctx context.Context, name string) []upstream.ConnectorStatus {
	u.embedding(ctx, name)

	return u.recovery.secretChanged(ctx, name)
}

// authFlows adapts auth's flow entry to admin.Flows. It only converts
// types: auth finds the connector, checks it uses OAuth, and decides the
// status and the advice of a flow that cannot start, for the browser's
// pages too. It reports a done flow once the recovery its token started
// has settled.
type authFlows struct {
	flows    *auth.Flows
	recovery *recovery
}

func (f authFlows) StartFlow(ctx context.Context, name string, start admin.AuthStart) (admin.AuthFlow, error) {
	fl, err := f.flows.Start(ctx, name, auth.StartOptions{Device: start.Device})

	if e, ok := auth.AsError(err); ok {
		return admin.AuthFlow{}, &admin.FlowError{
			Status: auth.HTTPStatus(err), Kind: string(e.Kind), Case: string(e.Case),
			Advice: adminAdvice(auth.Advise(e)),
		}
	}

	if err != nil {
		return admin.AuthFlow{}, err
	}

	return admin.AuthFlow{
		FlowID: fl.ID, Connector: fl.Connector, AuthorizeURL: fl.AuthorizeURL,
		UserCode: fl.UserCode, VerificationURI: fl.VerificationURI, VerificationURIComplete: fl.VerificationURIComplete,
		ExpiresAt: fl.ExpiresAt.UTC(),
	}, nil
}

func adminAdvice(a auth.Advice) admin.Advice {
	return admin.Advice{Summary: a.Summary, Action: a.Action, Link: a.Link}
}

func (f authFlows) WaitFlow(ctx context.Context, id string) (admin.FlowReport, bool) {
	res, ok := f.flows.Wait(ctx, id)

	if !ok {
		return admin.FlowReport{}, false
	}

	rep := admin.FlowReport{Connector: res.Connector, State: admin.FlowPending}

	switch res.State {
	case auth.FlowPending:
	case auth.FlowDone:
		// Until the recovery settles the flow reads as pending: the caller
		// polls again.
		if s, ok := f.recovery.wait(ctx, res.Connector); ok {
			rep.State, rep.Status = admin.FlowDone, &s
		}
	case auth.FlowFailed, auth.FlowExpired:
		rep.State = admin.FlowFailed

		if res.State == auth.FlowExpired {
			rep.State = admin.FlowExpired
		}

		if e, ok := auth.AsError(res.Err); ok {
			a := adminAdvice(auth.Advise(e))
			rep.Advice = &a
		}
	}

	return rep, true
}

// credentials adapts the recovery module's revoke to admin.Credentials.
type credentials struct {
	recovery *recovery
}

func (c credentials) Revoke(ctx context.Context, name string) (admin.RevokeReport, error) {
	found, s, err := c.recovery.revoke(ctx, name)

	return admin.RevokeReport{Found: found, Status: s}, err
}
