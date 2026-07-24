package agent

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync/atomic"
	"time"

	"github.com/voxelum/xmcl-shared-node-agent/internal/controlplane"
)

var ErrLeaseLost = errors.New("command lease was denied or could not be renewed")

type Daemon struct {
	NodeID            string
	Capacity          controlplane.NodeCapacity
	Source            controlplane.CommandSource
	Reporter          controlplane.Reporter
	Executor          *Executor
	Status            func() controlplane.NodeStatus
	HeartbeatInterval time.Duration
}

func (d *Daemon) Register(ctx context.Context) error {
	return retry(ctx, func() error { return d.Reporter.Register(ctx, d.Capacity) })
}

func (d *Daemon) Process(ctx context.Context, command controlplane.Command) error {
	if err := d.Executor.TrackLease(command.CommandID, command.Lease); err != nil {
		return fmt.Errorf("persist command lease: %w", err)
	}
	executionContext, cancel := context.WithCancel(ctx)
	defer cancel()
	var leaseLost atomic.Bool
	stopRenewal := d.renewLease(executionContext, command, &leaseLost, cancel)
	defer stopRenewal()

	result, err := d.Executor.Execute(executionContext, command)
	if err != nil {
		return err
	}
	if leaseLost.Load() {
		return ErrLeaseLost
	}
	if result.Status == "started" {
		if err := retry(ctx, func() error {
			return d.Reporter.ReportStarted(ctx, command.ServiceID, command.AssignmentID)
		}); err != nil {
			return fmt.Errorf("report started: %w", err)
		}
	}
	if result.Status == "stopped-and-synced" && result.Sync != nil {
		if err := retry(ctx, func() error {
			return d.Reporter.ReportStoppedAndSynced(ctx, *result.Sync)
		}); err != nil {
			return fmt.Errorf("report stopped and synced: %w", err)
		}
	}
	if leaseLost.Load() {
		return ErrLeaseLost
	}
	if err := retry(ctx, func() error {
		return d.Source.Ack(ctx, command.CommandID, result)
	}); err != nil {
		return fmt.Errorf("ack command: %w", err)
	}
	if result.Status == "stopped-and-synced" {
		if err := d.Executor.Release(ctx, command); err != nil {
			return fmt.Errorf("release acknowledged local workspace: %w", err)
		}
	}
	if err := d.Executor.ClearLease(command.CommandID); err != nil {
		return fmt.Errorf("clear command lease: %w", err)
	}
	return nil
}

func (d *Daemon) Run(ctx context.Context) error {
	if err := d.Register(ctx); err != nil {
		return fmt.Errorf("register node: %w", err)
	}
	heartbeatContext, stopHeartbeats := context.WithCancel(ctx)
	defer stopHeartbeats()
	go d.heartbeatLoop(heartbeatContext)
	for {
		command, err := d.Source.Next(ctx, d.NodeID)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if err := wait(ctx, backoff(0)); err != nil {
				return nil
			}
			continue
		}
		if err := d.Process(ctx, command); err != nil && !errors.Is(err, ErrLeaseLost) {
			if ctx.Err() != nil {
				return nil
			}
		}
	}
}

func (d *Daemon) heartbeatLoop(ctx context.Context) {
	interval := d.HeartbeatInterval
	if interval <= 0 {
		interval = 20 * time.Second
	}
	for {
		status := controlplane.NodeStatus{NodeID: d.NodeID}
		if d.Status != nil {
			status = d.Status()
			status.NodeID = d.NodeID
		}
		_ = retry(ctx, func() error { return d.Reporter.Heartbeat(ctx, status) })
		if err := wait(ctx, interval); err != nil {
			return
		}
	}
}

func (d *Daemon) renewLease(ctx context.Context, command controlplane.Command, lost *atomic.Bool, cancel context.CancelFunc) func() {
	renewer, ok := d.Source.(controlplane.LeaseRenewer)
	if !ok || (command.Lease.Token == "" && command.Lease.Generation == "") {
		return func() {}
	}
	stop := make(chan struct{})
	go func() {
		interval := 20 * time.Second
		if expiry, err := time.Parse(time.RFC3339, command.Lease.ExpiresAt); err == nil {
			remaining := time.Until(expiry) / 2
			if remaining > 0 && remaining < interval {
				interval = remaining
			}
		}
		for {
			if err := waitUntil(ctx, stop, interval); err != nil {
				return
			}
			lease, err := renewer.RenewLease(ctx, command.CommandID, command.Lease)
			if err != nil || (lease.Token == "" && lease.Generation == "") {
				lost.Store(true)
				cancel()
				return
			}
			command.Lease = lease
			if err := d.Executor.TrackLease(command.CommandID, lease); err != nil {
				lost.Store(true)
				cancel()
				return
			}
		}
	}()
	return func() { close(stop) }
}

func retry(ctx context.Context, operation func() error) error {
	var err error
	for attempt := 0; ; attempt++ {
		if err = operation(); err == nil {
			return nil
		}
		if waitErr := wait(ctx, backoff(attempt)); waitErr != nil {
			return err
		}
	}
}

func backoff(attempt int) time.Duration {
	if attempt > 6 {
		attempt = 6
	}
	base := 100 * time.Millisecond * time.Duration(1<<attempt)
	return base + time.Duration(rand.IntN(100))*time.Millisecond
}

func wait(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func waitUntil(ctx context.Context, stop <-chan struct{}, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-stop:
		return context.Canceled
	case <-timer.C:
		return nil
	}
}
