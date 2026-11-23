//go:build !linux

package dbnative

import (
	"context"
	"os/exec"
)

func nativeCommand(ctx context.Context, name string, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, name, args...)
}
