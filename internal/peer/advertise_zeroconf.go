//go:build !darwin

package peer

import (
	"errors"
	"net"

	"github.com/grandcat/zeroconf"
)

type mdnsAdvertiser interface {
	Shutdown()
}

func startMDNSAdvertisers(instance string, port int, txt []string, interfaces []*net.Interface) ([]mdnsAdvertiser, error) {
	var advertisers []mdnsAdvertiser
	for _, networkInterface := range interfaces {
		if networkInterface == nil {
			continue
		}
		ips := interfaceIPStrings(networkInterface)
		if len(ips) == 0 {
			continue
		}
		server, err := zeroconf.RegisterProxy(
			instance, ServiceType, "local", port, localMDNSHostname(), ips, txt, []net.Interface{*networkInterface},
		)
		if err == nil {
			advertisers = append(advertisers, server)
		}
	}
	if len(advertisers) == 0 {
		return nil, errors.New("no multicast listener could be started")
	}
	return advertisers, nil
}
