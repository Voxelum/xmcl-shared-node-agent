package controlplane

import "context"

type commandLeaseSnapshotKey struct{}

// CommandLeaseSnapshot returns the latest lease for one executing command.
// Implementations must be safe for concurrent renewal and request delivery.
type CommandLeaseSnapshot func() CommandLease

func WithCommandLeaseSnapshot(ctx context.Context, snapshot CommandLeaseSnapshot) context.Context {
	if snapshot == nil {
		return ctx
	}
	return context.WithValue(ctx, commandLeaseSnapshotKey{}, snapshot)
}

func CurrentCommandLease(ctx context.Context, fallback CommandLease) CommandLease {
	snapshot, ok := ctx.Value(commandLeaseSnapshotKey{}).(CommandLeaseSnapshot)
	if !ok || snapshot == nil {
		return fallback
	}
	return snapshot()
}
