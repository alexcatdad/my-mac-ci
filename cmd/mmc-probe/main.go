// Command mmc-probe runs inside a disposable Linux probe VM.
package main

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/alexcatdad/my-mac-ci/internal/isolation"
)

func main() {
	// A detached scan with an interrupted stdin must also expire.
	time.AfterFunc(4*time.Minute, func() { os.Exit(1) })
	if len(os.Args) != 2 || !run(os.Args[1]) {
		os.Exit(1)
	}
}

func run(mode string) bool {
	if mode == "idle" {
		time.Sleep(4 * time.Minute)
		return true
	}
	if mode == "listen" {
		listener, err := net.Listen("tcp4", ":"+isolation.ProbePort)
		if err != nil {
			return false
		}
		defer func() { _ = listener.Close() }()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return true
			}
			_ = conn.Close()
		}
	}
	network, ok := guestNetwork()
	if !ok {
		return false
	}
	if mode == "network" {
		// Invoked with container exec; the host captures metadata privately.
		return json.NewEncoder(os.Stdout).Encode(network) == nil
	}
	var scan isolation.Scan
	if mode != "scan" || json.NewDecoder(os.Stdin).Decode(&scan) != nil ||
		network.Prefix.Masked() != scan.Network.Prefix.Masked() || network.Gateway != scan.Network.Gateway {
		return false
	}
	result := isolation.Observations{TCP: make([]string, len(scan.Targets)), IPv6: ipv6()}
	var group sync.WaitGroup
	for i, target := range scan.Targets {
		group.Go(func() { result.TCP[i] = connect(target) })
	}
	group.Wait()
	return json.NewEncoder(os.Stdout).Encode(result) == nil
}

func connect(target string) string {
	conn, err := net.DialTimeout("tcp4", target, 5*time.Second)
	if err == nil {
		_ = conn.Close()
		return "open"
	}
	var netErr net.Error
	if errors.Is(err, syscall.EHOSTUNREACH) ||
		errors.Is(err, syscall.ENETUNREACH) || errors.As(err, &netErr) && netErr.Timeout() {
		return "closed"
	}
	return "unknown"
}

func ipv6() string {
	_, err := os.Stat("/proc/net/if_inet6")
	if err == nil {
		return "present"
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "unknown"
	}
	fd, socketErr := syscall.Socket(syscall.AF_INET6, syscall.SOCK_STREAM, 0)
	if socketErr == nil {
		_ = syscall.Close(fd)
		return "present"
	}
	if errors.Is(socketErr, syscall.EAFNOSUPPORT) {
		return "absent"
	}
	return "unknown"
}

// Match the IPv4 address to the default route's interface. /proc/net/route
// stores gateway bytes in native order (the guest is little-endian arm64).
func guestNetwork() (isolation.Network, bool) {
	data, err := os.ReadFile("/proc/net/route")
	if err != nil {
		return isolation.Network{}, false
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) < 8 || f[1] != "00000000" || f[7] != "00000000" {
			continue
		}
		gateway, err := strconv.ParseUint(f[2], 16, 32)
		if err != nil || gateway == 0 {
			continue
		}
		iface, err := net.InterfaceByName(f[0])
		if err != nil {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		var gatewayBytes [4]byte
		binary.LittleEndian.PutUint32(gatewayBytes[:], uint32(gateway))
		gw := netip.AddrFrom4(gatewayBytes)
		for _, addr := range addrs {
			prefix, err := netip.ParsePrefix(addr.String())
			if err == nil && prefix.Addr().Is4() && prefix.Contains(gw) {
				return isolation.Network{Prefix: prefix, Gateway: gw}, true
			}
		}
	}
	return isolation.Network{}, false
}
