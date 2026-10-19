package docker

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/voxelum/xmcl-shared-node-agent/internal/controlplane"
)

func TestBuilderOuterDockerSandboxLive(t *testing.T) {
	if os.Getenv("XMCL_BUILDER_DOCKER_LIVE_TEST") != "1" {
		t.Skip("set XMCL_BUILDER_DOCKER_LIVE_TEST=1 after building the final Builder image")
	}
	image := os.Getenv("XMCL_BUILDER_DOCKER_TEST_IMAGE")
	if image == "" {
		image = "xmcl-builder-outer-probe:local"
	}
	config, host := builderLiveCreateRequest(t, image, "outer-sandbox-live-probe")
	config.Entrypoint = []string{"/bin/sh", "-ceu"}
	config.Cmd = []string{builderOuterSandboxLiveProbe}

	api, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = api.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	status, output, id := runBuilderLiveContainer(t, ctx, api, config, host)
	if status != 0 {
		t.Fatalf("probe exited %d: %q", status, output)
	}
	inspect, err := api.ContainerInspect(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if inspect.HostConfig.Privileged ||
		!equalStrings(inspect.HostConfig.CapDrop, []string{"ALL"}) ||
		!equalStrings(inspect.HostConfig.SecurityOpt, host.SecurityOpt) {
		t.Fatalf("live HostConfig was weakened: %#v", inspect.HostConfig)
	}
}

func TestBuilderDefaultSeccompRejectsBwrapLive(t *testing.T) {
	image := os.Getenv("XMCL_BUILDER_BWRAP_PROBE_IMAGE")
	if image == "" {
		t.Skip("set XMCL_BUILDER_BWRAP_PROBE_IMAGE to the pre-change Builder image")
	}
	config, host := builderLiveCreateRequest(t, image, "default-seccomp-bwrap-probe")
	config.Entrypoint = []string{"/usr/bin/bwrap"}
	config.Cmd = []string{
		"--die-with-parent", "--new-session", "--unshare-all",
		"--uid", "10001", "--gid", "10001", "--clearenv",
		"--tmpfs", "/", "--dir", "/bin", "--dir", "/usr",
		"--proc", "/proc", "--dev", "/dev",
		"--ro-bind", "/bin", "/bin",
		"--ro-bind", "/lib", "/lib",
		"--ro-bind", "/lib64", "/lib64",
		"--ro-bind", "/usr/lib", "/usr/lib",
		"/bin/true",
	}
	api, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = api.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	status, output, _ := runBuilderLiveContainer(t, ctx, api, config, host)
	if status == 0 || !strings.Contains(output, "No permissions to create new namespace") {
		t.Fatalf("default seccomp bwrap result: status=%d output=%q", status, output)
	}
}

func TestBuilderProcessQuiescerLive(t *testing.T) {
	if os.Getenv("XMCL_BUILDER_DOCKER_LIVE_TEST") != "1" {
		t.Skip("set XMCL_BUILDER_DOCKER_LIVE_TEST=1 after building the final Builder image")
	}
	image := os.Getenv("XMCL_BUILDER_DOCKER_TEST_IMAGE")
	if image == "" {
		image = "xmcl-builder-outer-probe:local"
	}
	config, host := builderLiveCreateRequest(t, image, "process-quiescer-live-probe")
	config.Entrypoint = []string{"/usr/local/bin/node"}
	config.Cmd = []string{"--input-type=module", "--eval", builderProcessQuiescerLiveProbe}
	api, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = api.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	status, output, _ := runBuilderLiveContainer(t, ctx, api, config, host)
	if status != 0 || !strings.Contains(output, `"liveBefore":true,"liveAfter":false`) {
		t.Fatalf("quiescer probe exited %d: %q", status, output)
	}
}

func builderLiveCreateRequest(
	t *testing.T,
	image string,
	commandID string,
) (*container.Config, *container.HostConfig) {
	t.Helper()
	root, err := filepath.Abs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"source.pack", "build-spec.json", "dependencies.json"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("{}"), 0444); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"dependencies", "output"} {
		if err := os.Mkdir(filepath.Join(root, name), 0777); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(filepath.Join(root, name), 0777); err != nil {
			t.Fatal(err)
		}
	}
	command := controlplane.Command{
		CommandID: commandID,
		Attempt:   1,
		Prepare: &controlplane.ContentPrepareRequest{
			BuildSpec: controlplane.BuildSpecV1{
				Builder: controlplane.BuildImageIdentity{Image: image},
			},
		},
	}
	config, host, err := BuildBuilderCreateRequest(command, root, image)
	if err != nil {
		t.Fatal(err)
	}
	return config, host
}

func runBuilderLiveContainer(
	t *testing.T,
	ctx context.Context,
	api *client.Client,
	config *container.Config,
	host *container.HostConfig,
) (int64, string, string) {
	t.Helper()
	created, err := api.ContainerCreate(ctx, config, host, nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = api.ContainerRemove(context.Background(), created.ID, container.RemoveOptions{Force: true})
	})
	if err := api.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		_ = api.ContainerRemove(context.Background(), created.ID, container.RemoveOptions{Force: true})
		t.Fatal(err)
	}
	wait, waitErr := api.ContainerWait(ctx, created.ID, container.WaitConditionNotRunning)
	select {
	case err := <-waitErr:
		t.Fatal(err)
	case result := <-wait:
		logs, _ := api.ContainerLogs(ctx, created.ID, container.LogsOptions{
			ShowStdout: true,
			ShowStderr: true,
		})
		if logs == nil {
			return result.StatusCode, "", created.ID
		}
		defer logs.Close()
		output, _ := io.ReadAll(io.LimitReader(logs, 16*1024))
		return result.StatusCode, string(output), created.ID
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	return -1, "", created.ID
}

func equalStrings(left, right []string) bool {
	return strings.Join(left, "\x00") == strings.Join(right, "\x00")
}

const builderOuterSandboxLiveProbe = `
grep -Eq '^CapEff:[[:space:]]+0+$' /proc/self/status
grep -Eq '^CapBnd:[[:space:]]+0+$' /proc/self/status
grep -Eq '^NoNewPrivs:[[:space:]]+1$' /proc/self/status
mkdir /output/direct-mount
if mount -t tmpfs none /output/direct-mount 2>/dev/null; then
  echo "mount unexpectedly succeeded in the container user namespace" >&2
  exit 20
fi
if unshare --mount /bin/true 2>/dev/null; then
  echo "non-user unshare unexpectedly succeeded" >&2
  exit 21
fi
if nsenter --mount=/proc/1/ns/mnt /bin/true 2>/dev/null; then
  echo "setns unexpectedly succeeded" >&2
  exit 22
fi
if command -v bwrap >/dev/null 2>&1; then
  echo "node-local Builder unexpectedly contains bwrap" >&2
  exit 23
fi
prlimit --as=3221225472 --fsize=536870912 --nproc=256 --cpu=240 -- \
  /opt/xmcl/jres/java-runtime-delta-21/bin/java -version
printf '%s\n' '{"directRestrictedJava":true,"bubblewrap":"absent","capabilities":"none","noNewPrivileges":true,"directMount":false,"nonUserUnshare":false,"setns":false}'
`

const builderProcessQuiescerLiveProbe = `
import { spawn } from "node:child_process";
import { setTimeout as delay } from "node:timers/promises";
import {
  liveProcessIds,
  quiesceContainerProcesses,
} from "/opt/xmcl-builder/app/src/process-quiescer.mjs";

if (process.pid !== 1) throw new Error("trusted Node process is not PID1");
const launcher = spawn("/usr/bin/setsid", [
  "-f",
  "/bin/sh",
  "-c",
  "/bin/sh -c 'sleep 300 </dev/null >/dev/null 2>&1 &' &",
], { detached: true, stdio: "ignore" });
launcher.unref();
let before = [];
for (let attempt = 0; attempt < 50 && before.length === 0; attempt += 1) {
  await delay(10);
  before = await liveProcessIds();
}
if (before.length === 0) throw new Error("detached descendant was not observed");
await quiesceContainerProcesses();
const after = await liveProcessIds();
if (after.length !== 0) throw new Error("live descendant survived quiescence");
process.stdout.write(JSON.stringify({ liveBefore: true, liveAfter: false }) + "\n");
`
