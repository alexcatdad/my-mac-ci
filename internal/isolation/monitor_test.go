package isolation

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func TestMonitorRequiresFreshPassAtLaunch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		m := NewMonitor(Options{})
		var dirty atomic.Bool
		results := make(chan Status)
		status := make(chan Status, 8)
		done := make(chan error, 1)
		go func() {
			done <- m.run(ctx, func(s Status) { status <- s },
				func() (bool, error) { return dirty.Swap(false), nil },
				func(context.Context) Status { return <-results })
		}()
		expect := func(want Status) {
			t.Helper()
			if got := <-status; got != want || m.Current() != (want == Pass) {
				t.Fatalf("expected %s, got %s with launch gate %v", want, got, m.Current())
			}
		}

		expect(Inconclusive)
		dirty.Store(true) // a wake or network event during this probe
		results <- Pass
		expect(Inconclusive)
		results <- Pass
		expect(Pass)

		dirty.Store(true) // launch checks the OS before the next idle tick
		if m.Current() {
			t.Fatal("launch used a pass from before a host change")
		}
		expect(Inconclusive)
		results <- Fail
		expect(Fail)

		dirty.Store(true)
		time.Sleep(2 * time.Second)
		expect(Inconclusive)
		results <- Pass
		expect(Pass)
		time.Sleep(time.Hour)
		expect(Inconclusive)
		results <- Pass
		expect(Pass)

		cancel()
		if m.Current() || !errors.Is(<-done, context.Canceled) {
			t.Fatal("monitor retained a pass at shutdown")
		}
		expect(Inconclusive)
	})
}

func TestMonitorInvalidatesPassWhenObservationFails(t *testing.T) {
	m := NewMonitor(Options{})
	var samples int
	changed := func() (bool, error) {
		samples++
		if samples > 2 {
			return false, errors.New("unavailable")
		}
		return false, nil
	}
	var states []Status
	err := m.run(t.Context(), func(s Status) {
		states = append(states, s)
		if m.Current() {
			t.Fatal("monitor authorized launch after observer failed")
		}
	}, changed, func(context.Context) Status { return Pass })
	if err != errHostEvents || states[len(states)-1] != Inconclusive {
		t.Fatal("lost host observer must invalidate launch permission")
	}
}

func TestConcurrentChangeDuringProbeAttachment(t *testing.T) {
	const prefix = "State:/Network/Interface"
	before := map[string]int{"en0": 1}
	after := map[string]int{"en0": 1, "vmenet1": 2, "bridge1": 3}
	owned := map[string]bool{"vmenet1": true, "bridge1": true}
	keys := []string{prefix, prefix + "/vmenet1/Link", prefix + "/bridge1/IPv4"}
	if probeChanges(keys, before, after, owned) || probeChanges(keys, after, before, owned) {
		t.Fatal("verified probe attachment or removal invalidated the scan")
	}
	for _, external := range []string{prefix + "/en0/Link", prefix + "/vmenet2/Link", "State:/Network/Global/IPv4", "Setup:/Network/Service/example"} {
		if !probeChanges(append(keys, external), before, after, owned) {
			t.Fatal("external notification during probe attachment was ignored")
		}
	}
	if !probeChanges(keys, after, after, owned) {
		t.Fatal("steady-state change was attributed to a lifecycle transition")
	}
	after["utun2"] = 4
	if !probeChanges(keys, before, after, owned) {
		t.Fatal("unrelated interface appeared during probe attachment")
	}
}
