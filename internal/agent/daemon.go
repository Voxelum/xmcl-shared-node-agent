package agent

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
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

type credentialManager interface {
	HasCredential() bool
	IsAuthenticationFailure(error) bool
	InvalidateCredential() error
}

func (d *Daemon) Register(ctx context.Context) error {
	if credentials := d.credentials(); credentials != nil && credentials.HasCredential() {
		return nil
	}
	return retry(ctx, func() error { return d.Reporter.Register(ctx, d.Capacity) })
}

func (d *Daemon) Process(ctx context.Context, command controlplane.Command) error {
	if err := d.Executor.TrackLease(command.CommandID, command.Lease); err != nil {
		return fmt.Errorf("persist command lease: %w", err)
	}
	lease := command.Lease
	var leaseMu sync.RWMutex
	executionContext, cancel := context.WithCancel(ctx)
	defer cancel()
	var leaseLost atomic.Bool
	stopRenewal := d.renewLease(executionContext, command.CommandID, &lease, &leaseMu, &leaseLost, cancel)
	defer stopRenewal()

	result, err := d.Executor.Execute(executionContext, command)
	if err != nil {
		return err
	}
	if leaseLost.Load() {
		return ErrLeaseLost
	}
	if result.Status == "started" {
		if command.Connection == nil {
			return errors.New("started command is missing its assigned public connection")
		}
		endpoint := command.Connection.Endpoint()
		if err := d.retry(ctx, func() error {
			return d.Reporter.ReportStarted(ctx, command.ServiceID, command.AssignmentID, endpoint)
		}); err != nil {
			return fmt.Errorf("report started: %w", err)
		}
	}
	if result.Status == "stopped-and-synced" && result.Sync != nil {
		leaseMu.RLock()
		result.Sync.CommandID = command.CommandID
		result.Sync.Lease = lease
		leaseMu.RUnlock()
		if err := d.retry(ctx, func() error {
			return d.Reporter.ReportStoppedAndSynced(ctx, *result.Sync)
		}); err != nil {
			return fmt.Errorf("report stopped and synced: %w", err)
		}
	}
	if leaseLost.Load() {
		return ErrLeaseLost
	}
	leaseMu.RLock()
	acknowledgementLease := lease
	leaseMu.RUnlock()
	if err := d.retry(ctx, func() error {
		return d.Source.Ack(ctx, command.CommandID, acknowledgementLease, result)
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
	if err := d.heartbeat(ctx); err != nil {
		return fmt.Errorf("send initial heartbeat: %w", err)
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
			recovered, recoveryErr := d.recoverAuthentication(ctx, err)
			if recoveryErr != nil {
				return fmt.Errorf("re-enroll node after authentication failure: %w", recoveryErr)
			}
			if recovered {
				continue
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
		_ = d.heartbeat(ctx)
		if err := wait(ctx, interval); err != nil {
			return
		}
	}
}

func (d *Daemon) heartbeat(ctx context.Context) error {
	if d.Status == nil {
		return errors.New("shared-node status provider is required")
	}
	status := d.Status()
	status.NodeID = d.NodeID
	return d.retry(ctx, func() error { return d.Reporter.Heartbeat(ctx, status) })
}

func (d *Daemon) renewLease(ctx context.Context, commandID string, lease *controlplane.CommandLease, leaseMu *sync.RWMutex, lost *atomic.Bool, cancel context.CancelFunc) func() {
	renewer, ok := d.Source.(controlplane.LeaseRenewer)
	leaseMu.RLock()
	hasLease := lease.Token != "" && lease.Generation > 0
	leaseMu.RUnlock()
	if !ok || !hasLease {
		return func() {}
	}
	stop := make(chan struct{})
	go func() {
		interval := 20 * time.Second
		leaseMu.RLock()
		expiresAt := lease.ExpiresAt
		leaseMu.RUnlock()
		if expiry, err := time.Parse(time.RFC3339, expiresAt); err == nil {
			remaining := time.Until(expiry) / 2
			if remaining > 0 && remaining < interval {
				interval = remaining
			}
		}
		for {
			if err := waitUntil(ctx, stop, interval); err != nil {
				return
			}
			leaseMu.RLock()
			current := *lease
			leaseMu.RUnlock()
			renewed, err := d.renew(ctx, func() (controlplane.CommandLease, error) {
				return renewer.RenewLease(ctx, commandID, current)
			})
			if err != nil || renewed.Token == "" || renewed.Generation < 1 {
				lost.Store(true)
				cancel()
				return
			}
			leaseMu.Lock()
			*lease = renewed
			leaseMu.Unlock()
			if err := d.Executor.TrackLease(commandID, renewed); err != nil {
				lost.Store(true)
				cancel()
				return
			}
		}
	}()
	return func() { close(stop) }
}

func (d *Daemon) retry(ctx context.Context, operation func() error) error {
	for attempt := 0; ; attempt++ {
		operationErr := operation()
		if operationErr == nil {
			return nil
		}
		if recovered, recoveryErr := d.recoverAuthentication(ctx, operationErr); recoveryErr != nil {
			return recoveryErr
		} else if recovered {
			attempt = -1
			continue
		}
		if waitErr := wait(ctx, backoff(attempt)); waitErr != nil {
			return operationErr
		}
	}
}

func (d *Daemon) renew(ctx context.Context, operation func() (controlplane.CommandLease, error)) (controlplane.CommandLease, error) {
	lease, err := operation()
	if err == nil {
		return lease, nil
	}
	recovered, recoveryErr := d.recoverAuthentication(ctx, err)
	if recoveryErr != nil {
		return controlplane.CommandLease{}, recoveryErr
	}
	if !recovered {
		return controlplane.CommandLease{}, err
	}
	return operation()
}

func (d *Daemon) recoverAuthentication(ctx context.Context, err error) (bool, error) {
	credentials := d.credentials()
	if credentials == nil || !credentials.IsAuthenticationFailure(err) {
		return false, nil
	}
	if err := credentials.InvalidateCredential(); err != nil {
		return true, err
	}
	return true, retry(ctx, func() error { return d.Reporter.Register(ctx, d.Capacity) })
}

func (d *Daemon) credentials() credentialManager {
	if credentials, ok := d.Source.(credentialManager); ok {
		return credentials
	}
	if credentials, ok := d.Reporter.(credentialManager); ok {
		return credentials
	}
	return nil
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
