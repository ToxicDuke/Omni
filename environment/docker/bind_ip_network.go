package docker

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"strings"

	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
)

type bindIPNetwork struct {
	Name    string
	Address string
	Subnet  string
	Gateway string
}

// newBindIPNetwork returns the smallest bridge network Docker can use for the
// requested address. A /30 keeps the host route narrow and provides one peer
// address for Docker's bridge gateway.
func newBindIPNetwork(value string) (*bindIPNetwork, error) {
	ip := net.ParseIP(value).To4()
	if ip == nil || !ip.IsGlobalUnicast() {
		return nil, fmt.Errorf("bind IP alias %q is not a unicast IPv4 address", value)
	}

	raw := binary.BigEndian.Uint32(ip)
	base := raw &^ uint32(3)
	host := raw - base
	if host == 0 || host == 3 {
		return nil, fmt.Errorf("bind IP alias %q is not usable in its containing /30", value)
	}

	gatewayRaw := base + 1
	if gatewayRaw == raw {
		gatewayRaw = base + 2
	}

	baseIP := make(net.IP, net.IPv4len)
	gatewayIP := make(net.IP, net.IPv4len)
	binary.BigEndian.PutUint32(baseIP, base)
	binary.BigEndian.PutUint32(gatewayIP, gatewayRaw)

	return &bindIPNetwork{
		Name:    "pterodactyl_bind_" + strings.ReplaceAll(ip.String(), ".", "_"),
		Address: ip.String(),
		Subnet:  baseIP.String() + "/30",
		Gateway: gatewayIP.String(),
	}, nil
}

func (e *Environment) ensureBindIPNetwork(ctx context.Context, value string, outgoingIP string) (*bindIPNetwork, error) {
	definition, err := newBindIPNetwork(value)
	if err != nil {
		return nil, err
	}

	if existing, err := e.client.NetworkInspect(ctx, definition.Name, network.InspectOptions{}); err == nil {
		if existing.Driver != "bridge" ||
			len(existing.IPAM.Config) == 0 ||
			existing.IPAM.Config[0].Subnet != definition.Subnet ||
			existing.IPAM.Config[0].Gateway != definition.Gateway ||
			existing.Options["com.docker.network.host_ipv4"] != outgoingIP {
			return nil, fmt.Errorf("docker network %s exists with an incompatible configuration", definition.Name)
		}
		return definition, nil
	} else if !client.IsErrNotFound(err) {
		return nil, err
	}

	enableIPv4 := true
	enableIPv6 := false
	_, err = e.client.NetworkCreate(ctx, definition.Name, network.CreateOptions{
		Driver:     "bridge",
		EnableIPv4: &enableIPv4,
		EnableIPv6: &enableIPv6,
		Internal:   false,
		Attachable: false,
		Ingress:    false,
		ConfigOnly: false,
		IPAM: &network.IPAM{Config: []network.IPAMConfig{{
			Subnet:  definition.Subnet,
			Gateway: definition.Gateway,
		}}},
		Options: map[string]string{
			"com.docker.network.bridge.enable_ip_masquerade": "true",
			"com.docker.network.host_ipv4":                   outgoingIP,
		},
		Labels: map[string]string{
			"Service":     "Pterodactyl",
			"NetworkType": "bind_ip_alias",
		},
	})
	if err != nil {
		return nil, err
	}

	return definition, nil
}
