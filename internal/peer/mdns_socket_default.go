//go:build !darwin

package peer

import (
	"net"

	"github.com/pion/mdns/v2"
)

func listenMDNSUDP4() (*net.UDPConn, error) {
	address, err := net.ResolveUDPAddr("udp4", mdns.DefaultAddressIPv4)
	if err != nil {
		return nil, err
	}
	return net.ListenUDP("udp4", address)
}

func listenMDNSUDP6() (*net.UDPConn, error) {
	address, err := net.ResolveUDPAddr("udp6", mdns.DefaultAddressIPv6)
	if err != nil {
		return nil, err
	}
	return net.ListenUDP("udp6", address)
}
