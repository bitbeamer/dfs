package command

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

type Runner struct {
	Directory string
	Stdout    io.Writer
	Stderr    io.Writer
}

func (r Runner) Run(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = r.Directory
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = strings.TrimSpace(stdout.String())
		}
		if message == "" {
			message = err.Error()
		}
		return stdout.String(), fmt.Errorf("%s %s: %s", name, strings.Join(args, " "), message)
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
