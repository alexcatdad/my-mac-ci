// Package isolation verifies the behaviour of prepared job networks.
package isolation

import "net/netip"

const ProbePort = "18765"

type Network struct {
	Prefix  netip.Prefix
	Gateway netip.Addr
}

// The scan request travels over stdin. Probe output contains only observations.
type Scan struct {
	Network Network
	Targets []string
}

type Observations struct {
	TCP  []string // open, closed, unknown
	IPv6 string   // present, absent, unknown
}
