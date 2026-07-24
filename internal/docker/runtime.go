package docker

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/client"
	"github.com/docker/docker/errdefs"
	"github.com/voxelum/xmcl-shared-node-agent/internal/agent"
	"github.com/voxelum/xmcl-shared-node-agent/internal/controlplane"
)

const cpuPeriod int64 = 100000

type Runtime struct {
	client      *client.Client
	image       string
	stopTimeout time.Duration
	healthWait  time.Duration
}

func New(image string, stopTimeout time.Duration) (*Runtime, error) {
	api, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("create Docker client: %w", err)
	}
	return &Runtime{client: api, image: image, stopTimeout: stopTimeout, healthWait: 2 * time.Minute}, nil
}

func (r *Runtime) Validate(ctx context.Context) error {
	if _, err := r.client.Ping(ctx); err != nil {
		return fmt.Errorf("ping Docker daemon: %w", err)
	}
	if _, _, err := r.client.ImageInspectWithRaw(ctx, r.image); err != nil {
		return fmt.Errorf("inspect configured container image: %w", err)
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
		running = append(running, agent.RunningService{ServiceID: serviceID, AssignmentID: assignmentID})
	}
	return running, nil
}

func (r *Runtime) Start(ctx context.Context, command controlplane.Command, workspacePath string) error {
	name, err := containerName(command.ServiceID)
	if err != nil {
		return err
	}
	inspect, err := r.client.ContainerInspect(ctx, name)
	if err == nil {
		if inspect.Config.Labels["xmcl.assignment-id"] != command.AssignmentID {
			return errors.New("existing container belongs to a different assignment")
		}
		if inspect.State.Running {
			return r.waitHealthy(ctx, name)
		}
		if err := r.client.ContainerRemove(ctx, name, container.RemoveOptions{Force: true}); err != nil {
			return fmt.Errorf("remove stopped assigned container: %w", err)
		}
	} else if !errdefs.IsNotFound(err) {
		return fmt.Errorf("inspect existing container: %w", err)
	}
	config, hostConfig, err := BuildCreateRequest(command, workspacePath, r.image)
	if err != nil {
		return err
	}
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

func BuildCreateRequest(command controlplane.Command, workspacePath, image string) (*container.Config, *container.HostConfig, error) {
	if command.Resources.MemoryMiB < 1 || command.Resources.SharedCPU < 1 ||
		command.Resources.BurstCPU < command.Resources.SharedCPU || command.Resources.WorkspaceGiB < 1 {
		return nil, nil, errors.New("invalid container resource limits")
	}
	if filepath.IsAbs(workspacePath) == false {
		return nil, nil, errors.New("workspace mount source must be an absolute path")
	}
	memory := command.Resources.MemoryMiB * 1024 * 1024
	pids := int64(256)
	config := &container.Config{
		Image: image,
		User:  "1000:1000",
		Labels: map[string]string{
			"xmcl.service-id":    command.ServiceID,
			"xmcl.assignment-id": command.AssignmentID,
			"xmcl.managed":       "true",
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
		Mounts: []mount.Mount{{
			Type: mount.TypeBind, Source: workspacePath, Target: "/data", ReadOnly: false,
		}},
	}
	return config, hostConfig, nil
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
