package config

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

type Config struct {
	NodeID                 string
	ControlPlaneURL        string
	ControlPlaneCredential string
	ObjectStorageEndpoint  string
	ObjectStorageRegion    string
	ObjectStorageBucket    string
	ObjectStorageAccessKey string
	ObjectStorageSecretKey string
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
		{"XMCL_CONTROL_PLANE_URL", &c.ControlPlaneURL},
		{"XMCL_CONTROL_PLANE_CREDENTIAL", &c.ControlPlaneCredential},
		{"XMCL_VULTR_OBJECT_STORAGE_ENDPOINT", &c.ObjectStorageEndpoint},
		{"XMCL_VULTR_OBJECT_STORAGE_REGION", &c.ObjectStorageRegion},
		{"XMCL_VULTR_OBJECT_STORAGE_BUCKET", &c.ObjectStorageBucket},
		{"XMCL_VULTR_OBJECT_STORAGE_ACCESS_KEY", &c.ObjectStorageAccessKey},
		{"XMCL_VULTR_OBJECT_STORAGE_SECRET_KEY", &c.ObjectStorageSecretKey},
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
