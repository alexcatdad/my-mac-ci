package dispatch

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/alexcatdad/my-mac-ci/internal/config"
	"github.com/alexcatdad/my-mac-ci/internal/github"
	"github.com/alexcatdad/my-mac-ci/internal/isolation"
	"github.com/alexcatdad/my-mac-ci/internal/launcher"
	"github.com/alexcatdad/my-mac-ci/internal/scheduler"
)

var errReload = errors.New("dispatch cannot reload at this time")

type reloadRequest struct {
	cfg    config.Config
	result chan error
}

func (h *Health) Reload(ctx context.Context, cfg config.Config) error {
	c := h.Controller()
	if c == nil {
		return errReload
	}
	req := reloadRequest{cfg, make(chan error, 1)}
	select {
	case c.reloads <- req:
	case <-ctx.Done():
		return errReload
	}
	// Once accepted, the service applies the in-memory edit before it handles
	// any further event, including shutdown.
	return <-req.result
}

func (c *Controller) notifyListeners() {
	select {
	case c.listenersChanged <- struct{}{}:
	default:
	}
}

type liveListener struct {
	cancel  context.CancelFunc
	closing bool
}
type listenerExit struct {
	key scheduler.Key
	err error
}

func (c *Controller) serveListeners(ctx context.Context, cfg config.Config, auth github.Config, monitor isolationMonitor, listen listenFunc, workers *sync.WaitGroup, stopped <-chan error) error {
	live := make(map[scheduler.Key]*liveListener)
	exited := make(chan listenerExit)
	for {
		c.mu.Lock()
		occupied := make(map[scheduler.Key]bool)
		for _, r := range c.runners {
			occupied[r.key] = true
		}
		for k, l := range c.labels {
			cur := live[k]
			if l.retired && !occupied[k] {
				if cur == nil {
					delete(c.labels, k)
				} else if !cur.closing {
					cur.closing = true
					cur.cancel()
				}
				continue
			}
			if cur != nil {
				continue
			}
			if l.retired || c.status != isolation.Pass || !monitor.Current() {
				continue
			}
			lctx, cancel := context.WithCancel(ctx)
			live[k] = &liveListener{cancel: cancel}
			settings := auth
			settings.Repo, settings.Label, settings.MaxRunners = k.Repo, k.Label, l.capacity
			settings.Capacity = func() int {
				c.mu.Lock()
				defer c.mu.Unlock()
				if l.retired || c.draining || l.waitingImage {
					return 0
				}
				return l.capacity
			}
			workers.Go(func() {
				err := listen(lctx, settings, func(u github.Update) { c.Update(k, u) }, func(err error) {
					c.Update(k, github.Update{})
					c.mu.Lock()
					l.listenerFailure = true
					conflict := errors.Is(err, github.ErrSessionConflict)
					l.conflict = conflict
					c.mu.Unlock()
					if conflict {
						slog.Warn("GitHub listener session conflict, retrying")
					} else {
						slog.Warn("GitHub listener unavailable, retrying")
					}
				})
				c.Update(k, github.Update{})
				select {
				case exited <- listenerExit{k, err}:
				case <-ctx.Done():
				}
			})
		}
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-stopped:
			return err
		case <-c.listenersChanged:
		case e := <-exited:
			cur := live[e.key]
			cur.cancel()
			delete(live, e.key)
			if !cur.closing {
				return e.err
			}
		case req := <-c.reloads:
			err := c.apply(ctx, cfg, req.cfg, monitor, workers)
			if err == nil {
				cfg = req.cfg
			}
			req.result <- err
		}
	}
}

// apply replaces limits and image selections under the same lock as the actual
// spawn. Running containers retain their original reservations until release.
func (c *Controller) apply(ctx context.Context, old, cfg config.Config, monitor isolationMonitor, workers *sync.WaitGroup) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.draining {
		return errReload
	}
	if cfg.Isolation != nil && (old.Isolation == nil || *old.Isolation != *cfg.Isolation) {
		m, ok := monitor.(interface{ Reconfigure(isolation.Options) })
		if !ok {
			return errReload
		}
		m.Reconfigure(*cfg.Isolation)
		c.setIsolation(isolation.Inconclusive)
	}
	limits := make(map[scheduler.Key]scheduler.Label)
	for repo, entries := range cfg.Repos {
		for name, entry := range entries {
			k := scheduler.Key{Repo: repo, Label: name}
			limits[k] = scheduler.Label{Resources: scheduler.Resources(entry.Resources), Max: entry.Max}
			entry.Image = cfg.Registry + "/" + entry.Image
			l := c.labels[k]
			if l == nil {
				l = &label{created: time.Now()}
				c.labels[k] = l
			}
			if entry.Image != l.Image || l.retired {
				if l.imageCancel != nil {
					l.imageCancel()
				}
				l.waitingImage = true
				l.imageUnavailable = false
				l.versionBehind = nil
				imageCtx, cancel := context.WithCancel(ctx)
				l.imageCancel = cancel
				workers.Go(func() { c.prepareImage(imageCtx, l, entry.Image) })
			}
			l.capacity = listenerCapacity(cfg.Cap, entry)
			l.Label = entry
			l.retired = false
		}
	}
	for k, l := range c.labels {
		if _, keep := limits[k]; !keep {
			l.retired = true
			if l.imageCancel != nil {
				l.imageCancel()
			}
		}
	}
	for _, r := range c.runners {
		if !r.launched {
			r.cancel()
		}
	}
	c.scheduler.Reload(scheduler.Resources(cfg.Cap), limits)
	c.signal()
	select {
	case c.imagesChanged <- struct{}{}:
	default:
	}
	return nil
}

func (c *Controller) prepareImage(ctx context.Context, l *label, image string) {
	delay := time.Second
	for ctx.Err() == nil {
		err := launcher.EnsureImage(ctx, image)
		c.mu.Lock()
		if ctx.Err() != nil || l.Image != image {
			c.mu.Unlock()
			return
		}
		l.imageUnavailable = err != nil
		l.waitingImage = err != nil
		c.signal()
		c.mu.Unlock()
		if err == nil {
			select {
			case c.imagesChanged <- struct{}{}:
			default:
			}
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		delay = min(time.Minute, 2*delay)
	}
}

func listenerCapacity(cap config.Resources, l config.Label) int {
	n := int(min(int64(cap.CPUs/l.CPUs), cap.Memory/l.Memory, int64(math.MaxInt32)))
	if l.Max > 0 {
		n = min(l.Max, n)
	}
	return n
}
