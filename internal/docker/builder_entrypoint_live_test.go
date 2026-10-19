package docker

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/voxelum/xmcl-shared-node-agent/internal/builder"
	"github.com/voxelum/xmcl-shared-node-agent/internal/controlplane"
)

// Unlike the boundary probes, this uses the unmodified production entrypoint,
// real registry initialization, pinned JRE and offline NeoForge installer.
func TestBuilderEntrypointNeoForgeLive(t *testing.T) {
	if os.Getenv("XMCL_BUILDER_DOCKER_LIVE_TEST") != "1" {
		t.Skip("explicit CI live gate required")
	}
	root := os.Getenv("XMCL_BUILDER_PROBE_ROOT")
	if !filepath.IsAbs(root) {
		t.Fatal("absolute probe root required")
	}
	raw, err := os.ReadFile(filepath.Join(root, "build-spec.json"))
	if err != nil {
		t.Fatal(err)
	}
	var spec controlplane.BuildSpecV1
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	if spec.Builder.Image != os.Getenv("XMCL_BUILDER_DOCKER_TEST_IMAGE") {
		t.Fatal("probe image mismatch")
	}
	identity := builder.Identity{BuilderImage: spec.Builder.Image, RuntimeImage: spec.Runtime.Image, ToolchainCatalogRevision: spec.Builder.ToolchainCatalogRevision, RuntimeCatalogRevision: spec.Runtime.RuntimeCatalogRevision}
	if err := builder.ValidateBuildSpec(spec, identity); err != nil {
		t.Fatal(err)
	}
	key, err := builder.BuildKey(spec)
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewBuilder(identity)
	if err != nil {
		t.Fatal(err)
	}
	defer b.client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	if err := b.Validate(ctx); err != nil {
		t.Fatal(err)
	}
	command := controlplane.Command{CommandID: "ci-neoforge-entrypoint", Attempt: 1, Prepare: &controlplane.ContentPrepareRequest{BuildSpec: spec, ExpectedBuildKey: key}}
	if err := b.Run(ctx, command, root); err != nil {
		t.Fatal(err)
	}
	t.Log("production entrypoint, registry initialization and offline NeoForge installer completed; output consumption is the next mandatory gate")
}
