package isolation

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Options struct {
	Image           string `json:"image"`
	HostLAN         string `json:"hostLAN"`
	TailnetResolver string `json:"tailnetResolver"`
	ProbeBinary     string `json:"probeBinary"`
}

type Status string

const (
	Pass         Status = "pass"
	Fail         Status = "fail"
	Inconclusive Status = "inconclusive"
)

type CheckResult struct {
	Name   string `json:"name"`
	Status Status `json:"status"`
}

type Report struct {
	Status Status
	Checks []CheckResult
}

func (r *Report) add(name string, status Status) {
	r.Checks = append(r.Checks, CheckResult{name, status})
	if status == Fail || r.Status != Fail && status == Inconclusive {
		r.Status = status
	}
}

type command func(context.Context, []byte, ...string) ([]byte, error)

func container(ctx context.Context, input []byte, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "container", args...)
	cmd.WaitDelay = time.Second
	cmd.Stdin = bytes.NewReader(input)
	return cmd.Output()
}

func (o Options) Valid() bool {
	name, digest, ok := strings.Cut(o.Image, "@sha256:")
	_, err := hex.DecodeString(digest)
	lan, errLAN := netip.ParseAddr(o.HostLAN)
	resolver, errResolver := netip.ParseAddr(o.TailnetResolver)
	return ok && name != "" && name[0] != '-' && !strings.ContainsAny(name, "@ \t\r\n") && len(digest) == 64 && err == nil &&
		errLAN == nil && lan.Is4() && lan.IsGlobalUnicast() &&
		errResolver == nil && resolver.Is4() && resolver.IsGlobalUnicast() && o.ProbeBinary != ""
}

func localAddress(ip string) bool {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return false
	}
	for _, addr := range addrs {
		if prefix, err := netip.ParsePrefix(addr.String()); err == nil && prefix.Addr().String() == ip {
			return true
		}
	}
	return false
}

// Check runs only disposable probes. Callers receive fixed target names and
// classifications, never container output, endpoint addresses, or OS errors.
func Check(ctx context.Context, o Options) (report Report) {
	return check(ctx, o, nil)
}

type probeWindow func(context.Context, []string)

func check(ctx context.Context, o Options, window probeWindow) (report Report) {
	report.Status = Pass
	if !o.Valid() || !localAddress(o.HostLAN) {
		report.add("probe settings", Inconclusive)
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if _, err := container(ctx, nil, "image", "inspect", o.Image); err != nil {
		report.add("local probe image", Inconclusive)
		return
	}

	dir, err := os.MkdirTemp("", "mmc-probe-*")
	if err != nil {
		report.add("probe staging", Inconclusive)
		return
	}
	defer func() {
		if err := os.RemoveAll(dir); err != nil {
			report.add("probe staging cleanup", Inconclusive)
		}
	}()
	// The read-only guest mount contains only the standalone guest executable.
	mount := filepath.Join(dir, "guest")
	if !stageBinary(o.ProbeBinary, mount) || strings.Contains(mount, ",") {
		report.add("probe executable", Inconclusive)
		return
	}
	host, err := net.Listen("tcp4", ":"+ProbePort)
	if err != nil {
		report.add("host listener", Inconclusive)
		return
	}
	defer func() { _ = host.Close() }()
	go func() {
		for {
			conn, err := host.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	runChecks(ctx, &report, o, dir, mount, container, window)
	return
}

func stageBinary(path, dir string) bool {
	source, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = source.Close() }()
	info, err := source.Stat()
	if err != nil || !info.Mode().IsRegular() || os.Mkdir(dir, 0755) != nil {
		return false
	}
	target := filepath.Join(dir, "probe")
	dest, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0555)
	if err != nil {
		return false
	}
	_, err = io.Copy(dest, source)
	closeErr := dest.Close()
	return err == nil && closeErr == nil && os.Chmod(target, 0555) == nil && os.Chmod(dir, 0755) == nil
}

const guestPath = "/mmc-probe/probe"

func runChecks(ctx context.Context, report *Report, o Options, dir, mount string, run command, window probeWindow) {
	networks := [...]string{"default", "mmc"}
	var metadata [2]Network
	var names [3]string
	var needsCleanup [3]bool
	for i := range names {
		names[i] = "mmc-probe-" + strings.ToLower(rand.Text())
	}
	// The runtime may have created a VM before a timed-out CLI returned.
	// Its cidfile confirms ownership in that case. All probes also self-exit
	// and use --rm if the helper itself disappears.
	defer func() {
		for i, name := range names {
			if !needsCleanup[i] {
				continue
			}
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			_, err := run(cleanupCtx, nil, "rm", "-f", name)
			cancel()
			if err != nil {
				report.add("probe cleanup", Inconclusive)
			}
		}
	}()
	launch := func(i int, mode string) error {
		cidfile := filepath.Join(dir, names[i]+".id")
		args := []string{"run", "--detach", "--rm", "--name", names[i], "--cidfile", cidfile, "--network", networks[i%2],
			"--dns", "1.1.1.1", "--cpus", "1", "--memory", "256m", "--mount", "type=bind,source=" + mount + ",target=/mmc-probe,readonly", "--entrypoint", guestPath}
		if i%2 != 0 {
			args = append(args, "--kernel-arg", "ipv6.disable=1")
		}
		_, err := run(ctx, nil, append(args, o.Image, mode)...)
		if err == nil {
			needsCleanup[i] = true
		} else if id, readErr := os.ReadFile(cidfile); readErr == nil && strings.TrimSpace(string(id)) == names[i] {
			needsCleanup[i] = true
		}
		return err
	}
	for i, mode := range []string{"listen", "idle", "idle"} {
		if err := launch(i, mode); err != nil {
			report.add("probe startup", Inconclusive)
			return
		}
		if i == 2 {
			break
		}
		out, err := run(ctx, nil, "exec", names[i], guestPath, "network")
		if err != nil || json.Unmarshal(out, &metadata[i]) != nil || !validNetwork(metadata[i]) {
			report.add("probe network metadata", Inconclusive)
			return
		}
		for j := range i {
			if metadata[i].Prefix.Overlaps(metadata[j].Prefix) {
				report.add("distinct probe subnets", Inconclusive)
				return
			}
		}
	}
	targets := []string{"github.com:443", net.JoinHostPort(metadata[0].Prefix.Addr().String(), ProbePort),
		net.JoinHostPort(o.HostLAN, ProbePort), net.JoinHostPort(o.TailnetResolver, "53")}
	jobTargets := append(targets, net.JoinHostPort(metadata[1].Gateway.String(), ProbePort))
	scans := [2][]string{targets, jobTargets}
	var observations [2]Observations
	scan := func() {
		var group sync.WaitGroup
		for i := range networks {
			group.Go(func() {
				request, err := json.Marshal(Scan{Network: metadata[i], Targets: scans[i]})
				if err != nil {
					return
				}
				out, err := run(ctx, request, "exec", "-i", names[2-i], guestPath, "scan")
				if err != nil || json.Unmarshal(out, &observations[i]) != nil || len(observations[i].TCP) != len(scans[i]) {
					observations[i] = Observations{}
				}
			})
		}
		group.Wait()
	}
	if window != nil {
		// Send traffic before looking up the VM ports: a bridge learns a
		// guest MAC only after receiving a frame from that guest.
		scan()
		var initial Report
		evaluate(&initial, observations)
		if initial.Status == Fail {
			*report = initial
			return
		}
		window(ctx, names[:])
	}
	scan()
	if window != nil {
		window(ctx, nil)
	}
	evaluate(report, observations)
}

func validNetwork(n Network) bool {
	ip := n.Prefix.Addr()
	return n.Prefix.IsValid() && ip.Is4() && ip.IsGlobalUnicast() &&
		n.Gateway.Is4() && n.Gateway.IsGlobalUnicast() && n.Prefix.Contains(n.Gateway) && ip != n.Gateway
}

func evaluate(report *Report, obs [2]Observations) {
	sources := [...]string{"control", "mmc"}
	names := [...]string{"GitHub", "default network listener", "host LAN", "tailnet resolver"}
	get := func(src, dst int) string {
		if len(obs[src].TCP) != len(names)+src {
			return "unknown"
		}
		return obs[src].TCP[dst]
	}
	for src, source := range sources {
		for dst, target := range names {
			positive := src == 0 || dst == 0
			got := get(src, dst)
			status := Inconclusive
			switch {
			case positive && got == "open":
				status = Pass
			case !positive && got == "open":
				status = Fail
			case !positive && got == "closed" && get(0, dst) == "open":
				status = Pass
			}
			report.add(source+" to "+target, status)
		}
		if src > 0 {
			// Both host addresses serve the same listener. Verify it via the LAN
			// while the job probe independently confirms internet access.
			gateway := Inconclusive
			switch get(src, len(names)) {
			case "open":
				gateway = Fail
			case "closed":
				if get(src, 0) == "open" && get(0, 2) == "open" {
					gateway = Pass
				}
			}
			report.add(source+" to job gateway", gateway)
			switch obs[src].IPv6 {
			case "absent":
				report.add(source+" IPv6 disabled", Pass)
			case "present":
				report.add(source+" IPv6 disabled", Fail)
			default:
				report.add(source+" IPv6 disabled", Inconclusive)
			}
		}
	}
}
