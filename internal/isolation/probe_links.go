package isolation

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os/exec"
	"strings"
	"time"
)

// Resolve the host ports from the live guests' runtime-assigned MACs. The
// bridge forwarding table binds each MAC to a specific virtual interface.
func probeLinks(ctx context.Context, names []string) (map[string]bool, error) {
	data, err := container(ctx, nil, append([]string{"inspect"}, names...)...)
	if err != nil {
		return nil, errHostEvents
	}
	var rows []struct {
		ID     string
		Status struct {
			Networks []struct {
				IPv4Gateway string
				MACAddress  string
			}
		}
	}
	if json.Unmarshal(data, &rows) != nil || len(rows) != len(names) {
		return nil, errHostEvents
	}
	expected := make(map[string]bool)
	for _, name := range names {
		expected[name] = true
	}
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	bridges := map[string]string{}
	for _, iface := range interfaces {
		if !strings.HasPrefix(iface.Name, "bridge") {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			return nil, err
		}
		for _, addr := range addrs {
			ip, _, err := net.ParseCIDR(addr.String())
			if err == nil && ip.To4() != nil {
				if _, duplicate := bridges[ip.String()]; duplicate {
					return nil, errHostEvents
				}
				bridges[ip.String()] = iface.Name
			}
		}
	}
	links := make(map[string]bool)
	for _, row := range rows {
		if !expected[row.ID] || len(row.Status.Networks) != 1 {
			return nil, errHostEvents
		}
		delete(expected, row.ID)
		n := row.Status.Networks[0]
		mac, err := net.ParseMAC(n.MACAddress)
		bridge := bridges[n.IPv4Gateway]
		if err != nil || len(mac) != 6 || bridge == "" {
			return nil, errHostEvents
		}
		bounded, cancel := context.WithTimeout(ctx, 2*time.Second)
		table, err := exec.CommandContext(bounded, "/sbin/ifconfig", bridge, "addr").Output()
		cancel()
		if err != nil {
			return nil, errHostEvents
		}
		// Darwin ifconfig prints bridge MAC octets without leading zeroes.
		bridgeMAC := fmt.Sprintf("%x:%x:%x:%x:%x:%x", mac[0], mac[1], mac[2], mac[3], mac[4], mac[5])
		member := ""
		for line := range strings.SplitSeq(string(table), "\n") {
			f := strings.Fields(line)
			if len(f) >= 3 && strings.EqualFold(f[0], bridgeMAC) && strings.HasPrefix(f[2], "vmenet") {
				if member != "" {
					return nil, errHostEvents
				}
				member = f[2]
			}
		}
		if member == "" || links[member] {
			return nil, errHostEvents
		}
		links[bridge] = true
		links[member] = true
	}
	return links, nil
}

// Only newly attached or removed probe interfaces may account for a batch.
// Global, service, setup and unfamiliar interface events always invalidate.
func probeChanges(keys []string, before, after map[string]int, owned map[string]bool) bool {
	delta := map[string]bool{}
	for name, index := range before {
		if next, ok := after[name]; !ok {
			delta[name] = true
		} else if next != index {
			return true
		}
	}
	for name := range after {
		if _, ok := before[name]; !ok {
			delta[name] = true
		}
	}
	for name := range delta {
		if !owned[name] {
			return true
		}
	}
	for _, key := range keys {
		if key == "State:/Network/Interface" && len(delta) > 0 {
			continue
		}
		name, entity, ok := strings.Cut(strings.TrimPrefix(key, "State:/Network/Interface/"), "/")
		if !strings.HasPrefix(key, "State:/Network/Interface/") || !ok || !delta[name] {
			return true
		}
		switch entity {
		case "Link", "LinkQuality", "IPv4", "IPv6":
		default:
			return true
		}
	}
	return false
}
