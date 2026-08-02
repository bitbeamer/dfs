package peer

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/mdns"
)

type Offer struct {
	FileSystemID      string `json:"filesystem_id"`
	NetworkName       string `json:"network_name"`
	PeerID            string `json:"peer_id"`
	PeerName          string `json:"peer_name"`
	Host              string `json:"host"`
	Address           string `json:"address"`
	Port              int    `json:"port"`
	Endpoint          string `json:"endpoint"`
	ProtocolVersion   int    `json:"protocol_version"`
	CertificateSHA256 string `json:"certificate_sha256"`
}

type Network struct {
	FileSystemID string  `json:"filesystem_id"`
	NetworkName  string  `json:"network_name"`
	Offers       []Offer `json:"offers"`
}

func GroupOffers(offers []Offer) []Network {
	byID := make(map[string]int)
	var networks []Network
	for _, offer := range offers {
		index, found := byID[offer.FileSystemID]
		if !found {
			index = len(networks)
			byID[offer.FileSystemID] = index
			networks = append(networks, Network{FileSystemID: offer.FileSystemID, NetworkName: offer.NetworkName})
		}
		networks[index].Offers = append(networks[index].Offers, offer)
	}
	sort.Slice(networks, func(i, j int) bool {
		if networks[i].NetworkName != networks[j].NetworkName {
			return networks[i].NetworkName < networks[j].NetworkName
		}
		return networks[i].FileSystemID < networks[j].FileSystemID
	})
	return networks
}

func Discover(ctx context.Context, timeout time.Duration) ([]Offer, error) {
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	entries := make(chan *mdns.ServiceEntry, 128)
	interfaces := interfaceProvider()
	if len(interfaces) == 0 {
		interfaces = []*net.Interface{nil}
	}
	done := make(chan error, len(interfaces)*2)
	queries := 0
	for _, networkInterface := range interfaces {
		for _, family := range []string{"ipv4", "ipv6"} {
			params := mdns.DefaultParams(ServiceType)
			params.Timeout = timeout
			params.Interface = networkInterface
			params.Entries = entries
			params.DisableIPv4 = family == "ipv6"
			params.DisableIPv6 = family == "ipv4"
			params.Logger = log.New(io.Discard, "", 0)
			queries++
			go func() { done <- mdns.QueryContext(ctx, params) }()
		}
	}

	unique := make(map[string]Offer)
	remaining := queries
	for {
		select {
		case entry := <-entries:
			if entry == nil {
				continue
			}
			for _, offer := range offersFromEntry(entry) {
				key := offer.FileSystemID + "\x00" + offer.PeerID + "\x00" + offer.Endpoint
				unique[key] = offer
			}
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				remaining--
				if remaining > 0 {
					continue
				}
				if len(unique) == 0 {
					return nil, fmt.Errorf("discover DFS networks: %w", err)
				}
			} else {
				remaining--
				if remaining > 0 {
					continue
				}
			}
			result := make([]Offer, 0, len(unique))
			for _, offer := range unique {
				result = append(result, offer)
			}
			sort.Slice(result, func(i, j int) bool {
				if result[i].NetworkName != result[j].NetworkName {
					return result[i].NetworkName < result[j].NetworkName
				}
				if result[i].PeerName != result[j].PeerName {
					return result[i].PeerName < result[j].PeerName
				}
				return result[i].Endpoint < result[j].Endpoint
			})
			return result, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func multicastInterfaces() []*net.Interface {
	interfaces, _ := net.Interfaces()
	var result []*net.Interface
	for index := range interfaces {
		networkInterface := &interfaces[index]
		flags := networkInterface.Flags
		if flags&net.FlagUp == 0 || flags&net.FlagMulticast == 0 || flags&net.FlagLoopback != 0 || flags&net.FlagPointToPoint != 0 {
			continue
		}
		result = append(result, networkInterface)
	}
	return result
}

var interfaceProvider = multicastInterfaces

func offersFromEntry(entry *mdns.ServiceEntry) []Offer {
	fields := make(map[string]string)
	for _, field := range entry.InfoFields {
		key, value, found := strings.Cut(field, "=")
		if found {
			fields[key] = value
		}
	}
	version, err := strconv.Atoi(fields["v"])
	if err != nil || version <= 0 || fields["fs"] == "" || fields["peer"] == "" {
		return nil
	}
	networkName, err := decodeTXT(fields["network"])
	if err != nil {
		return nil
	}
	peerName, err := decodeTXT(fields["name"])
	if err != nil {
		return nil
	}
	base := Offer{
		FileSystemID: fields["fs"], NetworkName: networkName,
		PeerID: fields["peer"], PeerName: peerName, Host: strings.TrimSuffix(entry.Host, "."),
		Port: entry.Port, ProtocolVersion: version, CertificateSHA256: fields["cert"],
	}
	var addresses []net.IP
	if entry.AddrV4 != nil {
		addresses = append(addresses, entry.AddrV4)
	}
	if entry.AddrV6IPAddr != nil && entry.AddrV6IPAddr.IP != nil {
		addresses = append(addresses, entry.AddrV6IPAddr.IP)
	} else if entry.AddrV6 != nil {
		addresses = append(addresses, entry.AddrV6)
	}
	var result []Offer
	for _, address := range addresses {
		offer := base
		offer.Address = address.String()
		offer.Endpoint = "https://" + net.JoinHostPort(offer.Address, strconv.Itoa(offer.Port))
		result = append(result, offer)
	}
	return result
}

func encodeTXT(value string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(value))
}

func decodeTXT(value string) (string, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return "", err
	}
	return string(decoded), nil
}

func endpointHost(endpoint string) (string, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" {
		return "", errors.New("invalid DFS pairing endpoint")
	}
	return parsed.Hostname(), nil
}
