package isolation

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func baseline() [2]Observations {
	return [2]Observations{
		{TCP: []string{"open", "open", "open", "open"}},
		{TCP: []string{"open", "closed", "closed", "closed", "closed"}, IPv6: "absent"},
	}
}

func TestVerdicts(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*[2]Observations)
		want   Status
	}{
		{"isolated", func(o *[2]Observations) {}, Pass},
		{"unverified host", func(o *[2]Observations) { o[0].TCP[2] = "closed" }, Inconclusive},
		{"reachable job gateway", func(o *[2]Observations) { o[1].TCP[4] = "open" }, Fail},
		{"unverified job internet", func(o *[2]Observations) { o[1].TCP[0] = "closed" }, Inconclusive},
		{"unknown gateway", func(o *[2]Observations) { o[1].TCP[4] = "unknown" }, Inconclusive},
		{"ambiguous TCP reset", func(o *[2]Observations) { o[1].TCP[1] = "unknown" }, Inconclusive},
		{"missing observations", func(o *[2]Observations) { o[1] = Observations{} }, Inconclusive},
		{"positive leak with unreachable control", func(o *[2]Observations) { o[0] = Observations{}; o[1].TCP[4] = "open" }, Fail},
		{"IPv6 present", func(o *[2]Observations) { o[1].IPv6 = "present" }, Fail},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obs := baseline()
			tc.mutate(&obs)
			report := Report{Status: Pass}
			evaluate(&report, obs)
			if report.Status != tc.want {
				t.Errorf("got %s, want %s", report.Status, tc.want)
			}
		})
	}
}

func TestPartialStartupCleanup(t *testing.T) {
	dir := t.TempDir()
	var created, removed []string
	run := func(ctx context.Context, _ []byte, args ...string) ([]byte, error) {
		switch args[0] {
		case "run":
			name := args[slices.Index(args, "--name")+1]
			created = append(created, name)
			if len(created) == 2 {
				cidfile := args[slices.Index(args, "--cidfile")+1]
				if err := os.WriteFile(cidfile, []byte(name), 0600); err != nil {
					t.Fatal(err)
				}
				return nil, context.DeadlineExceeded
			}
		case "exec":
			return json.Marshal(Network{Prefix: netip.MustParsePrefix("192.0.2.2/24"), Gateway: netip.MustParseAddr("192.0.2.1")})
		case "rm":
			if _, bounded := ctx.Deadline(); !bounded {
				t.Fatal("cleanup has no deadline")
			}
			removed = append(removed, args[2])
		default:
			return nil, errors.New("unexpected command")
		}
		return nil, nil
	}
	report := Report{Status: Pass}
	runChecks(context.Background(), &report, Options{}, dir, dir, run, nil)
	if report.Status != Inconclusive || len(created) != 2 || !reflect.DeepEqual(created, removed) {
		t.Fatal("partial startup did not clean up both owned containers")
	}
}

func TestProbeScanFailures(t *testing.T) {
	for _, tc := range []struct {
		name    string
		control bool
		output  string
		err     error
		want    Status
	}{
		{"passing scan", false, `{"TCP":["open","closed","closed","closed","closed"],"IPv6":"absent"}`, nil, Pass},
		{"timeout with otherwise passing output", false, `{"TCP":["open","closed","closed","closed","closed"],"IPv6":"absent"}`, context.DeadlineExceeded, Inconclusive},
		{"malformed output", false, `{"TCP":`, nil, Inconclusive},
		{"missing output", false, "", nil, Inconclusive},
		{"missing target", false, `{"TCP":["open","closed","closed","closed"],"IPv6":"absent"}`, nil, Inconclusive},
		{"failed positive control", true, `{"TCP":["open","open","closed","open"]}`, nil, Inconclusive},
		{"reachable prohibited target", false, `{"TCP":["open","open","closed","closed","closed"],"IPv6":"absent"}`, nil, Fail},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var job string
			run := func(_ context.Context, input []byte, args ...string) ([]byte, error) {
				switch args[len(args)-1] {
				case "network":
					prefix := "192.0.2.2/24"
					gateway := "192.0.2.1"
					if args[1] == job {
						prefix, gateway = "198.51.100.2/24", "198.51.100.1"
					}
					return json.Marshal(Network{Prefix: netip.MustParsePrefix(prefix), Gateway: netip.MustParseAddr(gateway)})
				case "scan":
					var request Scan
					if err := json.Unmarshal(input, &request); err != nil {
						return nil, err
					}
					index := 0
					if request.Network.Prefix.Addr() == netip.MustParseAddr("198.51.100.2") {
						index = 1
					}
					if (index == 0) == tc.control {
						return []byte(tc.output), tc.err
					}
					return json.Marshal(baseline()[index])
				}
				if i := slices.Index(args, "--network"); i >= 0 && args[i+1] == "mmc" {
					job = args[slices.Index(args, "--name")+1]
				}
				return nil, nil
			}
			dir := t.TempDir()
			report := Report{Status: Pass}
			runChecks(t.Context(), &report, Options{HostLAN: "203.0.113.2", TailnetResolver: "203.0.113.3"}, dir, dir, run, nil)
			if report.Status != tc.want {
				t.Errorf("got %s, want %s", report.Status, tc.want)
			}
		})
	}
}

func TestInvalidSettingsAreRedacted(t *testing.T) {
	report := Check(t.Context(), Options{HostLAN: "sensitive.invalid"})
	output, err := json.Marshal(report)
	if err != nil || report.Status != Inconclusive || strings.Contains(string(output), "sensitive") {
		t.Fatal("invalid input must yield a redacted inconclusive result")
	}
}
