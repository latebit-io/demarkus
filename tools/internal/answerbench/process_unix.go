//go:build darwin || linux

package answerbench

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

func child(ctx context.Context, binary string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = 3 * time.Second
	return cmd
}

func killChild(cmd *exec.Cmd) error {
	err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	if errors.Is(err, syscall.EPERM) && cmd.ProcessState != nil {
		for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
			time.Sleep(10 * time.Millisecond)
			if probeErr := syscall.Kill(-cmd.Process.Pid, 0); errors.Is(probeErr, syscall.ESRCH) {
				return nil
			}
		}
	}
	return err
}

func terminateChild(cmd *exec.Cmd) error {
	if err := syscall.Kill(cmd.Process.Pid, 0); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	return err
}

func ownProcessTree(*exec.Cmd) (func() error, error) {
	return func() error { return nil }, nil
}
