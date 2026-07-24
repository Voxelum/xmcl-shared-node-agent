package agent

import (
	"context"
	"fmt"

	"github.com/voxelum/xmcl-shared-node-agent/internal/controlplane"
)

type Daemon struct {
	NodeID   string
	Capacity controlplane.NodeCapacity
	Source   controlplane.CommandSource
	Reporter controlplane.Reporter
	Executor *Executor
}

func (d *Daemon) Register(ctx context.Context) error {
	return d.Reporter.Register(ctx, d.Capacity)
}

func (d *Daemon) Process(ctx context.Context, command controlplane.Command) error {
	result, err := d.Executor.Execute(ctx, command)
	if err != nil {
		return err
	}
	if result.Status == "started" {
		if err := d.Reporter.ReportStarted(ctx, command.ServiceID, command.AssignmentID); err != nil {
			return fmt.Errorf("report started: %w", err)
		}
	}
	if result.Status == "stopped-and-synced" && result.Sync != nil {
		if err := d.Reporter.ReportStoppedAndSynced(ctx, *result.Sync); err != nil {
			return fmt.Errorf("report stopped and synced: %w", err)
		}
	}
	if err := d.Source.Ack(ctx, command.CommandID, result); err != nil {
		return fmt.Errorf("ack command: %w", err)
	}
	if result.Status == "stopped-and-synced" {
		if err := d.Executor.Release(ctx, command); err != nil {
			return fmt.Errorf("release acknowledged local workspace: %w", err)
		}
	}
	return nil
}

func (d *Daemon) Run(ctx context.Context) error {
	if err := d.Register(ctx); err != nil {
		return fmt.Errorf("register node: %w", err)
	}
	for {
		command, err := d.Source.Next(ctx, d.NodeID)
		if err != nil {
			return fmt.Errorf("receive command: %w", err)
		}
		if err := d.Process(ctx, command); err != nil {
			return err
		}
	}
}
