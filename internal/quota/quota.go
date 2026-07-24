package quota

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
)

type Applier interface {
	Validate(ctx context.Context) error
	Apply(ctx context.Context, directory string, gib int64) error
}

type XFSProjectQuota struct {
	MountPath   string
	ProjectBase uint32
}

func (q XFSProjectQuota) Validate(ctx context.Context) error {
	return q.run(ctx, "state")
}

func (q XFSProjectQuota) Apply(ctx context.Context, directory string, gib int64) error {
	if gib < 1 {
		return fmt.Errorf("workspace quota must be positive")
	}
	projectID := q.projectID(directory)
	if err := q.run(ctx, "project -s -p "+directory+" "+projectID); err != nil {
		return fmt.Errorf("assign XFS project quota: %w", err)
	}
	if err := q.run(ctx, "limit -p bhard="+strconv.FormatInt(gib, 10)+"g "+projectID); err != nil {
		return fmt.Errorf("set XFS project quota: %w", err)
	}
	return nil
}

func (q XFSProjectQuota) projectID(directory string) string {
	var hash uint32 = 2166136261
	for _, b := range []byte(directory) {
		hash ^= uint32(b)
		hash *= 16777619
	}
	return strconv.FormatUint(uint64(q.ProjectBase+(hash&0x3fffffff)), 10)
}

func (q XFSProjectQuota) run(ctx context.Context, command string) error {
	output, err := exec.CommandContext(ctx, "xfs_quota", "-x", "-c", command, q.MountPath).CombinedOutput()
	if err != nil {
		return fmt.Errorf("xfs_quota: %w: %s", err, output)
	}
	return nil
}
