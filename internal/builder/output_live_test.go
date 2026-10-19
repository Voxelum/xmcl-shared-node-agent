package builder

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/voxelum/xmcl-shared-node-agent/internal/controlplane"
)

func TestPublishedBuilderOutputLive(t *testing.T) {
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
	key, err := BuildKey(spec)
	if err != nil {
		t.Fatal(err)
	}
	contentKey, _, err := CacheKeys(key)
	if err != nil {
		t.Fatal(err)
	}
	// Exercise the same consumer as Manager.Prepare, not a parallel JSON checker.
	content, err := (&Manager{}).validateBuilderOutput(root, spec, key, contentKey)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("consumed real Builder output: key=%s sha256=%s bytes=%d", key, content.SHA256, content.CompressedSize)
}
