package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"
)

type Config struct {
	NodeID                 string
	Region                 string
	ControlPlaneURL        string
	ControlPlaneCredential string
	IngressHost            string
	ObjectStorageEndpoint  string
	ObjectStorageRegion    string
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
}

var ingressHostPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9.-]{0,251}[a-z0-9])?$`)
var sharedNodeRegionPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,30}[a-z0-9])?$`)
var runtimeImagePattern = regexp.MustCompile(`^ghcr\.io/voxelum/xmcl-shared-minecraft-runtime@sha256:[a-f0-9]{64}$`)

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
		{"XMCL_SHARED_NODE_REGION", &c.Region},
		{"XMCL_CONTROL_PLANE_URL", &c.ControlPlaneURL},
		{"XMCL_CONTROL_PLANE_CREDENTIAL", &c.ControlPlaneCredential},
		{"XMCL_SHARED_NODE_INGRESS_HOST", &c.IngressHost},
		{"XMCL_VULTR_OBJECT_STORAGE_ENDPOINT", &c.ObjectStorageEndpoint},
		{"XMCL_VULTR_OBJECT_STORAGE_REGION", &c.ObjectStorageRegion},
		{"XMCL_VULTR_OBJECT_STORAGE_BUCKET", &c.ObjectStorageBucket},
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
	c.WorkspaceRoot, err = filepath.Abs(c.WorkspaceRoot)
	if err != nil {
		return Config{}, fmt.Errorf("resolve workspace root: %w", err)
	}
	c.StateRoot, err = filepath.Abs(c.StateRoot)
	if err != nil {
		return Config{}, fmt.Errorf("resolve state root: %w", err)
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
	return c, nil
}

func validateObjectStorageEndpoint(rawURL string) error {
	endpoint, err := url.Parse(rawURL)
	if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" ||
		endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" ||
		(endpoint.Path != "" && endpoint.Path != "/") {
		return fmt.Errorf("XMCL_VULTR_OBJECT_STORAGE_ENDPOINT must be an absolute HTTPS origin without credentials, path, query, or fragment")
	}
	return nil
}
