package controller

import (
	"context"
	"time"

	"asterferry/internal/update"
)

type UpdateHelperOptions struct {
	ParentPID   int
	BinaryPath  string
	StagedPath  string
	BackupPath  string
	HealthURL   string
	Target      string
	Mode        string
	ServiceName string
	RestartArgs []string
	StatusPath  string
	PIDFile     string
	Timeout     time.Duration
}

func RunUpdateHelper(ctx context.Context, options UpdateHelperOptions) error {
	return update.RunReplacementHelper(ctx, update.ReplacementOptions{
		ParentPID: options.ParentPID, BinaryPath: options.BinaryPath, StagedPath: options.StagedPath, BackupPath: options.BackupPath,
		HealthURL: options.HealthURL, Target: options.Target, Mode: options.Mode, ServiceName: options.ServiceName,
		RestartArgs: options.RestartArgs, StatusPath: options.StatusPath, PIDFile: options.PIDFile, TargetState: UpdateStateUpToDate, Timeout: options.Timeout,
	})
}
