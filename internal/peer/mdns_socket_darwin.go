//go:build darwin

package peer

import (
	"context"
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

func listenMDNSUDP4() (*net.UDPConn, error) {
	return listenReusableMDNS("udp4", "0.0.0.0:5353")
}

func listenMDNSUDP6() (*net.UDPConn, error) {
	return listenReusableMDNS("udp6", "[::]:5353")
}

func listenReusableMDNS(network, address string) (*net.UDPConn, error) {
	listenConfig := net.ListenConfig{Control: func(_, _ string, raw syscall.RawConn) error {
		var socketErr error
		if err := raw.Control(func(fileDescriptor uintptr) {
			fd := int(fileDescriptor)
			if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_REUSEADDR, 1); err != nil {
				socketErr = err
				return
			}
			if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_REUSEPORT, 1); err != nil {
				socketErr = err
			}
		}); err != nil {
			return err
		}
		return socketErr
	}}
	packetConnection, err := listenConfig.ListenPacket(context.Background(), network, address)
	if err != nil {
		return nil, err
	}
	udpConnection, ok := packetConnection.(*net.UDPConn)
	if !ok {
		_ = packetConnection.Close()
		return nil, syscall.EPROTOTYPE
	}
	return udpConnection, nil
}
