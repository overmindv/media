package render

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

func runThumbnail(ctx context.Context, source, output string, width int) error {
	cmd := thumbnailCommand(ctx, source, output, width)
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return ErrRejected
		}

		return ErrUnavailable
	}

	return nil
}

func thumbnailCommand(ctx context.Context, source, output string, width int) *exec.Cmd {
	// #nosec G204 G702 -- fixed executable, temporary paths and allowlisted width.
	cmd := exec.CommandContext(ctx, "/usr/bin/vipsthumbnail", source,
		"--size", strconv.Itoa(width)+"x>", "--output", output+"[Q=88,strip]")
	cmd.Dir = filepath.Dir(source)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + cmd.Dir, "TMPDIR=" + cmd.Dir,
		"VIPS_CONCURRENCY=1", "VIPS_BLOCK_UNTRUSTED=1", "GIO_USE_VFS=local"}
	setProcessGroup(cmd)

	return cmd
}

func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}

		return err
	}
	cmd.WaitDelay = time.Second
}

func openOutput(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0) //nolint:gosec // path stays inside the job directory
}
