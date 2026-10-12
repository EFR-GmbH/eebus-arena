package arena

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"net"
)

func (d Definition) networkMode() string {
	if d.Network == nil || d.Network.Mode == "" {
		return "ipv4"
	}
	return d.Network.Mode
}

func (d Definition) validateNetwork() error {
	mode := d.networkMode()
	if !Contains([]string{"ipv4", "ipv6", "dual-stack"}, mode) {
		return errors.New("network mode must be ipv4, ipv6 or dual-stack")
	}
	n := d.Network
	if n == nil {
		return nil
	}
	if n.Interface != "" && !interfaceName.MatchString(n.Interface) {
		return errors.New("invalid lab interface")
	}
	for _, family := range []struct {
		bits         int
		subnet, pool string
		enabled      bool
	}{
		{32, n.Subnet, n.IPRange, mode != "ipv6"},
		{128, n.IPv6Subnet, n.IPv6Range, mode != "ipv4"},
	} {
		if !family.enabled {
			if family.subnet != "" || family.pool != "" {
				return fmt.Errorf("IPv%d ranges require that family to be enabled", map[int]int{32: 4, 128: 6}[family.bits])
			}
			continue
		}
		if family.subnet == "" && family.pool == "" && n.Interface == "" {
			continue // An isolated bridge gets Docker IPv4 IPAM and a per-run ULA.
		}
		ip, subnet, err := net.ParseCIDR(family.subnet)
		if err != nil {
			return errors.New("network subnet must be a private IPv4 or IPv6 ULA CIDR")
		}
		_, bits := subnet.Mask.Size()
		if bits != family.bits || !ip.IsPrivate() || !ip.Equal(subnet.IP) {
			return errors.New("network subnet must be a canonical private CIDR of its enabled family")
		}
		if family.pool == "" && n.Interface == "" {
			continue
		}
		poolIP, pool, err := net.ParseCIDR(family.pool)
		if err != nil || !subnet.Contains(poolIP) || !poolIP.Equal(pool.IP) {
			return errors.New("network IP range must be a canonical CIDR inside its subnet")
		}
		prefix, _ := subnet.Mask.Size()
		poolPrefix, poolBits := pool.Mask.Size()
		if poolBits != bits || poolPrefix < prefix || poolPrefix > bits-3 {
			return errors.New("network IP range must contain at least eight addresses inside subnet")
		}
	}
	return nil
}

func (c *Controller) composeNetwork(run *Run) map[string]any {
	n := run.Definition.Network
	mode := run.Definition.networkMode()
	network := map[string]any{"driver": "bridge", "internal": true, "labels": map[string]string{"org.eebus-arena.run": run.ID}}
	if n != nil && n.Interface != "" {
		network["driver"] = "macvlan"
		delete(network, "internal")
		network["driver_opts"] = map[string]string{"parent": n.Interface}
	}
	var ranges []map[string]string
	if mode != "ipv6" && n != nil && n.Subnet != "" {
		ranges = append(ranges, map[string]string{"subnet": n.Subnet})
		if n.IPRange != "" {
			ranges[len(ranges)-1]["ip_range"] = n.IPRange
		}
	}
	if mode != "ipv4" {
		network["enable_ipv6"] = true
		if mode == "ipv6" {
			network["enable_ipv4"] = false
		}
		subnet, pool := "", ""
		if n != nil {
			subnet, pool = n.IPv6Subnet, n.IPv6Range
		}
		if subnet == "" {
			// Locally generated /64, independent for each owned run.
			hash := sha256.Sum256([]byte(run.ID))
			subnet = fmt.Sprintf("fd%02x:%02x%02x:%02x%02x:%02x%02x::/64", hash[0], hash[1], hash[2], hash[3], hash[4], hash[5], hash[6])
		}
		ranges = append(ranges, map[string]string{"subnet": subnet})
		if pool != "" {
			ranges[len(ranges)-1]["ip_range"] = pool
		}
	}
	if len(ranges) > 0 {
		network["ipam"] = map[string]any{"config": ranges}
	}
	return network
}
