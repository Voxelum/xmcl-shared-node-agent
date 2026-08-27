package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/voxelum/xmcl-shared-node-agent/internal/controlplane"
)

type Workspace interface {
	Path(serviceID string) (string, error)
	Restore(ctx context.Context, command controlplane.Command) (string, error)
	Sync(ctx context.Context, command controlplane.Command) (controlplane.SyncResult, error)
	Release(ctx context.Context, command controlplane.Command) error
}

func (e *Executor) Release(ctx context.Context, command controlplane.Command) error {
	lock := e.serviceLock(command.ServiceID)
	lock.Lock()
	defer lock.Unlock()
	unlock, err := e.locker.Lock(ctx, command.ServiceID)
	if err != nil {
		return err
	}
	defer func() { _ = unlock() }()
	return e.workspace.Release(ctx, command)
}

type ContainerRuntime interface {
	Start(ctx context.Context, command controlplane.Command, workspacePath string) error
	Stop(ctx context.Context, command controlplane.Command) error
}

// OrphanReconciler removes only a managed container whose labels were already
// checked by Executor. It is used to recover safely after an agent crash
// before durable start state was written by an older binary.
type OrphanReconciler interface {
	RemoveOrphan(ctx context.Context, service RunningService) error
}

// RetryableError means the command must remain unacknowledged so its
// lease-based delivery can resume. It is used for object transfer and Docker
// operations, which are not valid terminal command outcomes.
type RetryableError struct{ err error }

func (e *RetryableError) Error() string { return e.err.Error() }
func (e *RetryableError) Unwrap() error { return e.err }

func retryable(err error) error {
	if err == nil {
		return nil
	}
	return &RetryableError{err: err}
}

type RunningService struct {
	ServiceID      string
	AssignmentID   string
	Resources      controlplane.Resources
	CPUPercent     float64
	MemoryUsageMiB int64
}

type RunningServiceProvider interface {
	Running(ctx context.Context) ([]RunningService, error)
}

type Executor struct {
	nodeID    string
	store     StateStore
	workspace Workspace
	runtime   ContainerRuntime
	locks     sync.Map
	locker    ServiceLocker
}

func NewExecutor(nodeID string, store StateStore, workspace Workspace, runtime ContainerRuntime, lockers ...ServiceLocker) *Executor {
	locker := ServiceLocker(&memoryLocker{})
	if len(lockers) > 0 {
		locker = lockers[0]
	}
	return &Executor{nodeID: nodeID, store: store, workspace: workspace, runtime: runtime, locker: locker}
}

func (e *Executor) Execute(ctx context.Context, command controlplane.Command) (controlplane.CommandResult, error) {
	if result, ok, err := e.store.Result(command.CommandID); err != nil {
		return controlplane.CommandResult{}, err
	} else if ok {
		return result, nil
	}

	lock := e.serviceLock(command.ServiceID)
	lock.Lock()
	defer lock.Unlock()
	unlock, err := e.locker.Lock(ctx, command.ServiceID)
	if err != nil {
		return controlplane.CommandResult{}, err
	}
	defer func() { _ = unlock() }()
	if result, ok, err := e.store.Result(command.CommandID); err != nil {
		return controlplane.CommandResult{}, err
	} else if ok {
		return result, nil
	}
	result, active, executeErr := e.execute(ctx, command)
	if executeErr != nil {
		return controlplane.CommandResult{}, executeErr
	}
	if result.Status == "stopped" && active != nil {
		if err := e.store.SetActive(command.ServiceID, *active); err != nil {
			return controlplane.CommandResult{}, err
		}
		return result, nil
	}
	if err := e.store.Commit(command.CommandID, command.ServiceID, result, active); err != nil {
		return controlplane.CommandResult{}, err
	}
	return result, nil
}

func (e *Executor) MarkStoppedReported(serviceID, assignmentID string) error {
	active, exists, err := e.store.Active(serviceID)
	if err != nil {
		return err
	}
	if !exists || active.AssignmentID != assignmentID ||
		active.Phase != "stopped" {
		return errors.New("stopped report does not match active assignment")
	}
	active.StopReported = true
	return e.store.SetActive(serviceID, active)
}

func (e *Executor) TrackLease(commandID string, lease controlplane.CommandLease) error {
	if lease.Token == "" && lease.Generation == 0 {
		return nil
	}
	return e.store.SetLease(commandID, lease)
}

func (e *Executor) ClearLease(commandID string) error {
	return e.store.ClearLease(commandID)
}

// Reconcile refuses to take ownership of a locally running container that is
// absent from, or disagrees with, durable assignment state.
func (e *Executor) Reconcile(ctx context.Context) error {
	provider, ok := e.runtime.(RunningServiceProvider)
	if !ok {
		return nil
	}
	running, err := provider.Running(ctx)
	if err != nil {
		return fmt.Errorf("list locally running containers: %w", err)
	}
	for _, service := range running {
		active, exists, err := e.store.Active(service.ServiceID)
		if err != nil {
			return fmt.Errorf("read active assignment during reconciliation: %w", err)
		}
		if !exists || active.AssignmentID != service.AssignmentID {
			reconciler, ok := e.runtime.(OrphanReconciler)
			if !ok {
				return fmt.Errorf("locally running service %q has no matching durable assignment", service.ServiceID)
			}
			if err := reconciler.RemoveOrphan(ctx, service); err != nil {
				return fmt.Errorf("remove unowned local service %q: %w", service.ServiceID, err)
			}
			continue
		}
		if active.Phase == "starting" {
			active.Phase = "running"
			if err := e.store.SetActive(service.ServiceID, active); err != nil {
				return fmt.Errorf("persist recovered running service %q: %w", service.ServiceID, err)
			}
		}
	}
	return nil
}

func (e *Executor) execute(ctx context.Context, command controlplane.Command) (controlplane.CommandResult, *ActiveAssignment, error) {
	if command.CommandID == "" || command.ServiceID == "" || command.AssignmentID == "" {
		return failed("command ID, service ID, and assignment ID are required"), e.currentActive(command.ServiceID), nil
	}
	if command.NodeID != e.nodeID {
		return failed("command is assigned to a different node"), e.currentActive(command.ServiceID), nil
	}
	active, exists, err := e.store.Active(command.ServiceID)
	if err != nil {
		return controlplane.CommandResult{}, nil, err
	}
	switch command.Kind {
	case controlplane.RestoreAndStart:
		if command.Connection == nil || !command.Connection.Valid() {
			return failed("restore command requires a control-plane assigned public connection"), activePointer(active, exists), nil
		}
		if exists && active.AssignmentID != command.AssignmentID {
			return failed("a different assignment is already active for this service"), &active, nil
		}
		if exists && active.Phase == "running" {
			return controlplane.CommandResult{Status: "started"}, &active, nil
		}
		var path string
		if exists && active.Phase == "starting" {
			path, err = e.workspace.Path(command.ServiceID)
		} else {
			path, err = e.workspace.Restore(ctx, command)
		}
		if err != nil {
			return controlplane.CommandResult{}, activePointer(active, exists), retryable(fmt.Errorf("restore workspace: %w", err))
		}
		starting := ActiveAssignment{AssignmentID: command.AssignmentID, Phase: "starting"}
		if !exists || active.Phase != "starting" {
			if err := e.store.SetActive(command.ServiceID, starting); err != nil {
				return controlplane.CommandResult{}, nil, err
			}
		}
		if err := e.runtime.Start(ctx, command, path); err != nil {
			return controlplane.CommandResult{}, &starting, retryable(fmt.Errorf("start container: %w", err))
		}
		return controlplane.CommandResult{Status: "started"}, &ActiveAssignment{AssignmentID: command.AssignmentID, Phase: "running"}, nil
	case controlplane.StopAndSync:
		if !exists {
			return controlplane.CommandResult{Status: "stopped"}, &ActiveAssignment{
				AssignmentID: command.AssignmentID,
				Phase:        "stopped",
			}, nil
		}
		if active.AssignmentID != command.AssignmentID {
			return failed("stop command does not match an active assignment"), activePointer(active, exists), nil
		}
		if active.Phase != "stopped" {
			if err := e.runtime.Stop(ctx, command); err != nil {
				return controlplane.CommandResult{}, &active, retryable(fmt.Errorf("stop container: %w", err))
			}
			return controlplane.CommandResult{Status: "stopped"}, &ActiveAssignment{
				AssignmentID: command.AssignmentID,
				Phase:        "stopped",
			}, nil
		}
		if !active.StopReported {
			return controlplane.CommandResult{Status: "stopped"}, &active, nil
		}
		syncResult, err := e.workspace.Sync(ctx, command)
		if err != nil {
			return controlplane.CommandResult{}, &active, retryable(fmt.Errorf("sync workspace: %w", err))
		}
		return controlplane.CommandResult{Status: "stopped-and-synced", Sync: &syncResult}, nil, nil
	default:
		return failed("unsupported command kind"), activePointer(active, exists), nil
	}
}

func (e *Executor) currentActive(serviceID string) *ActiveAssignment {
	active, exists, err := e.store.Active(serviceID)
	if err != nil || !exists {
		return nil
	}
	return &active
}

func (e *Executor) serviceLock(serviceID string) *sync.Mutex {
	value, _ := e.locks.LoadOrStore(serviceID, &sync.Mutex{})
	return value.(*sync.Mutex)
}

func activePointer(active ActiveAssignment, exists bool) *ActiveAssignment {
	if !exists {
		return nil
	}
	return &active
}

func failed(message string) controlplane.CommandResult {
	return controlplane.CommandResult{Status: "failed", Message: message}
}
