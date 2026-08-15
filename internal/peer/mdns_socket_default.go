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

func listenMDNSQueryUDP4(networkInterface *net.Interface) (*net.UDPConn, error) {
	return net.ListenMulticastUDP("udp4", networkInterface, &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: 5353})
}

func listenMDNSQueryUDP6(networkInterface *net.Interface) (*net.UDPConn, error) {
	return net.ListenMulticastUDP("udp6", networkInterface, &net.UDPAddr{
		IP: net.ParseIP("ff02::fb"), Port: 5353, Zone: networkInterface.Name,
	})
}
