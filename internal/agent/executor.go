package agent

import (
	"context"
	"fmt"
	"sync"

	"github.com/voxelum/xmcl-shared-node-agent/internal/controlplane"
)

type Workspace interface {
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

type RunningService struct {
	ServiceID    string
	AssignmentID string
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
	result, active := e.execute(ctx, command)
	if err := e.store.Commit(command.CommandID, command.ServiceID, result, active); err != nil {
		return controlplane.CommandResult{}, err
	}
	return result, nil
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
			return fmt.Errorf("locally running service %q has no matching durable assignment", service.ServiceID)
		}
	}
	return nil
}

func (e *Executor) execute(ctx context.Context, command controlplane.Command) (controlplane.CommandResult, *ActiveAssignment) {
	if command.CommandID == "" || command.ServiceID == "" || command.AssignmentID == "" {
		return failed("command ID, service ID, and assignment ID are required"), e.currentActive(command.ServiceID)
	}
	if command.NodeID != e.nodeID {
		return failed("command is assigned to a different node"), e.currentActive(command.ServiceID)
	}
	active, exists, err := e.store.Active(command.ServiceID)
	if err != nil {
		return failed(fmt.Sprintf("read active assignment: %v", err)), nil
	}
	switch command.Kind {
	case controlplane.RestoreAndStart:
		if exists && active.AssignmentID != command.AssignmentID {
			return failed("a different assignment is already active for this service"), &active
		}
		path, err := e.workspace.Restore(ctx, command)
		if err != nil {
			return failed(fmt.Sprintf("restore workspace: %v", err)), activePointer(active, exists)
		}
		if err := e.runtime.Start(ctx, command, path); err != nil {
			return failed(fmt.Sprintf("start container: %v", err)), activePointer(active, exists)
		}
		return controlplane.CommandResult{Status: "started"}, &ActiveAssignment{AssignmentID: command.AssignmentID}
	case controlplane.StopAndSync:
		if !exists || active.AssignmentID != command.AssignmentID {
			return failed("stop command does not match an active assignment"), activePointer(active, exists)
		}
		if err := e.runtime.Stop(ctx, command); err != nil {
			return failed(fmt.Sprintf("stop container: %v", err)), &active
		}
		syncResult, err := e.workspace.Sync(ctx, command)
		if err != nil {
			return failed(fmt.Sprintf("sync workspace: %v", err)), &active
		}
		return controlplane.CommandResult{Status: "stopped-and-synced", Sync: &syncResult}, nil
	default:
		return failed("unsupported command kind"), activePointer(active, exists)
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
