package config

import (
	"strings"
	"testing"
)

func TestLoadUsesOnlyCanonicalObjectStorageVariables(t *testing.T) {
	setCanonicalEnvironment(t)
	t.Setenv("XMCL_S3_ENDPOINT", "legacy.invalid")
	t.Setenv("XMCL_S3_REGION", "legacy")
	t.Setenv("XMCL_S3_BUCKET", "legacy")
	t.Setenv("XMCL_S3_ACCESS_KEY", "legacy")
	t.Setenv("XMCL_S3_SECRET_KEY", "legacy")
	config, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if config.ObjectStorageEndpoint != "https://sgp1.vultrobjects.com" || config.ObjectStorageBucket != "workspaces" {
		t.Fatalf("canonical object storage values were not loaded: %#v", config)
	}
}

func TestLoadRejectsStaticObjectStorageCredentialsAndCredentialEndpoint(t *testing.T) {
	setCanonicalEnvironment(t)
	for _, name := range []string{
		"XMCL_VULTR_OBJECT_STORAGE_ACCESS_KEY",
		"XMCL_VULTR_OBJECT_STORAGE_SECRET_KEY",
		"XMCL_VULTR_OBJECT_STORAGE_CREDENTIAL_URL",
	} {
		t.Setenv(name, "forbidden")
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), name) {
			t.Fatalf("%s was unexpectedly accepted: %v", name, err)
		}
		t.Setenv(name, "")
	}
}

func TestLoadRejectsLegacyObjectStorageVariables(t *testing.T) {
	setCanonicalEnvironment(t)
	t.Setenv("XMCL_VULTR_OBJECT_STORAGE_ENDPOINT", "")
	t.Setenv("XMCL_S3_ENDPOINT", "legacy.invalid")
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "XMCL_VULTR_OBJECT_STORAGE_ENDPOINT") {
		t.Fatalf("legacy object-storage variables unexpectedly accepted: %v", err)
	}
}

func TestLoadRejectsPublicMetricsAddress(t *testing.T) {
	setCanonicalEnvironment(t)
	t.Setenv("XMCL_METRICS_ADDR", "0.0.0.0:9464")
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "XMCL_METRICS_ADDR") {
		t.Fatalf("public metrics address unexpectedly accepted: %v", err)
	}
}

func TestLoadRequiresAPISafeIngressHost(t *testing.T) {
	setCanonicalEnvironment(t)
	t.Setenv("XMCL_SHARED_NODE_INGRESS_HOST", "https://public-node.example")
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "INGRESS_HOST") {
		t.Fatalf("unsafe ingress host unexpectedly accepted: %v", err)
	}
}

func TestLoadRequiresSafeSharedNodeRegion(t *testing.T) {
	setCanonicalEnvironment(t)
	for _, region := range []string{"", "SGP", "sgp!"} {
		t.Setenv("XMCL_SHARED_NODE_REGION", region)
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "XMCL_SHARED_NODE_REGION") {
			t.Fatalf("region %q was unexpectedly accepted: %v", region, err)
		}
	}
}

func TestLoadRequiresImmutableXMCLRuntimeImage(t *testing.T) {
	setCanonicalEnvironment(t)
	t.Setenv("XMCL_CONTAINER_IMAGE", "minecraft:latest")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "XMCL_CONTAINER_IMAGE") {
		t.Fatalf("mutable container image was unexpectedly accepted: %v", err)
	}
}

func setCanonicalEnvironment(t *testing.T) {
	t.Helper()
	for name, value := range map[string]string{
		"XMCL_SHARED_NODE_ID":                      "node_1",
		"XMCL_SHARED_NODE_REGION":                  "sgp",
		"XMCL_CONTROL_PLANE_URL":                   "https://control.example.test",
		"XMCL_CONTROL_PLANE_CREDENTIAL":            "bootstrap",
		"XMCL_SHARED_NODE_INGRESS_HOST":            "198.51.100.10",
		"XMCL_VULTR_OBJECT_STORAGE_ENDPOINT":       "https://sgp1.vultrobjects.com",
		"XMCL_VULTR_OBJECT_STORAGE_REGION":         "sgp",
		"XMCL_VULTR_OBJECT_STORAGE_BUCKET":         "workspaces",
		"XMCL_VULTR_OBJECT_STORAGE_ACCESS_KEY":     "",
		"XMCL_VULTR_OBJECT_STORAGE_SECRET_KEY":     "",
		"XMCL_VULTR_OBJECT_STORAGE_CREDENTIAL_URL": "",
		"XMCL_WORKSPACE_ROOT":                      "/var/lib/xmcl-shared/workspaces",
		"XMCL_STATE_ROOT":                          "/var/lib/xmcl-shared/state",
		"XMCL_CONTAINER_IMAGE":                     "ghcr.io/voxelum/xmcl-shared-minecraft-runtime@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"XMCL_RCON_STOP_TIMEOUT_SECONDS":           "60",
		"XMCL_TOTAL_MEMORY_MIB":                    "1024",
		"XMCL_TOTAL_SHARED_CPU":                    "2",
		"XMCL_TOTAL_WORKSPACE_GIB":                 "100",
		"XMCL_QUOTA_MOUNT_PATH":                    "/var/lib/xmcl-shared",
		"XMCL_QUOTA_PROJECT_BASE":                  "1000",
		"XMCL_METRICS_ADDR":                        "127.0.0.1:9464",
	} {
		t.Setenv(name, value)
	}
}
