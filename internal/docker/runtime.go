package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/docker/docker/errdefs"
	"github.com/docker/go-connections/nat"
	"github.com/voxelum/xmcl-shared-node-agent/internal/agent"
	"github.com/voxelum/xmcl-shared-node-agent/internal/controlplane"
	runtimecontract "github.com/voxelum/xmcl-shared-node-agent/internal/runtime"
)

const cpuPeriod int64 = 100000
const privateNetwork = "xmcl-shared-private"
const masqueradeOption = "com.docker.network.bridge.enable_ip_masquerade"
const runtimeCatalogLabel = "io.xmcl.runtime-catalog-sha256"

const (
	resourceMemoryLabel    = "xmcl.memory-mib"
	resourceSharedCPULabel = "xmcl.shared-cpu"
	resourceBurstCPULabel  = "xmcl.burst-cpu"
	resourceWorkspaceLabel = "xmcl.workspace-gib"
)

type Runtime struct {
	client      *client.Client
	image       string
	dockerImage string
	stopTimeout time.Duration
	healthWait  time.Duration
}

func New(image string, stopTimeout time.Duration) (*Runtime, error) {
	if err := ValidateRuntimeImage(image); err != nil {
		return nil, err
	}
	api, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("create Docker client: %w", err)
	}
	return &Runtime{
		client: api, image: image, dockerImage: image,
		stopTimeout: stopTimeout, healthWait: 2 * time.Minute,
	}, nil
}

func (r *Runtime) Validate(ctx context.Context) error {
	if _, err := r.client.Ping(ctx); err != nil {
		return fmt.Errorf("ping Docker daemon: %w", err)
	}
	image, _, err := r.client.ImageInspectWithRaw(ctx, r.dockerImage)
	if err != nil {
		return fmt.Errorf("inspect configured container image: %w", err)
	}
	if err := ValidateRuntimeCatalogLabels(image.Config.Labels); err != nil {
		return err
	}
	configuredNetwork, err := r.client.NetworkInspect(ctx, privateNetwork, network.InspectOptions{})
	if err != nil {
		if !errdefs.IsNotFound(err) {
			return fmt.Errorf("inspect private container network: %w", err)
		}
		if _, err := r.client.NetworkCreate(ctx, privateNetwork, network.CreateOptions{
			Driver: "bridge",
			Options: map[string]string{
				masqueradeOption: "false",
			},
		}); err != nil {
			return fmt.Errorf("create private container network: %w", err)
		}
	} else if err := validatePrivateNetwork(configuredNetwork); err != nil {
		return err
	}
	return nil
}

func validatePrivateNetwork(configured network.Inspect) error {
	if configured.Internal || configured.Options[masqueradeOption] != "false" {
		return errors.New("private container network must permit published ingress without outbound masquerading")
	}
	return nil
}

func ValidateRuntimeCatalogLabels(labels map[string]string) error {
	if labels[runtimeCatalogLabel] != runtimecontract.CatalogSHA256() {
		return errors.New("configured container image runtime catalog does not match the agent")
	}
	return nil
}

func (r *Runtime) Running(ctx context.Context) ([]agent.RunningService, error) {
	containers, err := r.client.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: filters.NewArgs(filters.Arg("label", "xmcl.managed=true")),
	})
	if err != nil {
		return nil, fmt.Errorf("list managed containers: %w", err)
	}
	running := make([]agent.RunningService, 0, len(containers))
	for _, summary := range containers {
		if summary.State != "running" {
			continue
		}
		serviceID, assignmentID := summary.Labels["xmcl.service-id"], summary.Labels["xmcl.assignment-id"]
		if serviceID == "" || assignmentID == "" {
			return nil, fmt.Errorf("managed container %q is missing ownership labels", summary.ID)
		}
		inspect, err := r.client.ContainerInspect(ctx, summary.ID)
		if err != nil {
			return nil, fmt.Errorf("inspect managed container %q: %w", summary.ID, err)
		}
		if inspect.HostConfig.Privileged || hasDockerSocketMount(inspect) {
			return nil, fmt.Errorf("managed container %q violates runtime isolation", summary.ID)
		}
		resources, err := resourcesFromLabels(summary.Labels)
		if err != nil {
			return nil, fmt.Errorf("managed container %q has invalid capacity labels: %w", summary.ID, err)
		}
		cpuPercent, memoryUsageMiB, err := r.containerMetrics(ctx, summary.ID, resources.MemoryMiB)
		if err != nil {
			return nil, fmt.Errorf("read managed container %q metrics: %w", summary.ID, err)
		}
		running = append(running, agent.RunningService{
			ServiceID: serviceID, AssignmentID: assignmentID, Resources: resources,
			CPUPercent: cpuPercent, MemoryUsageMiB: memoryUsageMiB,
		})
	}
	return running, nil
}

func (r *Runtime) containerMetrics(ctx context.Context, containerID string, memoryLimitMiB int64) (float64, int64, error) {
	response, err := r.client.ContainerStatsOneShot(ctx, containerID)
	if err != nil {
		return 0, 0, err
	}
	defer response.Body.Close()
	var value struct {
		CPUStats struct {
			CPUUsage struct {
				TotalUsage  uint64   `json:"total_usage"`
				PercpuUsage []uint64 `json:"percpu_usage"`
			} `json:"cpu_usage"`
			SystemUsage uint64 `json:"system_cpu_usage"`
			OnlineCPUs  uint32 `json:"online_cpus"`
		} `json:"cpu_stats"`
		PreCPUStats struct {
			CPUUsage struct {
				TotalUsage uint64 `json:"total_usage"`
			} `json:"cpu_usage"`
			SystemUsage uint64 `json:"system_cpu_usage"`
		} `json:"precpu_stats"`
		MemoryStats struct {
			Usage uint64            `json:"usage"`
			Stats map[string]uint64 `json:"stats"`
		} `json:"memory_stats"`
	}
	if err := json.NewDecoder(response.Body).Decode(&value); err != nil {
		return 0, 0, err
	}
	cpuPercent := 0.0
	onlineCPUs := value.CPUStats.OnlineCPUs
	if onlineCPUs == 0 {
		onlineCPUs = uint32(len(value.CPUStats.CPUUsage.PercpuUsage))
	}
	if value.CPUStats.CPUUsage.TotalUsage >= value.PreCPUStats.CPUUsage.TotalUsage &&
		value.CPUStats.SystemUsage >= value.PreCPUStats.SystemUsage {
		cpuDelta := value.CPUStats.CPUUsage.TotalUsage - value.PreCPUStats.CPUUsage.TotalUsage
		systemDelta := value.CPUStats.SystemUsage - value.PreCPUStats.SystemUsage
		if cpuDelta > 0 && systemDelta > 0 && onlineCPUs > 0 {
			cpuPercent = float64(cpuDelta) / float64(systemDelta) * float64(onlineCPUs) * 100
		}
	}
	usage := value.MemoryStats.Usage
	if cache := value.MemoryStats.Stats["inactive_file"]; cache < usage {
		usage -= cache
	}
	const mebibyte = 1024 * 1024
	memoryUsageMiB := int64((usage + mebibyte - 1) / mebibyte)
	if memoryUsageMiB > memoryLimitMiB {
		memoryUsageMiB = memoryLimitMiB
	}
	return cpuPercent, memoryUsageMiB, nil
}

func (r *Runtime) Start(ctx context.Context, command controlplane.Command, workspacePath string) error {
	if command.RuntimeContent == nil {
		return errors.New("shared modded runtime start requires compiler-selected runtime content")
	}
	if _, err := runtimecontract.ValidateWorkspace(workspacePath, command.RuntimeContent.SHA256); err != nil {
		return fmt.Errorf("validate compiler runtime descriptor: %w", err)
	}
	name, err := containerName(command.ServiceID)
	if err != nil {
		return err
	}
	inspect, err := r.client.ContainerInspect(ctx, name)
	if err == nil {
		if inspect.State.Running {
			if inspect.Config.Labels["xmcl.assignment-id"] != command.AssignmentID {
				return errors.New("existing running container belongs to a different assignment")
			}
			return r.waitHealthy(ctx, name)
		}
		// A successful stop deliberately leaves the container for its workspace
		// sync. Remove any inactive managed predecessor before a new assignment,
		// including one with an older assignment label.
		if err := r.client.ContainerRemove(ctx, name, container.RemoveOptions{Force: true}); err != nil {
			return fmt.Errorf("remove inactive managed container: %w", err)
		}
	} else if !errdefs.IsNotFound(err) {
		return fmt.Errorf("inspect existing container: %w", err)
	}

	config, hostConfig, err := BuildCreateRequest(command, workspacePath, r.image)
	if err != nil {
		return err
	}
	config.Image = r.dockerImage
	created, err := r.client.ContainerCreate(ctx, config, hostConfig, nil, nil, name)
	if err != nil {
		return fmt.Errorf("create container: %w", err)
	}
	if err := r.client.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		_ = r.client.ContainerRemove(context.Background(), created.ID, container.RemoveOptions{Force: true})
		return fmt.Errorf("start container: %w", err)
	}
	if err := r.waitHealthy(ctx, created.ID); err != nil {
		_ = r.client.ContainerRemove(context.Background(), created.ID, container.RemoveOptions{Force: true})
		return err
	}
	return nil
}

// RemoveOrphan is called only after Executor has compared managed labels with
// durable state. It refuses to remove a differently named/unmanaged container.
func (r *Runtime) RemoveOrphan(ctx context.Context, service agent.RunningService) error {
	name, err := containerName(service.ServiceID)
	if err != nil {
		return err
	}
	inspect, err := r.client.ContainerInspect(ctx, name)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("inspect orphan container: %w", err)
	}
	if inspect.Config.Labels["xmcl.managed"] != "true" ||
		inspect.Config.Labels["xmcl.service-id"] != service.ServiceID ||
		inspect.Config.Labels["xmcl.assignment-id"] != service.AssignmentID {
		return errors.New("orphan container ownership labels do not match")
	}
	if err := r.client.ContainerRemove(ctx, name, container.RemoveOptions{Force: true}); err != nil {
		return fmt.Errorf("remove orphan managed container: %w", err)
	}
	return nil
}

func (r *Runtime) Stop(ctx context.Context, command controlplane.Command) error {
	name, err := containerName(command.ServiceID)
	if err != nil {
		return err
	}
	inspect, err := r.client.ContainerInspect(ctx, name)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("inspect container before stop: %w", err)
	}
	if inspect.Config.Labels["xmcl.assignment-id"] != command.AssignmentID {
		return errors.New("container assignment does not match stop command")
	}
	if inspect.State.Running {
		timeoutSeconds := int(r.stopTimeout.Round(time.Second))
		if err := r.client.ContainerStop(ctx, name, container.StopOptions{Timeout: &timeoutSeconds}); err != nil {
			return fmt.Errorf("gracefully stop Minecraft container: %w", err)
		}
	}
	deadline := time.Now().Add(r.stopTimeout + 10*time.Second)
	for {
		inspect, err = r.client.ContainerInspect(ctx, name)
		if err != nil {
			return fmt.Errorf("inspect container after stop: %w", err)
		}
		if !inspect.State.Running {
			return nil
		}
		if time.Now().After(deadline) {
			if err := r.client.ContainerKill(ctx, name, "KILL"); err != nil {
				return fmt.Errorf("kill unresponsive Minecraft container: %w", err)
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// ValidateRuntimeImage prevents an operator or API caller from replacing the
// generic launcher with a mutable tag or a dynamically downloading image.
func ValidateRuntimeImage(image string) error {
	if !regexp.MustCompile(`^ghcr\.io/voxelum/xmcl-shared-minecraft-runtime@sha256:[a-f0-9]{64}$`).MatchString(image) {
		return errors.New("container image must be the immutable XMCL shared Minecraft runtime digest")
	}
	return nil
}

func BuildCreateRequest(command controlplane.Command, workspacePath, image string) (*container.Config, *container.HostConfig, error) {
	if err := ValidateRuntimeImage(image); err != nil {
		return nil, nil, err
	}
	if command.Resources.MemoryMiB < 1 || command.Resources.SharedCPU < 1 ||
		command.Resources.BurstCPU < command.Resources.SharedCPU || command.Resources.WorkspaceGiB < 1 {
		return nil, nil, errors.New("invalid container resource limits")
	}
	if filepath.IsAbs(workspacePath) == false {
		return nil, nil, errors.New("workspace mount source must be an absolute path")
	}
	if command.RuntimeContent == nil || command.RuntimeContent.SHA256 == "" ||
		command.RuntimeContent.Key == "" || len(command.RuntimeContent.Paths) == 0 {
		return nil, nil, errors.New("container start requires compiler-selected immutable runtime content")
	}
	if !command.EULAAccepted {
		return nil, nil, errors.New("container start requires server-side EULA acceptance")
	}
	memory := command.Resources.MemoryMiB * 1024 * 1024
	pids := int64(256)
	gamePort := nat.Port("25565/tcp")
	if command.Connection == nil || !command.Connection.Valid() {
		return nil, nil, errors.New("container start requires a control-plane assigned public connection")
	}

	config := &container.Config{
		Image: image,
		User:  "1000:1000",
		Env:   fixedRuntimeEnvironment(command),
		ExposedPorts: nat.PortSet{
			gamePort: struct{}{},
		},
		Labels: map[string]string{
			"xmcl.service-id":      command.ServiceID,
			"xmcl.assignment-id":   command.AssignmentID,
			"xmcl.managed":         "true",
			resourceMemoryLabel:    strconv.FormatInt(command.Resources.MemoryMiB, 10),
			resourceSharedCPULabel: strconv.FormatInt(command.Resources.SharedCPU, 10),
			resourceBurstCPULabel:  strconv.FormatInt(command.Resources.BurstCPU, 10),
			resourceWorkspaceLabel: strconv.FormatInt(command.Resources.WorkspaceGiB, 10),
		},
	}

	hostConfig := &container.HostConfig{
		Resources: container.Resources{
			Memory:     memory,
			MemorySwap: memory,
			CPUPeriod:  cpuPeriod,
			CPUQuota:   command.Resources.BurstCPU * cpuPeriod,
			CPUShares:  command.Resources.SharedCPU * 1024,
			PidsLimit:  &pids,
		},
		ReadonlyRootfs: true,
		Privileged:     false,
		SecurityOpt:    []string{"no-new-privileges:true"},
		CapDrop:        []string{"ALL"},
		NetworkMode:    container.NetworkMode(privateNetwork),
		PortBindings: nat.PortMap{
			gamePort: []nat.PortBinding{{HostIP: "0.0.0.0", HostPort: strconv.Itoa(command.Connection.HostPort)}},
		},
		Mounts: []mount.Mount{{
			Type:        mount.TypeBind,
			Source:      workspacePath,
			Target:      "/data",
			ReadOnly:    false,
			BindOptions: &mount.BindOptions{NonRecursive: true},
		}},
	}

	return config, hostConfig, nil
}

func fixedRuntimeEnvironment(command controlplane.Command) []string {
	if command.EULAAccepted {
		return []string{"XMCL_EULA_ACCEPTED=true"}
	}
	return nil
}

func resourcesFromLabels(labels map[string]string) (controlplane.Resources, error) {
	parse := func(label string) (int64, error) {
		value, err := strconv.ParseInt(labels[label], 10, 64)
		if err != nil || value < 1 {
			return 0, fmt.Errorf("%s must be a positive integer", label)
		}
		return value, nil
	}
	memory, err := parse(resourceMemoryLabel)
	if err != nil {
		return controlplane.Resources{}, err
	}
	sharedCPU, err := parse(resourceSharedCPULabel)
	if err != nil {
		return controlplane.Resources{}, err
	}
	burstCPU, err := parse(resourceBurstCPULabel)
	if err != nil {
		return controlplane.Resources{}, err
	}
	workspace, err := parse(resourceWorkspaceLabel)
	if err != nil {
		return controlplane.Resources{}, err
	}
	if burstCPU < sharedCPU {
		return controlplane.Resources{}, errors.New("burst CPU is below shared CPU")
	}
	return controlplane.Resources{
		MemoryMiB: memory, SharedCPU: sharedCPU, BurstCPU: burstCPU, WorkspaceGiB: workspace,
	}, nil
}

func (r *Runtime) waitHealthy(ctx context.Context, id string) error {
	deadline := time.Now().Add(r.healthWait)
	for {
		inspect, err := r.client.ContainerInspect(ctx, id)
		if err != nil {
			return fmt.Errorf("inspect container health: %w", err)
		}
		if !inspect.State.Running {
			return errors.New("Minecraft container exited before becoming healthy")
		}
		if inspect.State.Health == nil {
			return errors.New("container image must define a Docker health check")
		}
		switch inspect.State.Health.Status {
		case "healthy":
			return nil
		case "unhealthy":
			return errors.New("Minecraft container health check failed")
		}
		if time.Now().After(deadline) {
			return errors.New("timed out waiting for Minecraft container health check")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

func containerName(serviceID string) (string, error) {
	if serviceID == "" || strings.ContainsAny(serviceID, `/\`) {
		return "", errors.New("invalid service ID for container name")
	}
	return "xmcl-shared-" + serviceID, nil
}

func hasDockerSocketMount(inspect types.ContainerJSON) bool {
	for _, bind := range inspect.HostConfig.Binds {
		if strings.HasPrefix(bind, "/var/run/docker.sock:") {
			return true
		}
	}
	for _, mounted := range inspect.Mounts {
		if mounted.Source == "/var/run/docker.sock" {
			return true
		}
	}
	return false
}

var _ = types.ContainerJSON{}
