package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

const configPath = "/etc/xmcl-shared-node-agent/quota-helper.json"

type config struct {
	WorkspaceRoot string `json:"workspaceRoot"`
	MountPath     string `json:"mountPath"`
	ProjectBase   uint32 `json:"projectBase"`
}

func main() {
	if os.Geteuid() != 0 {
		fatal(errors.New("quota helper must run with effective root privileges"))
	}
	config, err := loadConfig()
	if err != nil {
		fatal(err)
	}
	if len(os.Args) == 2 && os.Args[1] == "check" {
		fatal(run(config, "state"))
	}
	if len(os.Args) != 6 || os.Args[1] != "apply" || os.Args[2] != "--directory" || os.Args[4] != "--gib" {
		fatal(errors.New("invalid quota helper arguments"))
	}
	directory, gib := os.Args[3], os.Args[5]
	if err := validateDirectory(config, directory); err != nil {
		fatal(err)
	}
	limit, err := strconv.ParseInt(gib, 10, 64)
	if err != nil || limit < 1 {
		fatal(errors.New("quota limit must be positive"))
	}
	id := projectID(config.ProjectBase, directory)
	if err := run(config, "project -s -p "+directory+" "+id); err != nil {
		fatal(err)
	}
	fatal(run(config, "limit -p bhard="+strconv.FormatInt(limit, 10)+"g "+id))
}

func loadConfig() (config, error) {
	info, err := os.Stat(configPath)
	if err != nil {
		return config{}, fmt.Errorf("stat quota helper config: %w", err)
	}
	if info.Mode().Perm()&0o022 != 0 {
		return config{}, errors.New("quota helper config must not be group or world writable")
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		return config{}, fmt.Errorf("read quota helper config: %w", err)
	}
	var loaded config
	if err := json.Unmarshal(data, &loaded); err != nil {
		return config{}, fmt.Errorf("decode quota helper config: %w", err)
	}
	if !filepath.IsAbs(loaded.WorkspaceRoot) || !filepath.IsAbs(loaded.MountPath) ||
		strings.ContainsAny(loaded.WorkspaceRoot+loaded.MountPath, " \t\n\r'\"") || loaded.ProjectBase == 0 {
		return config{}, errors.New("quota helper configuration is unsafe")
	}
	return loaded, nil
}

func validateDirectory(config config, directory string) error {
	relative, err := filepath.Rel(config.WorkspaceRoot, directory)
	if err != nil || relative == "." || relative == ".." || strings.Contains(relative, string(filepath.Separator)) ||
		strings.HasPrefix(relative, ".."+string(filepath.Separator)) || strings.ContainsAny(directory, " \t\n\r'\"") {
		return errors.New("directory is not a direct workspace child")
	}
	return nil
}

func projectID(base uint32, directory string) string {
	hash := uint32(2166136261)
	for _, byte := range []byte(directory) {
		hash ^= uint32(byte)
		hash *= 16777619
	}
	return strconv.FormatUint(uint64(base+(hash&0x3fffffff)), 10)
}

func run(config config, command string) error {
	output, err := exec.Command("xfs_quota", "-x", "-c", command, config.MountPath).CombinedOutput()
	if err != nil {
		return fmt.Errorf("xfs_quota: %w: %s", err, output)
	}
	return nil
}

func fatal(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
