//go:build darwin

package peer

import (
	"io"
	"net"
	"os/exec"
	"strconv"
)

type mdnsAdvertiser interface {
	Shutdown()
}

type nativeMDNSAdvertiser struct {
	command *exec.Cmd
}

func startMDNSAdvertisers(instance string, port int, txt []string, _ []*net.Interface) ([]mdnsAdvertiser, error) {
	arguments := []string{"-R", instance, ServiceType, "local.", strconv.Itoa(port)}
	arguments = append(arguments, txt...)
	command := exec.Command("/usr/bin/dns-sd", arguments...)
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	if err := command.Start(); err != nil {
		return nil, err
	}
	return []mdnsAdvertiser{&nativeMDNSAdvertiser{command: command}}, nil
}

func (a *nativeMDNSAdvertiser) Shutdown() {
	if a.command.Process != nil {
		_ = a.command.Process.Kill()
	}
	_ = a.command.Wait()
}
