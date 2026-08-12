package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/voxelum/xmcl-shared-node-agent/internal/agent"
	"github.com/voxelum/xmcl-shared-node-agent/internal/config"
	"github.com/voxelum/xmcl-shared-node-agent/internal/controlplane"
	dockerruntime "github.com/voxelum/xmcl-shared-node-agent/internal/docker"
	"github.com/voxelum/xmcl-shared-node-agent/internal/quota"
	"github.com/voxelum/xmcl-shared-node-agent/internal/workspace"
)

var version = "dev"

func main() {
	logger := log.New(os.Stdout, "", 0)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cfg, err := config.Load()
	if err != nil {
		fatal(logger, "load configuration", err)
	}
	capacity := controlplane.NodeCapacity{
		TotalMemoryMiB: cfg.TotalMemoryMiB, TotalSharedCPU: cfg.TotalSharedCPU,
		TotalWorkspaceGiB: cfg.TotalWorkspaceGiB,
	}
	controlPlane, err := controlplane.NewClient(controlplane.ClientOptions{
		BaseURL:             cfg.ControlPlaneURL,
		NodeID:              cfg.NodeID,
		Region:              cfg.Region,
		BootstrapCredential: cfg.ControlPlaneCredential,
		CredentialPath:      filepath.Join(cfg.StateRoot, "control-plane-credential"),
	})
	if err != nil {
		fatal(logger, "initialize control-plane client", err)
	}
	runtime, err := dockerruntime.New(cfg.ContainerImage, cfg.RCONStopTimeout)
	if err != nil {
		fatal(logger, "initialize Docker runtime", err)
	}
	if err := runtime.Validate(ctx); err != nil {
		fatal(logger, "validate Docker runtime", err)
	}
	transfer, err := workspace.NewDirectTransfer(
		cfg.ObjectStorageEndpoint,
		cfg.ObjectStorageBucket,
		nil,
	)
	if err != nil {
		fatal(logger, "initialize direct object transfer", err)
	}
	workspaceManager := workspace.New(
		cfg.WorkspaceRoot,
		controlPlane,
		transfer,
		quota.NewHelper(cfg.WorkspaceRoot),
	)
	if err := workspaceManager.Validate(ctx); err != nil {
		fatal(logger, "validate workspace manager", err)
	}
	go serveMetrics(logger, cfg.MetricsAddr, workspaceManager.MetricsHandler())
	state, err := agent.NewFileStore(cfg.StateRoot)
	if err != nil {
		fatal(logger, "initialize command state", err)
	}

	locker, err := agent.NewFileLocker(filepath.Join(cfg.StateRoot, "locks"))
	if err != nil {
		fatal(logger, "initialize service locks", err)
	}
	executor := agent.NewExecutor(cfg.NodeID, state, workspaceManager, runtime, locker)
	if err := executor.Reconcile(ctx); err != nil {
		fatal(logger, "reconcile local containers", err)
	}
	daemon := agent.Daemon{
		NodeID:   cfg.NodeID,
		Capacity: capacity,
		Source:   controlPlane,
		Reporter: controlPlane,
		Executor: executor,
		Status: func() controlplane.NodeStatus {
			return deriveNodeStatus(ctx, cfg, runtime.Running, version)
		},
	}
	if err := daemon.Run(ctx); err != nil {
		fatal(logger, "agent stopped", err)
	}
}

func deriveNodeStatus(
	ctx context.Context,
	cfg config.Config,
	running func(context.Context) ([]agent.RunningService, error),
	agentVersion string,
) controlplane.NodeStatus {
	status := controlplane.NodeStatus{
		ContractVersion: controlplane.SharedNodeContractVersion,
		Status:          "ready",
		Capacity: controlplane.AvailableCapacity{
			FreeWorkspaceGiB:     cfg.TotalWorkspaceGiB,
			AllocatableMemoryMiB: cfg.TotalMemoryMiB,
			AllocatableSharedCPU: cfg.TotalSharedCPU,
		},
		AgentVersion: agentVersion,
		Ingress:      controlplane.Ingress{Host: cfg.IngressHost},
	}
	services, err := running(ctx)
	if err != nil {
		status.Status = "draining"
		status.Capacity = controlplane.AvailableCapacity{}
		return status
	}
	status.Capacity.ActiveContainerCount = int64(len(services))
	for _, service := range services {
		status.Capacity.FreeWorkspaceGiB = subtractCapacity(
			status.Capacity.FreeWorkspaceGiB,
			service.Resources.WorkspaceGiB,
		)
		status.Capacity.AllocatableMemoryMiB = subtractCapacity(
			status.Capacity.AllocatableMemoryMiB,
			service.Resources.MemoryMiB,
		)
		status.Capacity.AllocatableSharedCPU = subtractCapacity(
			status.Capacity.AllocatableSharedCPU,
			service.Resources.SharedCPU,
		)
		status.Services = append(status.Services, controlplane.ServiceStatus{
			ServiceID: service.ServiceID, AssignmentID: service.AssignmentID,
			CPUPercent: service.CPUPercent, MemoryUsageMiB: service.MemoryUsageMiB,
			MemoryLimitMiB: service.Resources.MemoryMiB,
		})
	}
	return status
}

func subtractCapacity(total, allocated int64) int64 {
	if allocated >= total {
		return 0
	}
	return total - allocated
}

func serveMetrics(logger *log.Logger, address string, handler http.Handler) {
	if err := http.ListenAndServe(address, handler); err != nil && err != http.ErrServerClosed {
		fatal(logger, "serve metrics", err)
	}
}

func fatal(logger *log.Logger, message string, err error) {
	entry, marshalErr := json.Marshal(map[string]string{
		"level": "error", "message": message, "error": err.Error(), "version": version,
	})
	if marshalErr != nil {
		logger.Fatal(message)
	}
	logger.Fatal(string(entry))
}
