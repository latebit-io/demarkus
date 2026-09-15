//go:build darwin || linux

package answerbench

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestManagedProcessKillsDescendants(t *testing.T) {
	pidFile := t.TempDir() + "/descendant.pid"
	ctx, cancel := context.WithCancel(t.Context())
	cmd := child(ctx, os.Args[0], "-test.run=TestManagedProcessTreeHelper")
	cmd.Env = append(os.Environ(), "ANSWERBENCH_TREE_HELPER=parent", "ANSWERBENCH_TREE_PID="+pidFile)
	process, err := startManaged(cmd)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	var pid int
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
		raw, readErr := os.ReadFile(pidFile)
		if readErr == nil {
			if len(raw) > 0 {
				pid, err = strconv.Atoi(strings.TrimSpace(string(raw)))
				if err != nil {
					t.Fatal(err)
				}
				break
			}
			time.Sleep(10 * time.Millisecond)
			continue
		}
		if !errors.Is(readErr, os.ErrNotExist) {
			t.Fatal(readErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if pid == 0 {
		t.Fatal("descendant did not start")
	}
	if err := process.stop(cancel); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
		err = syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("descendant process %d survived owned cleanup", pid)
}

func TestManagedProcessStopsCompletedChild(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	process, err := startManaged(child(ctx, "/usr/bin/true"))
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	<-process.Done()
	if err := process.stop(cancel); err != nil {
		t.Fatal(err)
	}
}

func TestManagedProcessStopsDescendantAfterParentExit(t *testing.T) {
	pidFile := t.TempDir() + "/descendant.pid"
	ctx, cancel := context.WithCancel(t.Context())
	cmd := child(ctx, os.Args[0], "-test.run=TestManagedProcessTreeHelper")
	cmd.Env = append(os.Environ(), "ANSWERBENCH_TREE_HELPER=parent-exit", "ANSWERBENCH_TREE_PID="+pidFile)
	process, err := startManaged(cmd)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	<-process.Done()
	if err := process.stop(cancel); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
		err = syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("descendant process %d survived owned cleanup", pid)
}

func TestManagedProcessTreeHelper(t *testing.T) {
	mode := os.Getenv("ANSWERBENCH_TREE_HELPER")
	if mode == "" {
		return
	}
	if mode == "parent" || mode == "parent-exit" {
		cmd := exec.Command(os.Args[0], "-test.run=TestManagedProcessTreeHelper")
		cmd.Env = append(os.Environ(), "ANSWERBENCH_TREE_HELPER=descendant")
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(os.Getenv("ANSWERBENCH_TREE_PID"), []byte(strconv.Itoa(cmd.Process.Pid)), 0o600); err != nil {
			t.Fatal(err)
		}
		if mode == "parent-exit" {
			return
		}
	}
	for {
		time.Sleep(time.Second)
	}
}
