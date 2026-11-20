package dispatch

import (
	"context"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/alexcatdad/my-mac-ci/internal/config"
	"github.com/alexcatdad/my-mac-ci/internal/github"
	"github.com/alexcatdad/my-mac-ci/internal/isolation"
	"github.com/alexcatdad/my-mac-ci/internal/scheduler"
)

func TestQuitGateAndDetach(t *testing.T) {
	for _, stop := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			service, shutdown := context.WithCancel(t.Context())
			defer shutdown()
			var starts atomic.Int32
			exit := make(chan error, 1)
			var job context.Context
			c := New(dispatchConfig(), func(ctx context.Context, _ string, _ scheduler.Key, _ string, _ config.Resources, _ string, authorize func(func() error) error) (<-chan error, error) {
				return exit, authorize(func() error { starts.Add(1); job = ctx; return nil })
			})
			h := new(Health)
			if _, attached := h.PrepareQuit(false); attached {
				t.Fatal("unexpected controller")
			}
			h.attach(c)
			c.SetIsolation(isolation.Pass)
			c.Update(labelKey, github.Update{Assigned: new(1), JIT: func(context.Context, string) (string, error) { return "synthetic", nil }})
			go c.Run(service)
			synctest.Wait()
			if starts.Load() != 0 {
				t.Fatal("quit during inventory allowed a start")
			}
			h.Resume()
			synctest.Wait()
			if starts.Load() != 1 {
				t.Fatal("cancel did not resume scheduling")
			}
			if active, attached := h.PrepareQuit(stop); !active || !attached {
				t.Fatal("launched job missing")
			}
			if stop {
				if job.Err() == nil {
					t.Fatal("stop did not reach runner")
				}
				exit <- nil
				synctest.Wait()
				if active, _ := h.PrepareQuit(false); active {
					t.Fatal("exited job still counted")
				}
			} else {
				shutdown()
				synctest.Wait()
				if job.Err() != nil {
					t.Fatal("losing app lifeline stopped active job")
				}
				exit <- nil
			}
			if starts.Load() != 1 {
				t.Fatal("new start during quit")
			}
		})
	}
}
