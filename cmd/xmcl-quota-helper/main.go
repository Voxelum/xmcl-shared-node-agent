package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
)

const configPath = "/etc/xmcl-shared-node-agent/quota-helper.json"

type config struct {
	WorkspaceRoot string `json:"workspaceRoot"`
	MountPath     string `json:"mountPath"`
	ProjectBase   uint32 `json:"projectBase"`
	AgentUser     string `json:"agentUser"`
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
	if len(os.Args) == 4 && (os.Args[1] == "prepare" || os.Args[1] == "seal") &&
		os.Args[2] == "--directory" {
		directory := os.Args[3]
		if err := validateDirectory(config, directory); err != nil {
			fatal(err)
		}
		uid, gid, err := agentIdentity(config)
		if err != nil {
			fatal(err)
		}
		if os.Args[1] == "prepare" {
			fatal(transitionOwnership(directory, 1000, 1000))
		}
		fatal(transitionOwnership(directory, uid, gid))
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
		strings.ContainsAny(loaded.WorkspaceRoot+loaded.MountPath, " \t\n\r'\"") || loaded.ProjectBase == 0 ||
		!validAgentUser(loaded.AgentUser) {
		return config{}, errors.New("quota helper configuration is unsafe")
	}
	return loaded, nil
}

func validAgentUser(value string) bool {
	if len(value) < 1 || len(value) > 32 {
		return false
	}
	for index, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') ||
			character == '_' || character == '-' {
			if index != 0 || (character >= 'a' && character <= 'z') || character == '_' {
				continue
			}
		}
		return false
	}
	return true
}

func validateDirectory(config config, directory string) error {
	relative, err := filepath.Rel(config.WorkspaceRoot, directory)
	if err != nil || relative == "." || relative == ".." || strings.Contains(relative, string(filepath.Separator)) ||
		strings.HasPrefix(relative, ".."+string(filepath.Separator)) || strings.ContainsAny(directory, " \t\n\r'\"") {
		return errors.New("directory is not a direct workspace child")
	}
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("workspace directory must be a real direct child")
	}
	return nil
}

func agentIdentity(config config) (int, int, error) {
	account, err := user.Lookup(config.AgentUser)
	if err != nil {
		return 0, 0, fmt.Errorf("lookup configured agent user: %w", err)
	}
	uid, err := strconv.Atoi(account.Uid)
	if err != nil || uid < 1 {
		return 0, 0, errors.New("configured agent UID is invalid")
	}
	gid, err := strconv.Atoi(account.Gid)
	if err != nil || gid < 1 {
		return 0, 0, errors.New("configured agent GID is invalid")
	}
	return uid, gid, nil
}

// transitionOwnership runs only after the direct-child check above. Docker is
// stopped before sealing, so no untrusted process can race this walk. Symlinks
// and special files are rejected instead of followed or changed.
func transitionOwnership(directory string, uid, gid int) error {
	return filepath.WalkDir(directory, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || (!info.IsDir() && !info.Mode().IsRegular()) {
			return errors.New("workspace contains an unsupported filesystem entry")
		}
		if err := os.Chown(path, uid, gid); err != nil {
			return err
		}
		if info.IsDir() {
			return os.Chmod(path, 0o700)
		}
		return os.Chmod(path, 0o600)
	})
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
