package incus

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"

	"incus-for-devs/internal/core"
)

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

var _ core.ServiceBackend = (*Backend)(nil)

func (b *Backend) serviceInstance(ctx context.Context, p core.Principal, name string) (instance, string, error) {
	if err := validatePrincipal(p); err != nil {
		return instance{}, "", err
	}
	if !validName(name) {
		return instance{}, "", errors.New("invalid instance name")
	}
	items, err := b.list(ctx, p)
	if err != nil {
		return instance{}, "", err
	}
	item, err := findOwned(items, p, name)
	if err != nil {
		return instance{}, "", err
	}
	if item.Status != "Running" {
		return instance{}, "", errors.New("only a running owned container can be published")
	}
	id := instanceConfig(item)["volatile.uuid"]
	if !uuidPattern.MatchString(id) || item.LastUsedAt.IsZero() {
		return instance{}, "", errors.New("container has no verifiable immutable identity")
	}
	// Incus supplies the last-start timestamp; a guest clock or UUID alone cannot identify a boot.
	return item, id + "@" + item.LastUsedAt.UTC().Format(time.RFC3339Nano), nil
}

func (b *Backend) ServiceIdentity(ctx context.Context, p core.Principal, name string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, b.config.OperationTimeout)
	defer cancel()
	item, id, err := b.serviceInstance(ctx, p, name)
	if err != nil {
		return "", err
	}
	if _, err := b.serviceAddress(ctx, p, item); err != nil {
		return "", err
	}
	return id, nil
}

func (b *Backend) DialService(ctx context.Context, p core.Principal, name, expectedID string, port int) (net.Conn, error) {
	uuid, boot, _ := strings.Cut(expectedID, "@")
	_, bootErr := time.Parse(time.RFC3339Nano, boot)
	if port < 1 || port > 65535 || !uuidPattern.MatchString(uuid) || bootErr != nil {
		return nil, errors.New("invalid explicit container service")
	}
	ctx, cancel := context.WithTimeout(ctx, b.config.OperationTimeout)
	defer cancel()
	if err := b.lock(ctx); err != nil {
		return nil, err
	}
	defer func() { <-b.gate }()
	item, id, err := b.serviceInstance(ctx, p, name)
	if err != nil {
		return nil, err
	}
	if id != expectedID {
		return nil, errors.New("container was restarted or recreated; publishing requires explicit re-enablement")
	}
	address, err := b.serviceAddress(ctx, p, item)
	if err != nil {
		return nil, err
	}
	return b.dialVerified(ctx, p, name, expectedID, func() (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp4", net.JoinHostPort(address, strconv.Itoa(port)))
	})
}

func (b *Backend) dialVerified(ctx context.Context, p core.Principal, name, expectedID string, connect func() (net.Conn, error)) (net.Conn, error) {
	connection, err := connect()
	if err != nil {
		if connection != nil {
			connection.Close()
		}
		return nil, err
	}
	_, current, err := b.serviceInstance(ctx, p, name)
	if err != nil || current != expectedID {
		connection.Close()
		return nil, errors.New("container changed during service connection; explicit re-enablement is required")
	}
	return connection, nil
}

func (b *Backend) serviceAddress(ctx context.Context, p core.Principal, item instance) (string, error) {
	devices := item.ExpandedDevices
	if devices == nil {
		devices = item.Devices
	}
	var network, mac string
	for name, device := range devices {
		if device["type"] != "nic" {
			continue
		}
		if network != "" || !validSlug(device["network"]) {
			return "", errors.New("publishing requires exactly one managed bridge NIC")
		}
		network = device["network"]
		mac = instanceConfig(item)["volatile."+name+".hwaddr"]
		if device["hwaddr"] != "" {
			mac = device["hwaddr"]
		}
	}
	hardware, err := net.ParseMAC(mac)
	if network == "" || err != nil || len(hardware) != 6 {
		return "", errors.New("cannot verify the container NIC hardware address")
	}
	data, err := b.run(ctx, p, "query", "/1.0/networks/"+network+"?project="+p.Project)
	if err != nil {
		return "", err
	}
	var bridge struct {
		Type    string            `json:"type"`
		Managed bool              `json:"managed"`
		Config  map[string]string `json:"config"`
	}
	if json.Unmarshal(data, &bridge) != nil || bridge.Type != "bridge" || !bridge.Managed {
		return "", errors.New("publishing only supports a managed bridge")
	}
	gateway, subnet, err := net.ParseCIDR(bridge.Config["ipv4.address"])
	if err != nil || gateway.To4() == nil || !gateway.IsPrivate() {
		return "", errors.New("publishing requires a private IPv4 managed bridge")
	}
	data, err = b.run(ctx, p, "network", "list-leases", network, "--format=json")
	if err != nil {
		return "", err
	}
	var leases []struct {
		Address string `json:"address"`
		HWAddr  string `json:"hwaddr"`
		Type    string `json:"type"`
	}
	if json.Unmarshal(data, &leases) != nil {
		return "", errors.New("invalid managed bridge lease metadata")
	}
	local, err := net.InterfaceAddrs()
	if err != nil {
		return "", errors.New("cannot verify host interface addresses")
	}
	var address string
	for _, lease := range leases {
		leaseMAC, err := net.ParseMAC(lease.HWAddr)
		ip := net.ParseIP(lease.Address)
		if err != nil || leaseMAC.String() != hardware.String() || ip == nil || ip.To4() == nil || (lease.Type != "dynamic" && lease.Type != "static") {
			continue
		}
		if !subnet.Contains(ip) || ip.Equal(gateway) || !ip.IsPrivate() || ip.Equal(subnet.IP) {
			return "", errors.New("unsafe container lease address")
		}
		for _, hostAddress := range local {
			hostIP, _, err := net.ParseCIDR(hostAddress.String())
			if err == nil && hostIP.Equal(ip) {
				return "", errors.New("refusing to publish a host interface address")
			}
		}
		if address != "" && address != ip.String() {
			return "", errors.New("ambiguous IPv4 leases for the container NIC")
		}
		address = ip.String()
	}
	if address == "" {
		return "", errors.New("no verified IPv4 lease for the container NIC")
	}
	return address, nil
}
