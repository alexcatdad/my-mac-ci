package isolation

import (
	"context"
	"errors"
	"sync"
	"time"
)

var errHostEvents = errors.New("isolation monitor cannot observe host network or wake changes")

type Monitor struct {
	options  Options
	recheck  chan struct{}
	revision uint64

	mu         sync.Mutex
	changed    func() (bool, error)
	ctx        context.Context
	expires    time.Time
	passed     bool
	lastReport Report
	checkedAt  int64
}

func NewMonitor(options Options) *Monitor {
	return &Monitor{options: options, recheck: make(chan struct{}, 1)}
}

// Current rechecks the host's change indicators at the launch boundary. This
// also catches changes before the monitor's idle ticker has delivered them.
func (m *Monitor) Current() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.passed || m.ctx.Err() != nil {
		return false
	}
	changed, err := m.changed()
	if changed || err != nil || !time.Now().Before(m.expires) {
		m.passed = false
		select {
		case m.recheck <- struct{}{}:
		default:
		}
	}
	return m.passed
}

// Run owns host monitoring until cancellation. Only the real behavioural
// check can publish a pass. Callbacks run serially, outside the monitor lock.
func (m *Monitor) Run(ctx context.Context, publish func(Status)) error {
	m.mu.Lock()
	if m.ctx != nil {
		m.mu.Unlock()
		return errors.New("isolation monitor already started")
	}
	m.ctx = ctx
	m.mu.Unlock()
	for ctx.Err() == nil {
		events, closeSource, err := observeHost()
		if err != nil {
			publish(Inconclusive)
		} else {
			var teardown map[string]bool
			var invalid bool
			changed := func() (bool, error) {
				dirty, err := events(teardown)
				dirty = dirty || invalid
				invalid = false
				teardown = nil
				return dirty, err
			}
			err = m.run(ctx, publish, changed, func(ctx context.Context) Status {
				m.mu.Lock()
				options, revision := m.options, m.revision
				m.mu.Unlock()
				report := check(ctx, options, func(ctx context.Context, names []string) {
					var allowed map[string]bool
					if names != nil {
						var err error
						allowed, err = probeLinks(ctx, names)
						if err != nil {
							invalid = true
						}
						teardown = allowed
					}
					// The scan between these two checkpoints has no VM lifecycle
					// operations. Every network change in that interval is external.
					dirty, err := events(allowed)
					invalid = invalid || dirty || err != nil
				})
				m.mu.Lock()
				if revision == m.revision {
					m.lastReport, m.checkedAt = report, time.Now().Unix()
				}
				m.mu.Unlock()
				return report.Status
			})
			closeSource()
			if !errors.Is(err, errHostEvents) {
				return err
			}
		}
		// A host observer outage suspends starts while active jobs finish.
		// A replacement observer always starts with a new behavioural check.
		select {
		case <-ctx.Done():
		case <-time.After(time.Minute):
		}
	}
	return ctx.Err()
}

func (m *Monitor) run(ctx context.Context, publish func(Status), changed func() (bool, error), check func(context.Context) Status) error {
	m.mu.Lock()
	m.ctx, m.changed = ctx, changed
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.passed = false
		m.mu.Unlock()
		publish(Inconclusive)
	}()

	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for ctx.Err() == nil {
		m.mu.Lock()
		m.passed = false
		select {
		case <-m.recheck:
		default:
		}
		_, err := changed() // consume changes which this probe is about to test
		revision := m.revision
		m.expires = time.Now().Add(time.Hour)
		m.mu.Unlock()
		publish(Inconclusive)
		if err != nil {
			return errHostEvents
		}
		result := check(ctx)
		m.mu.Lock()
		stale, err := changed()
		reconfigured := revision != m.revision
		stale = stale || reconfigured
		m.passed = result == Pass && !stale && err == nil && ctx.Err() == nil
		m.mu.Unlock()

		if result == Fail && !reconfigured {
			publish(Fail)
		}
		if err != nil {
			return errHostEvents
		}
		if stale || ctx.Err() != nil {
			continue
		}
		if result != Fail {
			publish(result)
		}

	wait:
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-m.recheck:
				break wait
			case <-tick.C:
				m.mu.Lock()
				dirty, err := changed()
				due := dirty || err != nil || !time.Now().Before(m.expires)
				if due {
					m.passed = false
				}
				m.mu.Unlock()
				if err != nil {
					return errHostEvents
				}
				if due {
					break wait
				}
			}
		}
	}
	return ctx.Err()
}

// LastReport exposes only the fixed probe target names and classifications.
// Reports become immutable when the probe finishes.
func (m *Monitor) LastReport() (Report, int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastReport, m.checkedAt
}

// Reconfigure invalidates the previous result before another launch can pass.
func (m *Monitor) Reconfigure(o Options) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.options = o
	m.revision++
	m.passed = false
	select {
	case m.recheck <- struct{}{}:
	default:
	}
}
