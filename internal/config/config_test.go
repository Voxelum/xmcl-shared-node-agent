package config

import (
	"os"
	"path/filepath"
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
	if config.ObjectStorageEndpoint != "https://xmclstaging.blob.core.windows.net" || config.ObjectStorageBucket != "workspaces" {
		t.Fatalf("canonical object storage values were not loaded: %#v", config)
	}
}

func TestLoadRejectsStaticObjectStorageCredentialsAndCredentialEndpoint(t *testing.T) {
	setCanonicalEnvironment(t)
	for _, name := range []string{
		"AZURE_STORAGE_ACCOUNT",
		"AZURE_STORAGE_KEY",
		"AZURE_STORAGE_CONNECTION_STRING",
		"AZURE_CLIENT_SECRET",
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
	t.Setenv("XMCL_AZURE_BLOB_ENDPOINT", "")
	t.Setenv("XMCL_S3_ENDPOINT", "legacy.invalid")
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "XMCL_AZURE_BLOB_ENDPOINT") {
		t.Fatalf("legacy object-storage variables unexpectedly accepted: %v", err)
	}
}

func TestLoadRequiresAzureBlobStorageOrigin(t *testing.T) {
	for _, endpoint := range []string{
		"https://objects.example.com",
		"https://xmclstaging.blob.core.windows.net/container",
		"http://xmclstaging.blob.core.windows.net",
	} {
		setCanonicalEnvironment(t)
		t.Setenv("XMCL_AZURE_BLOB_ENDPOINT", endpoint)
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "XMCL_AZURE_BLOB_ENDPOINT") {
			t.Fatalf("endpoint %q was unexpectedly accepted: %v", endpoint, err)
		}
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

func TestLoadValidatesOptionalOTLPExporter(t *testing.T) {
	for _, endpoint := range []string{
		"https://otel.example.test",
		"http://127.0.0.1:4318",
		"http://[::1]:4318",
	} {
		setCanonicalEnvironment(t)
		t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", endpoint)
		config, err := Load()
		if err != nil {
			t.Fatalf("endpoint %q: %v", endpoint, err)
		}
		if config.OTLPEndpoint != endpoint {
			t.Fatalf("endpoint %q did not enable OTLP", endpoint)
		}
	}
}

func TestLoadRejectsSignalSpecificOTLPOverrides(t *testing.T) {
	for _, name := range []string{
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT",
		"OTEL_EXPORTER_OTLP_METRICS_HEADERS",
		"OTEL_EXPORTER_OTLP_INSECURE",
	} {
		setCanonicalEnvironment(t)
		t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "https://otel.example.test")
		t.Setenv(name, "override")
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), name) {
			t.Fatalf("%s was unexpectedly accepted: %v", name, err)
		}
	}
}

func TestLoadRejectsUnsafeOTLPExporter(t *testing.T) {
	for _, endpoint := range []string{
		"http://collector.example.test:4318",
		"https://user:secret@collector.example.test",
		"https://collector.example.test?token=secret",
	} {
		setCanonicalEnvironment(t)
		t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", endpoint)
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "OTEL_EXPORTER_OTLP_ENDPOINT") {
			t.Fatalf("endpoint %q was unexpectedly accepted: %v", endpoint, err)
		}
	}
}

func TestLoadRejectsOTLPHeadersWithoutEndpoint(t *testing.T) {
	setCanonicalEnvironment(t)
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "authorization=node-token")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "OTEL_EXPORTER_OTLP_HEADERS") {
		t.Fatalf("orphan OTLP headers were unexpectedly accepted: %v", err)
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

func TestLoadAllowsConsumedBootstrapWithPersistedNodeCredential(t *testing.T) {
	setCanonicalEnvironment(t)
	stateRoot := t.TempDir()
	t.Setenv("XMCL_STATE_ROOT", stateRoot)
	t.Setenv("XMCL_CONTROL_PLANE_CREDENTIAL", "")
	if err := os.WriteFile(filepath.Join(stateRoot, "control-plane-credential"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(); err != nil {
		t.Fatal(err)
	}
}

func TestLoadRequiresBootstrapWithoutPersistedNodeCredential(t *testing.T) {
	setCanonicalEnvironment(t)
	t.Setenv("XMCL_STATE_ROOT", t.TempDir())
	t.Setenv("XMCL_CONTROL_PLANE_CREDENTIAL", "")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "XMCL_CONTROL_PLANE_CREDENTIAL") {
		t.Fatalf("missing enrollment credential was accepted: %v", err)
	}
}

func setCanonicalEnvironment(t *testing.T) {
	t.Helper()
	for name, value := range map[string]string{
		"XMCL_SHARED_NODE_ID":                      "node_1",
		"XMCL_SHARED_NODE_INSTANCE_ID":             "instance_1",
		"XMCL_SHARED_NODE_REGION":                  "sgp",
		"XMCL_CONTROL_PLANE_URL":                   "https://control.example.test",
		"XMCL_CONTROL_PLANE_CREDENTIAL":            "bootstrap",
		"XMCL_SHARED_NODE_INGRESS_HOST":            "198.51.100.10",
		"XMCL_AZURE_BLOB_ENDPOINT":                 "https://xmclstaging.blob.core.windows.net",
		"XMCL_AZURE_BLOB_CONTAINER":                "workspaces",
		"AZURE_STORAGE_ACCOUNT":                    "",
		"AZURE_STORAGE_KEY":                        "",
		"AZURE_STORAGE_CONNECTION_STRING":          "",
		"AZURE_CLIENT_SECRET":                      "",
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
		"OTEL_EXPORTER_OTLP_ENDPOINT":              "",
		"OTEL_EXPORTER_OTLP_HEADERS":               "",
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT":       "",
		"OTEL_EXPORTER_OTLP_METRICS_HEADERS":       "",
		"OTEL_EXPORTER_OTLP_INSECURE":              "",
	} {
		t.Setenv(name, value)
	}
}
