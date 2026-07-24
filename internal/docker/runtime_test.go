package docker

import (
	"path/filepath"
	"testing"

	"github.com/voxelum/xmcl-shared-node-agent/internal/controlplane"
)

func TestBuildCreateRequestHardensContainer(t *testing.T) {
	command := controlplane.Command{
		ServiceID: "service_1", AssignmentID: "assignment_1",
		Resources:  controlplane.Resources{MemoryMiB: 512, SharedCPU: 1, BurstCPU: 2, WorkspaceGiB: 4},
		Connection: &controlplane.Connection{Host: "public-node.example", HostPort: 25572},
	}
	config, host, err := BuildCreateRequest(command, filepath.Join(t.TempDir(), "service_1"), "minecraft:test")
	if err != nil {
		t.Fatal(err)
	}
	if config.User != "1000:1000" || !host.ReadonlyRootfs || host.Privileged || host.Resources.Memory != 512*1024*1024 {
		t.Fatalf("unexpected hardened container configuration: %#v %#v", config, host)
	}
	if host.Resources.MemorySwap != host.Resources.Memory || host.Resources.CPUQuota != 2*cpuPeriod || len(host.Mounts) != 1 ||
		host.Mounts[0].Target != "/data" || host.Mounts[0].Source == "/var/run/docker.sock" {
		t.Fatalf("unexpected resource or mount configuration: %#v", host)
	}
	if len(host.SecurityOpt) != 1 || host.SecurityOpt[0] != "no-new-privileges:true" || host.Resources.PidsLimit == nil {
		t.Fatalf("security settings missing: %#v", host)
	}
	if config.Labels[resourceMemoryLabel] != "512" || config.Labels[resourceSharedCPULabel] != "1" ||
		config.Labels[resourceWorkspaceLabel] != "4" {
		t.Fatalf("capacity labels = %#v", config.Labels)
	}
	if host.NetworkMode != privateNetwork || len(host.PortBindings) != 1 || config.ExposedPorts["25565/tcp"] != struct{}{} {
		t.Fatalf("Minecraft ingress is not configured: %#v %#v", config.ExposedPorts, host.PortBindings)
	}
	binding := host.PortBindings["25565/tcp"]
	if len(binding) != 1 || binding[0].HostPort != "25572" {
		t.Fatalf("host port = %#v, want the assigned port", binding)
	}
}

func TestResourcesFromLabelsRejectsMissingCapacity(t *testing.T) {
	_, err := resourcesFromLabels(map[string]string{resourceMemoryLabel: "512"})
	if err == nil {
		t.Fatal("missing capacity labels were accepted")
	}
}

func TestBuildCreateRequestRejectsMissingAssignedConnection(t *testing.T) {
	command := controlplane.Command{
		ServiceID: "service_1", AssignmentID: "assignment_1",
		Resources: controlplane.Resources{MemoryMiB: 512, SharedCPU: 1, BurstCPU: 2, WorkspaceGiB: 4},
	}
	if _, _, err := BuildCreateRequest(command, filepath.Join(t.TempDir(), "service_1"), "minecraft:test"); err == nil {
		t.Fatal("missing assigned public connection was accepted")
	}
}
