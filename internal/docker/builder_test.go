package docker

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/voxelum/xmcl-shared-node-agent/internal/builder"
	"github.com/voxelum/xmcl-shared-node-agent/internal/controlplane"
)

func TestBuildBuilderCreateRequestIsFixedAndIsolated(t *testing.T) {
	image := builder.BuilderImageName + "@sha256:" + strings.Repeat("a", 64)
	command := controlplane.Command{
		CommandID: "command-1", Attempt: 2,
		Prepare: &controlplane.ContentPrepareRequest{
			ExpectedBuildKey: strings.Repeat("b", 64),
			BuildSpec: controlplane.BuildSpecV1{
				Builder: controlplane.BuildImageIdentity{Image: image},
			},
		},
	}
	root, err := filepath.Abs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	config, host, err := BuildBuilderCreateRequest(command, root, image)
	if err != nil {
		t.Fatal(err)
	}
	if config.Image != image || config.User != "10001:10001" || len(config.Env) != 0 ||
		len(config.Entrypoint) != 2 || config.Entrypoint[0] != "/usr/local/bin/node" ||
		config.Entrypoint[1] != "/opt/xmcl-builder/app/scripts/agent_builder_v1.mjs" ||
		len(config.Cmd) != 0 {
		t.Fatalf("untrusted builder process config: %#v", config)
	}
	if host.NetworkMode != "none" || !host.ReadonlyRootfs || host.Privileged ||
		len(host.CapDrop) != 1 || host.CapDrop[0] != "ALL" ||
		len(host.SecurityOpt) != 1 || host.SecurityOpt[0] != "no-new-privileges:true" {
		t.Fatalf("builder security config: %#v", host)
	}
	if host.Memory != builderMemoryBytes || host.MemorySwap != builderMemoryBytes ||
		host.CPUPeriod != cpuPeriod || host.CPUQuota != builderCPUQuota ||
		host.PidsLimit == nil || *host.PidsLimit != 256 {
		t.Fatalf("builder resource limits: %#v", host.Resources)
	}
	if host.AutoRemove || len(host.Binds) != 0 || len(host.Devices) != 0 ||
		host.PidMode != "" || host.IpcMode != "" || host.UsernsMode != "" {
		t.Fatalf("builder host access is not fixed: %#v", host)
	}
	if host.Tmpfs["/tmp"] != "rw,noexec,nosuid,nodev,size=256m,mode=1700" ||
		host.Tmpfs["/run"] != "rw,noexec,nosuid,nodev,size=16m,mode=1700" {
		t.Fatalf("builder tmpfs = %#v", host.Tmpfs)
	}
	expectedTargets := []string{
		"/input/source.pack", "/input/build-spec.json", "/input/dependencies.json",
		"/input/dependencies", "/output",
	}
	if len(host.Mounts) != len(expectedTargets) {
		t.Fatalf("builder mounts = %#v", host.Mounts)
	}
	for index, target := range expectedTargets {
		actual := host.Mounts[index]
		if actual.Target != target || actual.BindOptions == nil || !actual.BindOptions.NonRecursive ||
			actual.ReadOnly != (target != "/output") {
			t.Fatalf("builder mount %d = %#v", index, actual)
		}
		if !strings.HasPrefix(actual.Source, root+string(filepath.Separator)) {
			t.Fatalf("builder mount escaped staging root: %s", actual.Source)
		}
	}
	for key, value := range config.Labels {
		if strings.Contains(strings.ToLower(key+value), "sas") || strings.Contains(value, "?") {
			t.Fatalf("credential-shaped value reached builder label %q", key)
		}
	}
}

func TestBuildBuilderCreateRequestRejectsImageAndRelativeRoot(t *testing.T) {
	image := builder.BuilderImageName + "@sha256:" + strings.Repeat("a", 64)
	command := controlplane.Command{
		CommandID: "command-1", Attempt: 1,
		Prepare: &controlplane.ContentPrepareRequest{
			BuildSpec: controlplane.BuildSpecV1{
				Builder: controlplane.BuildImageIdentity{Image: image},
			},
		},
	}
	if _, _, err := BuildBuilderCreateRequest(command, "relative", image); err == nil {
		t.Fatal("relative staging root was accepted")
	}
	if _, _, err := BuildBuilderCreateRequest(command, filepath.Clean(`C:\absolute`), image+"0"); err == nil {
		t.Fatal("mismatched image was accepted")
	}
}

func TestClassifyBuilderCodeUsesFrozenFailureTaxonomy(t *testing.T) {
	cases := map[string]string{
		"invalid_build_spec":                    "invalid_build_spec",
		"unsupported_compatibility":             "unsupported_compatibility",
		"source_digest_mismatch":                "source_hash_mismatch",
		"invalid_mrpack":                        "source_pack_invalid",
		"mounted_dependency_identity_mismatch":  "dependency_hash_mismatch",
		"installer_timeout":                     "builder_timeout",
		"invalid_builder_output":                "builder_output_invalid",
		"catalog_revision_mismatch":             "builder_identity_mismatch",
		"installer_process_failed":              "installer_failed",
		"unrecognized_internal_builder_failure": "installer_failed",
	}
	for input, expected := range cases {
		if actual := classifyBuilderCode(input); actual != expected {
			t.Fatalf("%s classified as %s, want %s", input, actual, expected)
		}
	}
}
