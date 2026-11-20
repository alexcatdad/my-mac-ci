package dispatch

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/alexcatdad/my-mac-ci/internal/scheduler"
)

// Called under c.mu. Count working jobs toward demand, but only registrations
// without an observed JobStarted can be surplus. Recovered VMs have unknown
// job state until the service confirms it.
func (c *Controller) reconcileIdle(ctx context.Context, workers *sync.WaitGroup) time.Time {
	count := make(map[scheduler.Key]int)
	var candidates []string
	for name, r := range c.runners {
		if !r.launched || r.grace != nil || r.retiring {
			continue
		}
		count[r.key]++
		if !r.working || r.recovered {
			candidates = append(candidates, name)
		}
	}
	// Keep the same candidates through successive polls and grace intervals.
	slices.Sort(candidates)
	var earliest time.Time
	now := time.Now()
	for _, name := range candidates {
		r := c.runners[name]
		l := c.labels[r.key]
		if l == nil || l.removeIdle == nil || count[r.key] <= l.assigned {
			r.idleAfter = time.Time{}
			continue
		}
		count[r.key]--
		if r.idleAfter.IsZero() {
			r.idleAfter = now.Add(30 * time.Second)
		}
		if l.checkingIdle {
			continue
		}
		if r.idleAfter.After(now) {
			if earliest.IsZero() || r.idleAfter.Before(earliest) {
				earliest = r.idleAfter
			}
			continue
		}
		l.checkingIdle = true
		remove := l.removeIdle
		workers.Go(func() {
			bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			removed, err := remove(bounded, name)
			c.mu.Lock()
			defer c.mu.Unlock()
			l.checkingIdle = false
			if c.runners[name] == r {
				switch {
				case err != nil:
					// Ambiguous status keeps this VM alive.
					r.idleAfter = time.Now().Add(30 * time.Second)
				case removed:
					r.retiring = true
					r.cancel()
				default:
					r.working, r.recovered = true, false
				}
			}
			c.signal()
		})
	}
	return earliest
}
