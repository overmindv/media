package render

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCancellationStopsProcessGroup(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", "sleep 10 & echo $! > \"$1\"; wait", "fixture", pidFile)
	setProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var child int
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline) && child == 0; {
		if data, err := os.ReadFile(pidFile); err == nil {
			_, _ = fmt.Sscan(string(data), &child)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	if child == 0 {
		t.Fatal("fixture child did not start")
	}
	proc := fmt.Sprintf("/proc/%d/stat", child)
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
		data, err := os.ReadFile(proc)
		if os.IsNotExist(err) || strings.Contains(string(data), ") Z ") {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("child kept running after its process group was cancelled")
}

func TestRendererDoesNotFollowOutputSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "source")
	if err := os.WriteFile(target, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(dir, "preview.webp")
	if err := os.Symlink(target, output); err != nil {
		t.Fatal(err)
	}
	if file, err := openOutput(output); err == nil {
		_ = file.Close()
		t.Error("renderer followed a symlink instead of opening its output")
	}
}
