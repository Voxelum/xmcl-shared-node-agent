package quota

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

const HelperPath = "/usr/local/libexec/xmcl-quota-helper"

type Applier interface {
	Validate(ctx context.Context) error
	Apply(ctx context.Context, directory string, gib int64) error
	Prepare(ctx context.Context, directory string) error
	Seal(ctx context.Context, directory string) error
}

// Helper delegates the privileged XFS mutation to a root-owned setuid helper.
// The helper reads its mount and project configuration from a root-owned file;
// the agent can supply only a workspace directory and a positive limit.
type Helper struct {
	Path          string
	WorkspaceRoot string
	run           func(context.Context, string, ...string) error
}

func NewHelper(workspaceRoot string) *Helper {
	return &Helper{
		Path:          HelperPath,
		WorkspaceRoot: workspaceRoot,
		run: func(ctx context.Context, path string, args ...string) error {
			output, err := exec.CommandContext(ctx, path, args...).CombinedOutput()
			if err != nil {
				return fmt.Errorf("quota helper: %w: %s", err, output)
			}
			return nil
		},
	}
}

func (q *Helper) Validate(ctx context.Context) error {
	if err := q.validateWorkspaceRoot(); err != nil {
		return err
	}
	return q.run(ctx, q.Path, "check")
}

func (q *Helper) Apply(ctx context.Context, directory string, gib int64) error {
	if gib < 1 {
		return errors.New("workspace quota must be positive")
	}
	if err := q.validateDirectory(directory); err != nil {
		return err
	}
	return q.run(ctx, q.Path, "apply", "--directory", directory, "--gib", strconv.FormatInt(gib, 10))
}

// Prepare grants container UID 1000 ownership of its direct bind-mounted
// workspace while retaining read-only group access for the trusted node agent.
// The root-owned helper performs the transition without granting access to
// other host users.
func (q *Helper) Prepare(ctx context.Context, directory string) error {
	if err := q.validateDirectory(directory); err != nil {
		return err
	}
	return q.run(ctx, q.Path, "prepare", "--directory", directory)
}

// Seal takes ownership back after Docker is stopped so untrusted runtime file
// modes cannot prevent the agent from archiving the active workspace.
func (q *Helper) Seal(ctx context.Context, directory string) error {
	if err := q.validateDirectory(directory); err != nil {
		return err
	}
	return q.run(ctx, q.Path, "seal", "--directory", directory)
}

func (q *Helper) validateWorkspaceRoot() error {
	if q.Path != HelperPath {
		return errors.New("quota helper path is fixed and cannot be overridden")
	}
	if !filepath.IsAbs(q.WorkspaceRoot) || strings.ContainsAny(q.WorkspaceRoot, " \t\n\r'\"") {
		return errors.New("workspace root is not safe for quota helper")
	}
	return nil
}

func (q *Helper) validateDirectory(directory string) error {
	if err := q.validateWorkspaceRoot(); err != nil {
		return err
	}
	relative, err := filepath.Rel(q.WorkspaceRoot, directory)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return errors.New("quota directory must be a direct child of the workspace root")
	}
	if strings.Contains(relative, string(filepath.Separator)) || strings.ContainsAny(directory, " \t\n\r'\"") {
		return errors.New("quota directory is not a safe service workspace")
	}
	return nil
}
