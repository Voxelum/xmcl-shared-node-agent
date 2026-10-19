package docker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/client"
	"github.com/docker/docker/errdefs"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/voxelum/xmcl-shared-node-agent/internal/builder"
	"github.com/voxelum/xmcl-shared-node-agent/internal/controlplane"
)

const (
	builderSchemaLabel  = "io.xmcl.builder.schema"
	builderCatalogLabel = "io.xmcl.builder.toolchain-catalog-revision"
	builderRuntimeLabel = "io.xmcl.builder.runtime-catalog-revision"
	builderTargetLabel  = "io.xmcl.builder.target"
	builderMemoryBytes  = int64(4 * 1024 * 1024 * 1024)
	builderCPUQuota     = int64(200000)
	builderTimeout      = 15 * time.Minute
)

type Builder struct {
	client   *client.Client
	image    string
	identity builder.Identity
	timeout  time.Duration
}

type boundedWriter struct {
	writer io.Writer
	left   int
}

func (w *boundedWriter) Write(data []byte) (int, error) {
	original := len(data)
	if len(data) > w.left {
		data = data[:w.left]
	}
	if len(data) > 0 {
		if _, err := w.writer.Write(data); err != nil {
			return 0, err
		}
		w.left -= len(data)
	}
	return original, nil
}

func NewBuilder(identity builder.Identity) (*Builder, error) {
	api, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("create Docker client for builder: %w", err)
	}
	return &Builder{client: api, image: identity.BuilderImage, identity: identity, timeout: builderTimeout}, nil
}

func (b *Builder) Validate(ctx context.Context) error {
	if _, err := b.client.Ping(ctx); err != nil {
		return fmt.Errorf("ping Docker daemon for builder: %w", err)
	}
	image, _, err := b.client.ImageInspectWithRaw(ctx, b.image)
	if err != nil {
		return fmt.Errorf("inspect configured builder image: %w", err)
	}
	labels := image.Config.Labels
	if labels[builderSchemaLabel] != builder.Schema ||
		labels[builderCatalogLabel] != b.identity.ToolchainCatalogRevision ||
		labels[builderRuntimeLabel] != b.identity.RuntimeCatalogRevision ||
		labels[builderTargetLabel] != "linux-amd64" {
		return errors.New("configured builder image labels do not match the reviewed release")
	}
	return nil
}

func (b *Builder) Reconcile(ctx context.Context) error {
	containers, err := b.client.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: filters.NewArgs(filters.Arg("label", "xmcl.workload=builder")),
	})
	if err != nil {
		return fmt.Errorf("list stale builder containers: %w", err)
	}
	for _, stale := range containers {
		if err := b.client.ContainerRemove(ctx, stale.ID, container.RemoveOptions{Force: true}); err != nil && !errdefs.IsNotFound(err) {
			return fmt.Errorf("remove stale builder container: %w", err)
		}
	}
	return nil
}

func (b *Builder) Run(ctx context.Context, command controlplane.Command, stagingRoot string) error {
	if command.Prepare == nil || command.Prepare.BuildSpec.Builder.Image != b.image {
		return &builder.Failure{Code: "builder_identity_mismatch", Class: "terminal", Err: errors.New("builder image differs from command specification")}
	}
	config, hostConfig, err := BuildBuilderCreateRequest(command, stagingRoot, b.image)
	if err != nil {
		return &builder.Failure{Code: "invalid_prepare_command", Class: "terminal", Err: err}
	}
	name := builderContainerName(command.CommandID)
	inspect, inspectErr := b.client.ContainerInspect(ctx, name)
	if inspectErr == nil {
		if inspect.State.Running {
			if removeErr := b.client.ContainerRemove(ctx, name, container.RemoveOptions{Force: true}); removeErr != nil {
				return &builder.Failure{Code: "docker_unavailable", Class: "retryable", Err: removeErr}
			}
		} else if removeErr := b.client.ContainerRemove(ctx, name, container.RemoveOptions{Force: true}); removeErr != nil {
			return &builder.Failure{Code: "docker_unavailable", Class: "retryable", Err: removeErr}
		}
	} else if !errdefs.IsNotFound(inspectErr) {
		return &builder.Failure{Code: "docker_unavailable", Class: "retryable", Err: inspectErr}
	}
	created, err := b.client.ContainerCreate(ctx, config, hostConfig, nil, nil, name)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return &builder.Failure{Code: "builder_image_unavailable", Class: "retryable", Err: fmt.Errorf("configured builder image is unavailable: %w", err)}
		}
		return &builder.Failure{Code: "docker_unavailable", Class: "retryable", Err: fmt.Errorf("create builder container: %w", err)}
	}
	defer b.client.ContainerRemove(context.Background(), created.ID, container.RemoveOptions{Force: true})
	if err := b.client.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		return &builder.Failure{Code: "docker_unavailable", Class: "retryable", Err: fmt.Errorf("start builder container: %w", err)}
	}
	waitContext, cancel := context.WithTimeout(ctx, b.timeout)
	defer cancel()
	status, waitErr := b.client.ContainerWait(waitContext, created.ID, container.WaitConditionNotRunning)
	select {
	case <-waitContext.Done():
		_ = b.client.ContainerKill(context.Background(), created.ID, "KILL")
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &builder.Failure{Code: "builder_timeout", Class: "retryable", Err: errors.New("builder exceeded fixed wall timeout")}
	case err := <-waitErr:
		if err != nil {
			return &builder.Failure{Code: "docker_unavailable", Class: "retryable", Err: err}
		}
	case result := <-status:
		if result.StatusCode != 0 {
			inspect, _ := b.client.ContainerInspect(ctx, created.ID)
			if inspect.State != nil && inspect.State.OOMKilled {
				return &builder.Failure{Code: "builder_resource_exhausted", Class: "retryable", Err: errors.New("builder exhausted fixed memory limit")}
			}
			code := b.failureCode(ctx, created.ID)
			class, ok := builder.FailureClass(code)
			if !ok {
				code, class = "installer_failed", "terminal"
			}
			return &builder.Failure{Code: code, Class: class, Err: fmt.Errorf("builder exited with status %d", result.StatusCode)}
		}
	}
	return nil
}

func (b *Builder) failureCode(ctx context.Context, containerID string) string {
	logs, err := b.client.ContainerLogs(ctx, containerID, container.LogsOptions{
		ShowStderr: true, Tail: "8",
	})
	if err != nil {
		return "installer_failed"
	}
	defer logs.Close()
	var stderr bytes.Buffer
	if _, err := stdcopy.StdCopy(io.Discard, &boundedWriter{writer: &stderr, left: 4096}, logs); err != nil {
		return "installer_failed"
	}
	const marker = "agent_builder_v1 failed: "
	for _, line := range strings.Split(stderr.String(), "\n") {
		if strings.HasPrefix(line, marker) {
			return classifyBuilderCode(strings.TrimPrefix(line, marker))
		}
	}
	return "installer_failed"
}

func classifyBuilderCode(code string) string {
	switch code {
	case "invalid_build_spec":
		return "invalid_build_spec"
	case "unsupported_compatibility":
		return "unsupported_compatibility"
	case "source_digest_mismatch":
		return "source_hash_mismatch"
	case "invalid_mrpack", "mrpack_dependency_mismatch", "mrpack_compatibility_mismatch",
		"source_file_mismatch", "invalid_bundle", "bundle_too_large":
		return "source_pack_invalid"
	case "mounted_dependency_identity_mismatch", "mounted_dependency_mismatch",
		"mounted_dependency_invalid", "invalid_reviewed_artifact":
		return "dependency_hash_mismatch"
	case "installer_timeout":
		return "builder_timeout"
	case "invalid_builder_output", "invalid_installer_output", "invalid_local_content",
		"output_write_failed", "output_not_empty":
		return "builder_output_invalid"
	case "catalog_revision_mismatch", "invalid_builder_configuration", "invalid_builder_input",
		"invalid_builder_invocation":
		return "builder_identity_mismatch"
	case "installer_failed", "installer_spawn_failed", "installer_process_failed":
		return "installer_failed"
	default:
		return "installer_failed"
	}
}

func BuildBuilderCreateRequest(command controlplane.Command, stagingRoot, image string) (*container.Config, *container.HostConfig, error) {
	if command.Prepare == nil || command.CommandID == "" || command.Attempt < 1 || command.Attempt > 3 {
		return nil, nil, errors.New("invalid builder command")
	}
	if command.Prepare.BuildSpec.Builder.Image != image {
		return nil, nil, errors.New("builder image does not match build specification")
	}
	absolute, err := filepath.Abs(stagingRoot)
	if err != nil || absolute != stagingRoot {
		return nil, nil, errors.New("builder staging root must be absolute")
	}
	pids := int64(256)
	config := &container.Config{
		Image:      image,
		User:       "10001:10001",
		Env:        []string{},
		Entrypoint: []string{"/usr/local/bin/node", "/opt/xmcl-builder/app/scripts/agent_builder_v1.mjs"},
		Cmd:        []string{},
		Labels: map[string]string{
			"xmcl.workload":   "builder",
			"xmcl.command-id": command.CommandID,
			"xmcl.build-key":  command.Prepare.ExpectedBuildKey,
			"xmcl.attempt":    strconv.Itoa(command.Attempt),
		},
	}
	host := &container.HostConfig{
		Resources: container.Resources{
			Memory: builderMemoryBytes, MemorySwap: builderMemoryBytes,
			CPUPeriod: cpuPeriod, CPUQuota: builderCPUQuota, PidsLimit: &pids,
		},
		ReadonlyRootfs: true,
		Privileged:     false,
		NetworkMode:    "none",
		SecurityOpt:    []string{"no-new-privileges:true"},
		CapDrop:        []string{"ALL"},
		Tmpfs: map[string]string{
			"/tmp": "rw,noexec,nosuid,nodev,size=256m,mode=1700",
			"/run": "rw,noexec,nosuid,nodev,size=16m,mode=1700",
		},
		Mounts: []mount.Mount{
			bind(filepath.Join(absolute, "source.pack"), "/input/source.pack", true),
			bind(filepath.Join(absolute, "build-spec.json"), "/input/build-spec.json", true),
			bind(filepath.Join(absolute, "dependencies.json"), "/input/dependencies.json", true),
			bind(filepath.Join(absolute, "dependencies"), "/input/dependencies", true),
			bind(filepath.Join(absolute, "output"), "/output", false),
		},
	}
	return config, host, nil
}

func bind(source, target string, readOnly bool) mount.Mount {
	return mount.Mount{
		Type: mount.TypeBind, Source: source, Target: target, ReadOnly: readOnly,
		BindOptions: &mount.BindOptions{NonRecursive: true},
	}
}

func builderContainerName(commandID string) string {
	hash := sha256.Sum256([]byte(commandID))
	return "xmcl-builder-" + hex.EncodeToString(hash[:8])
}
