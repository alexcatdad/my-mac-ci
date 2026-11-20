// Package dispatch reconciles listener demand with local runner capacity.
package dispatch

import (
	"context"
	"crypto/rand"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/alexcatdad/my-mac-ci/internal/config"
	"github.com/alexcatdad/my-mac-ci/internal/github"
	"github.com/alexcatdad/my-mac-ci/internal/isolation"
	"github.com/alexcatdad/my-mac-ci/internal/launcher"
	"github.com/alexcatdad/my-mac-ci/internal/scheduler"
	"github.com/alexcatdad/my-mac-ci/internal/usage"
)

type startFunc func(context.Context, string, scheduler.Key, string, config.Resources, string, func(func() error) error) (<-chan error, error)

type label struct {
	removeIdle                              func(context.Context, string) (bool, error)
	checkingIdle                            bool
	capacity                                int
	created                                 time.Time
	retired, waitingImage, imageUnavailable bool
	imageCancel                             context.CancelFunc
	config.Label
	assigned                               int
	jit                                    func(context.Context, string) (string, error)
	retry                                  time.Time
	backoff                                time.Duration
	lastContact                            time.Time
	startFailed, conflict, listenerFailure bool
	versionBehind                          *bool
}

type runner struct {
	job                 usage.Job
	sample              measurement
	sampleAdopted       time.Time
	stopSample          context.CancelFunc
	measured            bool
	idleAfter           time.Time
	recovered, retiring bool
	key                 scheduler.Key
	cancel              context.CancelFunc
	launched            bool
	working             bool
	grace               *time.Timer
}

type Controller struct {
	measurements     *jobMeasurements
	draining         bool
	imagesChanged    chan struct{}
	listenersChanged chan struct{}
	reloads          chan reloadRequest
	mu               sync.Mutex
	scheduler        *scheduler.Scheduler
	labels           map[scheduler.Key]*label
	runners          map[string]*runner
	status           isolation.Status
	wake             chan struct{}
	start            startFunc
	current          func() bool
	created          time.Time
	report           func() (isolation.Report, int64)
}

func New(cfg config.Config, start startFunc) *Controller {
	c := &Controller{
		listenersChanged: make(chan struct{}, 1), reloads: make(chan reloadRequest), imagesChanged: make(chan struct{}, 1), labels: make(map[scheduler.Key]*label), runners: make(map[string]*runner),
		wake: make(chan struct{}, 1), start: start, created: time.Now(),
	}
	labels := make(map[scheduler.Key]scheduler.Label)
	for repo, entries := range cfg.Repos {
		for name, entry := range entries {
			key := scheduler.Key{Repo: repo, Label: name}
			labels[key] = scheduler.Label{Resources: scheduler.Resources(entry.Resources), Max: entry.Max}
			entry.Image = cfg.Registry + "/" + entry.Image
			c.labels[key] = &label{Label: entry, capacity: listenerCapacity(cfg.Cap, entry), created: c.created}
		}
	}
	c.scheduler = scheduler.New(scheduler.Resources(cfg.Cap), labels)
	return c
}

// recover accounts for the whole inventory before any listener can acquire a
// job. Wait retains the reservation through transient inspection failures.
func (c *Controller) recover(ctx context.Context, survivors []launcher.Survivor, workers *sync.WaitGroup) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, s := range survivors {
		if err := c.scheduler.Adopt(s.Name, s.Key, scheduler.Resources(s.Resources)); err != nil {
			return err
		}
	}
	for _, s := range survivors {
		stop, cancel := context.WithCancel(context.WithoutCancel(ctx))
		r := &runner{key: s.Key, cancel: cancel, launched: true, working: true, recovered: true, job: allocation(s.Key, s.Resources, "")}
		r.job.ImageDigest = s.ImageDigest
		c.runners[s.Name] = r
		c.startMeasurement(r, github.Job{Runner: s.Name})
		r.sampleAdopted = time.Now()
		workers.Go(func() {
			gone := s.Wait(ctx, stop)
			if !gone && stop.Err() != nil {
				// A confirmed failure or elapsed grace has already requested
				// removal. Finish it even if the monitor now shuts down.
				gone = s.Wait(context.WithoutCancel(ctx), stop)
			}
			c.mu.Lock()
			defer c.mu.Unlock()
			if gone {
				c.release(s.Name)
			} else {
				// The service detached. Keep its final accounting intact.
				if r := c.runners[s.Name]; r.grace != nil {
					r.grace.Stop()
				}
				cancel()
			}
		})
	}
	return nil
}

// release runs under the controller lock once disappearance is confirmed.
func (c *Controller) release(name string) {
	r := c.runners[name]
	c.finishMeasurement(r)
	if r.grace != nil {
		r.grace.Stop()
	}
	r.cancel()
	delete(c.runners, name)
	c.scheduler.Release(name)
	c.notifyListeners()
	c.signal()
}

func (c *Controller) signal() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// Update receives one complete poll. A nil JIT marks a lost session. Demand
// must be observed on the replacement session before that label can start.
func (c *Controller) Update(key scheduler.Key, u github.Update) {
	c.mu.Lock()
	defer c.mu.Unlock()
	l := c.labels[key]
	if l == nil {
		return
	}
	if u.JIT != nil {
		l.lastContact = time.Now()
		l.conflict, l.listenerFailure = false, false
	}
	for _, job := range u.Started {
		if r := c.runners[job.Runner]; r != nil && r.key == key {
			c.startMeasurement(r, job)
			r.working = true
			r.recovered = false
			l.startFailed = false
		}
	}
	assigned := u.Assigned
	if u.JIT == nil {
		assigned = new(int)
	}
	if u.JIT == nil || u.Assigned != nil {
		l.removeIdle = u.RemoveIdle
	}
	l.jit = u.JIT
	if assigned != nil {
		if *assigned > l.assigned || u.JIT == nil {
			for _, r := range c.runners {
				if r.key == key {
					r.idleAfter = time.Time{}
				}
			}
		}
		if *assigned < l.assigned || u.JIT == nil {
			for _, r := range c.runners {
				if r.key == key && !r.launched {
					r.cancel()
				}
			}
		}
		l.assigned = *assigned
	}
	var completed []string
	for _, job := range u.Completed {
		completed = append(completed, job.Runner)
		c.completeMeasurement(key, job)
		if r := c.runners[job.Runner]; r != nil && r.key == key && r.grace == nil {
			// A late timer owns only the context of this exact reservation.
			r.grace = time.AfterFunc(30*time.Second, r.cancel)
		}
	}
	c.scheduler.Update(key, assigned, completed)
	c.signal()
}

func (c *Controller) SetIsolation(status isolation.Status) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.setIsolation(status)
}

func (c *Controller) setIsolation(status isolation.Status) {
	c.status = status
	for _, r := range c.runners {
		if status == isolation.Fail || (status != isolation.Pass && !r.launched) {
			r.cancel()
		}
	}
	c.signal()
}

// Run has one reconciliation loop. Completion and exit notifications wake it;
// failed starts additionally set a timer, so idle queues are retried.
func (c *Controller) Run(ctx context.Context) {
	var running sync.WaitGroup
	defer func() {
		c.mu.Lock()
		for _, r := range c.runners {
			if !r.launched {
				r.cancel()
			}
		}
		c.mu.Unlock()
		running.Wait()
	}()
	for ctx.Err() == nil {
		c.mu.Lock()
		earliest := c.reconcileIdle(ctx, &running)
		if !c.draining && c.status == isolation.Pass {
			now := time.Now()
			name := "mmc-" + strings.ToLower(rand.Text())
			key, ok := c.scheduler.Reserve(name, func(k scheduler.Key) bool {
				l := c.labels[k]
				if l.jit == nil || l.waitingImage || l.retired {
					return false
				}
				if l.retry.After(now) {
					if earliest.IsZero() || l.retry.Before(earliest) {
						earliest = l.retry
					}
					return false
				}
				return true
			})
			if ok {
				runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
				l := c.labels[key]
				r := &runner{key: key, cancel: cancel, job: allocation(key, l.Resources, l.Image)}
				c.runners[name] = r
				jit, image, resources := l.jit, l.Image, l.Resources
				c.mu.Unlock()
				running.Go(func() { c.launch(ctx, runCtx, name, r, jit, image, resources) })
				continue
			}
		}
		c.mu.Unlock()
		var timer <-chan time.Time
		if !earliest.IsZero() {
			timer = time.After(time.Until(earliest))
		}
		select {
		case <-ctx.Done():
		case <-c.wake:
		case <-timer:
		}
	}
}

func (c *Controller) launch(service, ctx context.Context, name string, r *runner, jit func(context.Context, string) (string, error), image string, resources config.Resources) {
	failed, detached := false, false
	defer func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if detached {
			if r.grace != nil {
				r.grace.Stop()
			}
			return
		}
		if ctx.Err() == nil {
			l := c.labels[r.key]
			// A clean exit before JobCompleted can leave demand stale.
			// Apply the usual retry delay before launching a replacement.
			if failed || r.grace == nil {
				l.startFailed = true
				l.backoff = min(time.Minute, max(time.Second, 2*l.backoff))
				l.retry = time.Now().Add(l.backoff)
				slog.Warn("runner failed, retry scheduled")
			} else {
				l.startFailed = false
				l.backoff = 0
			}
		}
		c.release(name)
	}()

	jitCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	credential, err := jit(jitCtx, name)
	expired := jitCtx.Err() != nil
	cancel()
	if err != nil || expired || credential == "" {
		failed = true
		return
	}
	authorize := func(spawn func() error) error {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.current != nil && c.status == isolation.Pass && !c.current() {
			c.setIsolation(isolation.Inconclusive)
		}
		if ctx.Err() != nil || service.Err() != nil || c.draining || c.status != isolation.Pass {
			return context.Canceled
		}
		if err := spawn(); err != nil {
			return err
		}
		r.launched = true
		return nil
	}
	exit, err := c.start(ctx, name, r.key, image, resources, credential, authorize)
	if err != nil || exit == nil {
		failed = true
		return
	}
	select {
	case err := <-exit:
		failed = err != nil
	case <-service.Done():
		if ctx.Err() != nil {
			failed = <-exit != nil
		} else {
			// An app crash closes its lifeline. Leave the attached container
			// CLI and VM running for the next helper to recover.
			detached = true
		}
	}
}
