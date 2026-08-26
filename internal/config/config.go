package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/voxelum/xmcl-shared-node-agent/internal/otlpconfig"
)

type Config struct {
	NodeID                 string
	InstanceID             string
	Region                 string
	ControlPlaneURL        string
	ControlPlaneCredential string
	IngressHost            string
	ObjectStorageEndpoint  string
	ObjectStorageBucket    string
	WorkspaceRoot          string
	StateRoot              string
	ContainerImage         string
	RCONStopTimeout        time.Duration
	TotalMemoryMiB         int64
	TotalSharedCPU         int64
	TotalWorkspaceGiB      int64
	QuotaMountPath         string
	QuotaProjectBase       uint32
	MetricsAddr            string
	OTLPEndpoint           string
	OTLPHeaders            map[string]string
}

var ingressHostPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9.-]{0,251}[a-z0-9])?$`)
var sharedNodeRegionPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,30}[a-z0-9])?$`)
var runtimeImagePattern = regexp.MustCompile(`^ghcr\.io/voxelum/xmcl-shared-minecraft-runtime@sha256:[a-f0-9]{64}$`)
var azureContainerPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{1,61}[a-z0-9])?$`)

func Load() (Config, error) {
	required := func(name string) (string, error) {
		value := os.Getenv(name)
		if value == "" {
			return "", fmt.Errorf("%s is required", name)
		}
		return value, nil
	}
	getInt := func(name string, minimum int64) (int64, error) {
		value, err := required(name)
		if err != nil {
			return 0, err
		}
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil || parsed < minimum {
			return 0, fmt.Errorf("%s must be an integer >= %d", name, minimum)
		}
		return parsed, nil
	}

	var err error
	c := Config{}
	for _, field := range []struct {
		name   string
		target *string
	}{
		{"XMCL_SHARED_NODE_ID", &c.NodeID},
		{"XMCL_SHARED_NODE_INSTANCE_ID", &c.InstanceID},
		{"XMCL_SHARED_NODE_REGION", &c.Region},
		{"XMCL_CONTROL_PLANE_URL", &c.ControlPlaneURL},
		{"XMCL_SHARED_NODE_INGRESS_HOST", &c.IngressHost},
		{"XMCL_AZURE_BLOB_ENDPOINT", &c.ObjectStorageEndpoint},
		{"XMCL_AZURE_BLOB_CONTAINER", &c.ObjectStorageBucket},
		{"XMCL_WORKSPACE_ROOT", &c.WorkspaceRoot},
		{"XMCL_STATE_ROOT", &c.StateRoot},
		{"XMCL_CONTAINER_IMAGE", &c.ContainerImage},
		{"XMCL_QUOTA_MOUNT_PATH", &c.QuotaMountPath},
		{"XMCL_METRICS_ADDR", &c.MetricsAddr},
	} {
		if *field.target, err = required(field.name); err != nil {
			return Config{}, err
		}
	}
	if !ingressHostPattern.MatchString(c.IngressHost) {
		return Config{}, fmt.Errorf("XMCL_SHARED_NODE_INGRESS_HOST must be a hostname or IPv4 address")
	}
	if !sharedNodeRegionPattern.MatchString(c.Region) {
		return Config{}, fmt.Errorf("XMCL_SHARED_NODE_REGION must be a provider region identifier")
	}
	if !runtimeImagePattern.MatchString(c.ContainerImage) {
		return Config{}, fmt.Errorf("XMCL_CONTAINER_IMAGE must be an immutable XMCL shared Minecraft runtime digest")
	}
	for _, name := range []string{
		"AZURE_STORAGE_ACCOUNT",
		"AZURE_STORAGE_KEY",
		"AZURE_STORAGE_CONNECTION_STRING",
		"AZURE_CLIENT_SECRET",
		"XMCL_VULTR_OBJECT_STORAGE_ACCESS_KEY",
		"XMCL_VULTR_OBJECT_STORAGE_SECRET_KEY",
		"XMCL_VULTR_OBJECT_STORAGE_CREDENTIAL_URL",
	} {
		if os.Getenv(name) != "" {
			return Config{}, fmt.Errorf("%s is forbidden: agents receive command-scoped URL grants, not object-storage credentials", name)
		}
	}
	if err := validateObjectStorageEndpoint(c.ObjectStorageEndpoint); err != nil {
		return Config{}, err
	}
	if !azureContainerPattern.MatchString(c.ObjectStorageBucket) ||
		strings.Contains(c.ObjectStorageBucket, "--") {
		return Config{}, fmt.Errorf("XMCL_AZURE_BLOB_CONTAINER must be a valid Azure Blob container name")
	}
	c.WorkspaceRoot, err = filepath.Abs(c.WorkspaceRoot)
	if err != nil {
		return Config{}, fmt.Errorf("resolve workspace root: %w", err)
	}
	c.StateRoot, err = filepath.Abs(c.StateRoot)
	if err != nil {
		return Config{}, fmt.Errorf("resolve state root: %w", err)
	}
	c.ControlPlaneCredential = os.Getenv("XMCL_CONTROL_PLANE_CREDENTIAL")
	if c.ControlPlaneCredential == "" {
		credentialPath := filepath.Join(c.StateRoot, "control-plane-credential")
		info, statErr := os.Stat(credentialPath)
		if statErr != nil || !info.Mode().IsRegular() {
			return Config{}, fmt.Errorf("XMCL_CONTROL_PLANE_CREDENTIAL is required without a persisted node credential")
		}
	}
	stopSeconds, err := getInt("XMCL_RCON_STOP_TIMEOUT_SECONDS", 1)
	if err != nil {
		return Config{}, err
	}
	c.RCONStopTimeout = time.Duration(stopSeconds) * time.Second
	if c.TotalMemoryMiB, err = getInt("XMCL_TOTAL_MEMORY_MIB", 1); err != nil {
		return Config{}, err
	}
	if c.TotalSharedCPU, err = getInt("XMCL_TOTAL_SHARED_CPU", 1); err != nil {
		return Config{}, err
	}
	if c.TotalWorkspaceGiB, err = getInt("XMCL_TOTAL_WORKSPACE_GIB", 1); err != nil {
		return Config{}, err
	}
	projectBase, err := getInt("XMCL_QUOTA_PROJECT_BASE", 1)
	if err != nil || projectBase > int64(^uint32(0)) {
		return Config{}, fmt.Errorf("XMCL_QUOTA_PROJECT_BASE must be an unsigned 32-bit integer")
	}
	c.QuotaProjectBase = uint32(projectBase)
	host, _, err := net.SplitHostPort(c.MetricsAddr)
	if err != nil || (host != "127.0.0.1" && host != "::1" && host != "[::1]" && host != "localhost") {
		return Config{}, fmt.Errorf("XMCL_METRICS_ADDR must bind to a loopback address")
	}
	for _, entry := range os.Environ() {
		name, value, _ := strings.Cut(entry, "=")
		if value != "" && (strings.HasPrefix(name, "OTEL_EXPORTER_OTLP_TRACES_") ||
			strings.HasPrefix(name, "OTEL_EXPORTER_OTLP_METRICS_") ||
			name == "OTEL_EXPORTER_OTLP_INSECURE") {
			return Config{}, fmt.Errorf("%s is forbidden; use the validated common OTLP endpoint and headers", name)
		}
	}
	if endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"); endpoint != "" {
		if err := otlpconfig.ValidateEndpoint(endpoint); err != nil {
			return Config{}, err
		}
		c.OTLPEndpoint = endpoint
		c.OTLPHeaders, err = otlpconfig.ParseHeaders(os.Getenv("OTEL_EXPORTER_OTLP_HEADERS"))
		if err != nil {
			return Config{}, err
		}
	} else if os.Getenv("OTEL_EXPORTER_OTLP_HEADERS") != "" {
		return Config{}, fmt.Errorf("OTEL_EXPORTER_OTLP_HEADERS requires OTEL_EXPORTER_OTLP_ENDPOINT")
	}

	return c, nil
}

func validateObjectStorageEndpoint(rawURL string) error {
	endpoint, err := url.Parse(rawURL)
	if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" ||
		endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" ||
		(endpoint.Path != "" && endpoint.Path != "/") ||
		!strings.HasSuffix(strings.ToLower(endpoint.Hostname()), ".blob.core.windows.net") {
		return fmt.Errorf("XMCL_AZURE_BLOB_ENDPOINT must be an Azure Blob HTTPS origin without credentials, path, query, or fragment")
	}
	return nil
}
