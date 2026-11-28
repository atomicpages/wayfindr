package daemon

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/atomicpages/wayfindr/internal/admin"
	"github.com/atomicpages/wayfindr/internal/gateway"
)

// adminApprovals adapts the gateway's approval queue to admin.Approvals
// (ADR 0024). It only converts: rows to admin's, and gateway.ErrNoApproval
// and ErrApprovalChanged to admin's. It never logs an entry's arguments.
type adminApprovals struct{ gw *gateway.Gateway }

var _ admin.Approvals = adminApprovals{}

// List implements admin.Approvals.
func (a adminApprovals) List(context.Context) ([]admin.ApprovalRow, error) {
	entries := a.gw.Approvals()
	rows := make([]admin.ApprovalRow, 0, len(entries))

	for _, e := range entries {
		rows = append(rows, admin.ApprovalRow{
			ID:        e.ID,
			Client:    e.Client,
			Tool:      e.Connector + "." + e.Tool,
			Class:     string(e.Class),
			Arguments: e.Args,
			State:     adminApprovalState(e.State),
			Created:   e.Created,
			Expires:   e.Expires,
		})
	}

	return rows, nil
}

// adminApprovalState is s in admin's words. The switch lists every
// gateway state, so a new one fails the exhaustive lint here rather than
// reaching the CLI and console unnamed.
func adminApprovalState(s gateway.ApprovalState) admin.ApprovalState {
	switch s {
	case gateway.ApprovalPending:
		return admin.ApprovalPending
	case gateway.ApprovalGranted:
		return admin.ApprovalGranted
	case gateway.ApprovalDenied:
		return admin.ApprovalDenied
	}

	return admin.ApprovalState(s)
}

// Approve implements admin.Approvals.
func (a adminApprovals) Approve(_ context.Context, id string, created time.Time) error {
	return adminApprovalErr(a.gw.Approve(id, created))
}

// Deny implements admin.Approvals.
func (a adminApprovals) Deny(_ context.Context, id string, created time.Time) error {
	return adminApprovalErr(a.gw.Deny(id, created))
}

// adminApprovalErr translates an error of the gateway's Approve or Deny
// into admin's: ErrNoApproval into admin.ErrNoApproval, ErrApprovalChanged
// into admin.ErrApprovalChanged, any other error passed through.
func adminApprovalErr(err error) error {
	switch {
	case errors.Is(err, gateway.ErrNoApproval):
		return fmt.Errorf("%w: %w", admin.ErrNoApproval, err)
	case errors.Is(err, gateway.ErrApprovalChanged):
		return fmt.Errorf("%w: %w", admin.ErrApprovalChanged, err)
	}

	return err
}
