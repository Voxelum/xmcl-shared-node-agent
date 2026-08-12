package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/voxelum/xmcl-shared-node-agent/internal/agent"
	"github.com/voxelum/xmcl-shared-node-agent/internal/config"
	"github.com/voxelum/xmcl-shared-node-agent/internal/controlplane"
)

func TestWriteReadyMarkerPublishesConfirmedNode(t *testing.T) {
	root := t.TempDir()
	if err := writeReadyMarker(root, "node_1"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "bootstrap-ready"))
	if err != nil {
		t.Fatal(err)
	}
	var marker map[string]string
	if err := json.Unmarshal(data, &marker); err != nil {
		t.Fatal(err)
	}
	if marker["nodeId"] != "node_1" || marker["status"] != "heartbeat-confirmed" {
		t.Fatalf("marker = %#v", marker)
	}
}

func TestDeriveNodeStatusSubtractsManagedCapacity(t *testing.T) {
	status := deriveNodeStatus(context.Background(), config.Config{
		TotalMemoryMiB: 4096, TotalSharedCPU: 8, TotalWorkspaceGiB: 100, IngressHost: "198.51.100.10",
	}, func(context.Context) ([]agent.RunningService, error) {
		return []agent.RunningService{{
			Resources: controlplane.Resources{MemoryMiB: 1024, SharedCPU: 2, WorkspaceGiB: 20},
		}}, nil
	}, "test-agent")

	if !status.Valid() {
		t.Fatalf("invalid heartbeat status: %#v", status)
	}
	if status.Status != "ready" || status.Capacity != (controlplane.AvailableCapacity{
		FreeWorkspaceGiB: 80, AllocatableMemoryMiB: 3072, AllocatableSharedCPU: 6, ActiveContainerCount: 1,
	}) {
		t.Fatalf("status = %#v", status)
	}
}

func TestDeriveNodeStatusDrainsWhenRuntimeCannotReportCapacity(t *testing.T) {
	status := deriveNodeStatus(context.Background(), config.Config{IngressHost: "public-node.example"},
		func(context.Context) ([]agent.RunningService, error) {
			return nil, errors.New("Docker unavailable")
		}, "test-agent")

	if !status.Valid() || status.Status != "draining" || status.Capacity != (controlplane.AvailableCapacity{}) {
		t.Fatalf("status = %#v", status)
	}
}
