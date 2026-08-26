package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/voxelum/xmcl-shared-node-agent/internal/agent"
	"github.com/voxelum/xmcl-shared-node-agent/internal/config"
	"github.com/voxelum/xmcl-shared-node-agent/internal/controlplane"
	dockerruntime "github.com/voxelum/xmcl-shared-node-agent/internal/docker"
	"github.com/voxelum/xmcl-shared-node-agent/internal/quota"
	"github.com/voxelum/xmcl-shared-node-agent/internal/telemetry"
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
	telemetryProvider, err := telemetry.New(
		ctx, cfg.OTLPEndpoint, cfg.OTLPHeaders, cfg.NodeID, cfg.Region, version,
	)
	if err != nil {
		fatal(logger, "initialize telemetry", err)
	}
	defer func() {
		shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := telemetryProvider.Shutdown(shutdownContext); err != nil {
			logger.Printf(`{"level":"error","message":"shutdown telemetry","error":%q}`, err.Error())
		}
	}()
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
		HTTPClient: telemetryProvider.HTTPClient(&http.Client{
			Timeout: 45 * time.Second,
		}),
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
	if err := telemetryProvider.RegisterWorkspaceMetrics(func() telemetry.WorkspaceMetrics {
		metrics := workspaceManager.Metrics()
		return telemetry.WorkspaceMetrics{
			LogicalBytes:      metrics.LogicalBytes,
			ActualObjectBytes: metrics.ActualObjectBytes,
			RestoreBytes:      metrics.RestoreBytes,
			SyncBytes:         metrics.SyncBytes,
			RestoreFailures:   metrics.RestoreFailures,
			SyncFailures:      metrics.SyncFailures,
		}
	}); err != nil {
		fatal(logger, "register workspace telemetry", err)
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
		Ready: func() error {
			return writeReadyMarker(cfg.StateRoot, cfg.NodeID)
		},
		Status: func() controlplane.NodeStatus {
			return deriveNodeStatus(ctx, cfg, runtime.Running, version)
		},
	}
	if err := daemon.Run(ctx); err != nil {
		fatal(logger, "agent stopped", err)
	}
}

func writeReadyMarker(stateRoot, nodeID string) error {
	marker := filepath.Join(stateRoot, "bootstrap-ready")
	temporary := marker + ".tmp"
	content, err := json.Marshal(map[string]string{
		"nodeId": nodeID,
		"status": "heartbeat-confirmed",
	})
	if err != nil {
		return fmt.Errorf("encode readiness marker: %w", err)
	}
	if err := os.WriteFile(temporary, append(content, '\n'), 0o600); err != nil {
		return fmt.Errorf("write readiness marker: %w", err)
	}
	if err := os.Rename(temporary, marker); err != nil {
		_ = os.Remove(temporary)
		return fmt.Errorf("publish readiness marker: %w", err)
	}
	return nil
}

func deriveNodeStatus(ctx context.Context, cfg config.Config, running func(context.Context) ([]agent.RunningService, error), agentVersion string) controlplane.NodeStatus {
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
		status.Capacity.FreeWorkspaceGiB = subtractCapacity(status.Capacity.FreeWorkspaceGiB, service.Resources.WorkspaceGiB)
		status.Capacity.AllocatableMemoryMiB = subtractCapacity(status.Capacity.AllocatableMemoryMiB, service.Resources.MemoryMiB)
		status.Capacity.AllocatableSharedCPU = subtractCapacity(status.Capacity.AllocatableSharedCPU, service.Resources.SharedCPU)
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
