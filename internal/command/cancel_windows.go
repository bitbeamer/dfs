//go:build windows

package command

import "os/exec"

func configureCancellation(*exec.Cmd) {}
