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
	if config.ObjectStorageEndpoint != "ewr1.vultrobjects.com" || config.ObjectStorageBucket != "workspaces" {
		t.Fatalf("canonical object storage values were not loaded: %#v", config)
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

func setCanonicalEnvironment(t *testing.T) {
	t.Helper()
	for name, value := range map[string]string{
		"XMCL_SHARED_NODE_ID":                  "node_1",
		"XMCL_CONTROL_PLANE_URL":               "https://control.example.test",
		"XMCL_CONTROL_PLANE_CREDENTIAL":        "bootstrap",
		"XMCL_VULTR_OBJECT_STORAGE_ENDPOINT":   "ewr1.vultrobjects.com",
		"XMCL_VULTR_OBJECT_STORAGE_REGION":     "ewr1",
		"XMCL_VULTR_OBJECT_STORAGE_BUCKET":     "workspaces",
		"XMCL_VULTR_OBJECT_STORAGE_ACCESS_KEY": "access",
		"XMCL_VULTR_OBJECT_STORAGE_SECRET_KEY": "secret",
		"XMCL_WORKSPACE_ROOT":                  "/var/lib/xmcl-shared/workspaces",
		"XMCL_STATE_ROOT":                      "/var/lib/xmcl-shared/state",
		"XMCL_CONTAINER_IMAGE":                 "minecraft:test",
		"XMCL_RCON_STOP_TIMEOUT_SECONDS":       "60",
		"XMCL_TOTAL_MEMORY_MIB":                "1024",
		"XMCL_TOTAL_SHARED_CPU":                "2",
		"XMCL_TOTAL_WORKSPACE_GIB":             "100",
		"XMCL_QUOTA_MOUNT_PATH":                "/var/lib/xmcl-shared",
		"XMCL_QUOTA_PROJECT_BASE":              "1000",
		"XMCL_METRICS_ADDR":                    "127.0.0.1:9464",
	} {
		t.Setenv(name, value)
	}
}
