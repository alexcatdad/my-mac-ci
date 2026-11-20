package scheduler

import (
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
)

func wantStart(t *testing.T, s *Scheduler, name string, want Key) {
	t.Helper()
	got, ok := s.Reserve(name, nil)
	if !ok || got != want {
		t.Fatalf("Reserve(%q) = (%v, %v), want %v", name, got, ok, want)
	}
}

func wantWait(t *testing.T, s *Scheduler) {
	t.Helper()
	if got, ok := s.Reserve("waiting", nil); ok {
		t.Fatalf("expected to wait, got %v", got)
	}
}

func TestRotationDemandAndCaps(t *testing.T) {
	a := Key{"owner/a", "linux"}
	b := Key{"owner/b", "linux"}
	c := Key{"owner/c", "large"}
	s := New(Resources{6, 8}, map[Key]Label{
		a: {Resources: Resources{1, 2}, Max: 1},
		b: {Resources: Resources{1, 1}},
		c: {Resources: Resources{4, 6}},
	})
	for _, key := range []Key{a, b, c} {
		s.Update(key, new(4), nil)
	}
	wantStart(t, s, "a1", a)
	wantStart(t, s, "b1", b)
	wantStart(t, s, "b2", b) // large cannot fit available memory, a is capped
	s.Update(b, new(1), nil)
	wantWait(t, s)
	s.Release("a1")
	wantStart(t, s, "c1", c)
	wantWait(t, s)
}

func TestCompletionAndStartFailure(t *testing.T) {
	key := Key{"owner/repo", "linux"}
	s := New(Resources{1, 8}, map[Key]Label{key: {Resources: Resources{1, 1}}})
	s.Update(key, new(1), nil)
	wantStart(t, s, "first", key)
	s.Update(key, new(1), []string{"first"})
	wantWait(t, s) // completion releases demand, but CPU is held until exit
	s.Release("first")
	wantStart(t, s, "second", key)
	s.Release("second") // failed start, last known demand still applies
	wantStart(t, s, "third", key)
}

func TestAdoptAndReload(t *testing.T) {
	a, b := Key{"owner/repo", "a"}, Key{"owner/repo", "b"}
	s := New(Resources{4, 8}, map[Key]Label{a: {Resources: Resources{1, 1}}})
	beforeReload := Resources{3, 6}
	if err := s.Adopt("survivor", a, beforeReload); err != nil {
		t.Fatal(err)
	}
	s.Update(a, new(1), nil)
	wantWait(t, s) // recovered active job fills the target
	s.Update(a, nil, []string{"survivor"})
	if err := s.Adopt("survivor", a, beforeReload); err != nil {
		t.Fatal(err)
	}
	wantStart(t, s, "next", a) // repeat adoption retained the completed state
	s.Release("next")
	s.Reload(Resources{2, 4}, map[Key]Label{b: {Resources: Resources{1, 1}}})
	s.Update(a, new(8), nil) // stale demand for removed label
	s.Update(b, new(1), nil)
	wantWait(t, s)
	s.Release("survivor")
	wantStart(t, s, "after", b)
}

func TestConcurrentReservations(t *testing.T) {
	key := Key{"owner/repo", "linux"}
	s := New(Resources{4, 4}, map[Key]Label{key: {Resources: Resources{1, 1}}})
	s.Update(key, new(32), nil)
	var count atomic.Int32
	var wg sync.WaitGroup
	for i := range 32 {
		wg.Go(func() {
			if _, ok := s.Reserve(strconv.Itoa(i), nil); ok {
				count.Add(1)
			}
		})
	}
	wg.Wait()
	if got := count.Load(); got != 4 {
		t.Fatalf("reserved %d runners, want 4", got)
	}
}
