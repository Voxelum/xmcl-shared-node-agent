package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

type Config struct {
	NodeID                 string
	ControlPlaneURL        string
	ControlPlaneCredential string
	S3Endpoint             string
	S3Region               string
	S3Bucket               string
	S3AccessKey            string
	S3SecretKey            string
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
		{"XMCL_S3_ENDPOINT", &c.S3Endpoint},
		{"XMCL_S3_REGION", &c.S3Region},
		{"XMCL_S3_BUCKET", &c.S3Bucket},
		{"XMCL_S3_ACCESS_KEY", &c.S3AccessKey},
		{"XMCL_S3_SECRET_KEY", &c.S3SecretKey},
		{"XMCL_CONTAINER_IMAGE", &c.ContainerImage},
		{"XMCL_QUOTA_MOUNT_PATH", &c.QuotaMountPath},
	} {
		if *field.target, err = required(field.name); err != nil {
			return Config{}, err
		}
	}
	c.WorkspaceRoot = os.Getenv("XMCL_WORKSPACE_ROOT")
	if c.WorkspaceRoot == "" {
		c.WorkspaceRoot = "/var/lib/xmcl-shared/workspaces"
	}
	c.WorkspaceRoot, err = filepath.Abs(c.WorkspaceRoot)
	if err != nil {
		return Config{}, fmt.Errorf("resolve workspace root: %w", err)
	}
	c.StateRoot = os.Getenv("XMCL_STATE_ROOT")
	if c.StateRoot == "" {
		c.StateRoot = filepath.Join(filepath.Dir(c.WorkspaceRoot), "agent-state")
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
	c.MetricsAddr = os.Getenv("XMCL_METRICS_ADDR")
	if c.MetricsAddr == "" {
		c.MetricsAddr = "127.0.0.1:9464"
	}
	return c, nil
}
