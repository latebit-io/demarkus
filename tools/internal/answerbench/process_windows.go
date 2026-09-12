package answerbench

import (
	"context"
	"os/exec"
)

func child(ctx context.Context, binary string, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, binary, args...)
}
