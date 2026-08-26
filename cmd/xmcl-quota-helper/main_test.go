package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExecuteTerminalCommandsDoNotFallThrough(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspaces")
	service := filepath.Join(workspace, "service-1")
	if err := makeDirectory(service); err != nil {
		t.Fatal(err)
	}
	cfg := config{
		WorkspaceRoot: workspace,
		MountPath:     root,
		ProjectBase:   1000,
		AgentUser:     "xmcl-agent",
	}
	tests := []struct {
		name         string
		args         []string
		wantUID      int
		wantGID      int
		wantDirMode  os.FileMode
		wantFileMode os.FileMode
		wantQuota    string
		transition   bool
	}{
		{name: "check", args: []string{"check"}, wantQuota: "state"},
		{
			name: "prepare", args: []string{"prepare", "--directory", service},
			wantUID: 1000, wantGID: 3000, wantDirMode: 0o750,
			wantFileMode: 0o640, transition: true,
		},
		{
			name: "seal", args: []string{"seal", "--directory", service},
			wantUID: 2000, wantGID: 3000, wantDirMode: 0o700,
			wantFileMode: 0o600, transition: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			quotaCalls := 0
			transitionCalls := 0
			err := execute(
				cfg,
				test.args,
				func(_ config, command string) error {
					quotaCalls++
					if command != test.wantQuota {
						t.Fatalf("quota command = %q", command)
					}
					return nil
				},
				func(config) (int, int, error) { return 2000, 3000, nil },
				func(path string, uid, gid int, directoryMode, fileMode os.FileMode) error {
					transitionCalls++
					if path != service || uid != test.wantUID || gid != test.wantGID ||
						directoryMode != test.wantDirMode || fileMode != test.wantFileMode {
						t.Fatalf(
							"transition = %q %d:%d %04o/%04o",
							path, uid, gid, directoryMode, fileMode,
						)
					}
					return nil
				},
			)
			if err != nil {
				t.Fatal(err)
			}
			if test.transition {
				if quotaCalls != 0 || transitionCalls != 1 {
					t.Fatalf("quota calls = %d, transition calls = %d", quotaCalls, transitionCalls)
				}
			} else if quotaCalls != 1 || transitionCalls != 0 {
				t.Fatalf("quota calls = %d, transition calls = %d", quotaCalls, transitionCalls)
			}
		})
	}
}

func makeDirectory(path string) error {
	return os.MkdirAll(path, 0o700)
}
