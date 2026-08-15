package peer

import (
	"errors"
	"net"
	"strings"

	"github.com/pion/mdns/v2"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

type mdnsAdvertiser interface {
	Shutdown()
}

type pionMDNSAdvertiser struct {
	conn *mdns.Conn
}

func (a *pionMDNSAdvertiser) Shutdown() {
	_ = a.conn.Close()
}

func startMDNSAdvertisers(instance string, port int, txt []string, interfaces []*net.Interface) ([]mdnsAdvertiser, error) {
	service := mdns.ServiceInstance{
		Instance: instance,
		Service:  ServiceType,
		Domain:   "local",
		Host:     localMDNSHostname(),
		Port:     uint16(port),
		Text:     pionTXTEntries(txt),
	}
	localName := strings.TrimSuffix(localMDNSHostname(), ".")
	conn, err := newMDNSConn(interfaces, mdns.WithLocalNames(localName), mdns.WithService(service))
	if err != nil {
		return nil, err
	}
	return []mdnsAdvertiser{&pionMDNSAdvertiser{conn: conn}}, nil
}

func newMDNSConn(interfacePointers []*net.Interface, options ...mdns.ServerOption) (*mdns.Conn, error) {
	interfaces := make([]net.Interface, 0, len(interfacePointers))
	includeLoopback := false
	for _, networkInterface := range interfacePointers {
		if networkInterface == nil {
			continue
		}
		interfaces = append(interfaces, *networkInterface)
		includeLoopback = includeLoopback || networkInterface.Flags&net.FlagLoopback != 0
	}
	if len(interfaces) == 0 {
		return nil, errors.New("no multicast-capable interface")
	}

	packet4, packet6, err := multicastPacketConns()
	if err != nil {
		return nil, err
	}
	options = append(options, mdns.WithInterfaces(interfaces...), mdns.WithIncludeLoopback(includeLoopback))
	conn, err := mdns.NewServer(packet4, packet6, options...)
	if err != nil {
		if packet4 != nil {
			_ = packet4.Close()
		}
		if packet6 != nil {
			_ = packet6.Close()
		}
		return nil, err
	}
	return conn, nil
}

func multicastPacketConns() (*ipv4.PacketConn, *ipv6.PacketConn, error) {
	var packet4 *ipv4.PacketConn
	address4, err4 := net.ResolveUDPAddr("udp4", mdns.DefaultAddressIPv4)
	if err4 == nil {
		if listener4, listenErr := net.ListenUDP("udp4", address4); listenErr == nil {
			packet4 = ipv4.NewPacketConn(listener4)
		}
	}

	var packet6 *ipv6.PacketConn
	address6, err6 := net.ResolveUDPAddr("udp6", mdns.DefaultAddressIPv6)
	if err6 == nil {
		if listener6, listenErr := net.ListenUDP("udp6", address6); listenErr == nil {
			packet6 = ipv6.NewPacketConn(listener6)
		}
	}
	if packet4 == nil && packet6 == nil {
		return nil, nil, errors.New("cannot listen for IPv4 or IPv6 multicast DNS")
	}
	return packet4, packet6, nil
}

func pionTXTEntries(fields []string) []mdns.TXTEntry {
	entries := make([]mdns.TXTEntry, 0, len(fields))
	for _, field := range fields {
		key, value, found := strings.Cut(field, "=")
		if found {
			entries = append(entries, mdns.NewTXTString(key, value))
		} else {
			entries = append(entries, mdns.NewTXTFlag(key))
		}
	}
	return entries
}
