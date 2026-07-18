package command

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strings"
	"time"
)

type Runner struct {
	Directory string
	Stdout    io.Writer
	Stderr    io.Writer
	Logger    *slog.Logger
}

func (r Runner) Run(ctx context.Context, name string, args ...string) (string, error) {
	started := time.Now()
	command := strings.TrimSpace(strings.Join(append([]string{name}, args...), " "))
	if r.Logger != nil {
		r.Logger.Debug("command started", "command", command, "directory", r.Directory)
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = r.Directory
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if r.Logger != nil {
			r.Logger.Debug("command failed",
				"command", command,
				"duration", time.Since(started),
				"stdout_bytes", stdout.Len(),
				"stderr_bytes", stderr.Len(),
				"error", err,
			)
		}
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = strings.TrimSpace(stdout.String())
		}
		if message == "" {
			message = err.Error()
		}
		return stdout.String(), fmt.Errorf("%s %s: %s", name, strings.Join(args, " "), message)
	}
	if r.Logger != nil {
		r.Logger.Debug("command completed",
			"command", command,
			"duration", time.Since(started),
			"stdout_bytes", stdout.Len(),
			"stderr_bytes", stderr.Len(),
		)
	}
	if r.Stdout != nil {
		_, _ = io.Copy(r.Stdout, bytes.NewReader(stdout.Bytes()))
	}
	if r.Stderr != nil {
		_, _ = io.Copy(r.Stderr, bytes.NewReader(stderr.Bytes()))
	}
	return stdout.String(), nil
}

func Exists(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}
