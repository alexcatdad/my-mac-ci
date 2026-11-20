package dispatch

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/alexcatdad/my-mac-ci/internal/config"
	"github.com/alexcatdad/my-mac-ci/internal/github"
	"github.com/alexcatdad/my-mac-ci/internal/isolation"
	"github.com/alexcatdad/my-mac-ci/internal/scheduler"
)

var labelKey = scheduler.Key{Repo: "example/project", Label: "linux"}

func dispatchConfig() config.Config {
	resources := config.Resources{CPUs: 1, Memory: 1 << 30}
	return config.Config{
		Registry: "example.invalid/ci", Cap: resources,
		Repos: map[string]map[string]config.Label{
			labelKey.Repo: {labelKey.Label: {Resources: resources, Image: "runner"}},
		},
	}
}

func TestRetryAndIsolation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		var calls atomic.Int32
		var runner context.Context
		c := New(dispatchConfig(), func(ctx context.Context, _ string, _ scheduler.Key, _ string, _ config.Resources, _ string, authorize func(func() error) error) (<-chan error, error) {
			if err := authorize(func() error { calls.Add(1); return nil }); err != nil {
				return nil, err
			}
			if calls.Load() == 1 {
				return nil, errors.New("synthetic failure")
			}
			runner = ctx
			exit := make(chan error, 1)
			go func() {
				<-ctx.Done()
				exit <- ctx.Err()
			}()
			return exit, nil
		})
		c.Update(labelKey, github.Update{Assigned: new(1), JIT: func(context.Context, string) (string, error) { return "test", nil }})
		go c.Run(ctx)
		synctest.Wait()
		if calls.Load() != 0 {
			t.Fatal("started without isolation result")
		}
		c.SetIsolation(isolation.Pass)
		synctest.Wait()
		if calls.Load() != 1 {
			t.Fatalf("initial calls = %d", calls.Load())
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if calls.Load() != 2 {
			t.Fatalf("calls after backoff = %d", calls.Load())
		}
		c.SetIsolation(isolation.Inconclusive)
		synctest.Wait()
		if runner.Err() != nil {
			t.Fatal("inconclusive check interrupted active job")
		}
		c.SetIsolation(isolation.Fail)
		synctest.Wait()
		if runner.Err() == nil || calls.Load() != 2 {
			t.Fatal("failed isolation must stop the active runner and block new starts")
		}
	})
}

func TestStaleJITDiscarded(t *testing.T) {
	for _, reason := range []string{"no demand", "lost session", "isolation changed"} {
		t.Run(reason, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				starts := 0
				c := New(dispatchConfig(), func(context.Context, string, scheduler.Key, string, config.Resources, string, func(func() error) error) (<-chan error, error) {
					starts++
					return nil, errors.New("unexpected start")
				})
				unblock := make(chan struct{})
				fetched := make(chan struct{})
				update := github.Update{Assigned: new(1), JIT: func(context.Context, string) (string, error) {
					close(fetched)
					<-unblock
					return "test", nil
				}}
				c.SetIsolation(isolation.Pass)
				c.Update(labelKey, update)
				go c.Run(ctx)
				<-fetched
				switch reason {
				case "no demand":
					update.Assigned = new(0)
					c.Update(labelKey, update)
				case "lost session":
					c.Update(labelKey, github.Update{})
				case "isolation changed":
					c.SetIsolation(isolation.Inconclusive)
				}
				close(unblock)
				synctest.Wait()
				if starts != 0 || len(c.runners) != 0 {
					t.Fatal("stale reservation launched or held capacity")
				}
			})
		})
	}
}

func TestIsolationDuringPreflight(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		checking := make(chan struct{})
		ready := make(chan struct{})
		spawned := false
		c := New(dispatchConfig(), func(_ context.Context, _ string, _ scheduler.Key, _ string, _ config.Resources, _ string, authorize func(func() error) error) (<-chan error, error) {
			close(checking)
			<-ready
			err := authorize(func() error { spawned = true; return nil })
			return nil, err
		})
		var fresh atomic.Bool
		fresh.Store(true)
		c.current = fresh.Load
		c.Update(labelKey, github.Update{Assigned: new(1), JIT: func(context.Context, string) (string, error) { return "test", nil }})
		c.SetIsolation(isolation.Pass)
		go c.Run(ctx)
		<-checking
		// A network event is pending but the monitor has not published it yet.
		fresh.Store(false)
		close(ready)
		synctest.Wait()
		if spawned || len(c.runners) != 0 {
			t.Fatal("started with inconclusive isolation or leaked a reservation")
		}
	})
}

func TestInconclusiveIsolationLetsActiveJobFinishWithoutReplacement(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var starts int
		var active context.Context
		var name string
		exit := make(chan error, 1)
		c := New(dispatchConfig(), func(ctx context.Context, id string, _ scheduler.Key, _ string, _ config.Resources, _ string, authorize func(func() error) error) (<-chan error, error) {
			return exit, authorize(func() error {
				starts++
				active, name = ctx, id
				return nil
			})
		})
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		update := github.Update{Assigned: new(1), JIT: func(context.Context, string) (string, error) { return "test", nil }}
		c.Update(labelKey, update)
		c.SetIsolation(isolation.Pass)
		go c.Run(ctx)
		synctest.Wait()
		if starts != 1 {
			t.Fatal("expected a runner before the recheck")
		}

		c.SetIsolation(isolation.Inconclusive)
		synctest.Wait()
		if active.Err() != nil {
			t.Fatal("inconclusive recheck canceled the active job")
		}
		update.Completed = []github.Job{{Runner: name}}
		c.Update(labelKey, update)
		exit <- nil
		synctest.Wait()
		if starts != 1 || len(c.runners) != 0 {
			t.Fatal("completion under inconclusive isolation started a replacement or held capacity")
		}
	})
}

func TestCompletionGraceUsesFirstNotification(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		var name string
		var active context.Context
		c := New(dispatchConfig(), func(ctx context.Context, id string, _ scheduler.Key, _ string, _ config.Resources, _ string, authorize func(func() error) error) (<-chan error, error) {
			exit := make(chan error, 1)
			err := authorize(func() error {
				name, active = id, ctx
				go func() {
					<-ctx.Done()
					exit <- ctx.Err()
				}()
				return nil
			})
			return exit, err
		})
		dispatch := github.Update{Assigned: new(1), JIT: func(context.Context, string) (string, error) { return "test", nil }}
		c.Update(labelKey, dispatch)
		c.SetIsolation(isolation.Pass)
		go c.Run(ctx)
		synctest.Wait()
		dispatch.Assigned = new(0)
		dispatch.Completed = []github.Job{{Runner: name}}
		c.Update(labelKey, dispatch)
		time.Sleep(29 * time.Second)
		c.Update(labelKey, dispatch)
		if active.Err() != nil {
			t.Fatal("runner stopped before grace elapsed")
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if active.Err() == nil || len(c.runners) != 0 {
			t.Fatal("duplicate completion extended grace or leaked capacity")
		}
	})
}

func TestCleanExitWithoutCompletionBacksOff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		var starts atomic.Int32
		exit := make(chan error)
		c := New(dispatchConfig(), func(_ context.Context, _ string, _ scheduler.Key, _ string, _ config.Resources, _ string, authorize func(func() error) error) (<-chan error, error) {
			return exit, authorize(func() error { starts.Add(1); return nil })
		})
		c.Update(labelKey, github.Update{Assigned: new(1), JIT: func(context.Context, string) (string, error) { return "test", nil }})
		c.SetIsolation(isolation.Pass)
		go c.Run(ctx)
		exit <- nil
		synctest.Wait()
		if starts.Load() != 1 {
			c.Update(labelKey, github.Update{})
			close(exit)
			t.Fatal("immediate respawn after an exit without JobCompleted")
		}
		time.Sleep(time.Second)
		synctest.Wait()
		c.Update(labelKey, github.Update{})
		close(exit)
		if starts.Load() != 2 {
			t.Fatal("no retry after the backoff expired")
		}
	})
}
