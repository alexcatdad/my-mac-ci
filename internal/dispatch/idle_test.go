package dispatch

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/alexcatdad/my-mac-ci/internal/github"
)

func TestSurplusGraceAndGuardedRemoval(t *testing.T) {
	for _, mode := range []string{"removed", "assigned", "ambiguous", "reconnected", "restored demand", "started"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				service, shutdown := context.WithCancel(t.Context())
				defer shutdown()
				job, stop := context.WithCancel(service)
				c := New(dispatchConfig(), nil)
				c.runners["mmc-prior"] = &runner{key: labelKey, launched: true, working: true, recovered: true, cancel: stop}
				var calls atomic.Int32
				u := github.Update{Assigned: new(1), JIT: func(context.Context, string) (string, error) { return "", nil }, RemoveIdle: func(ctx context.Context, name string) (bool, error) {
					calls.Add(1)
					if name != "mmc-prior" {
						t.Error("wrong registration")
					}
					if mode == "ambiguous" {
						return false, errors.New("synthetic failure")
					}
					return mode != "assigned", nil
				}}
				c.Update(labelKey, u)
				go c.Run(service)
				synctest.Wait()
				u.Assigned = new(0)
				c.Update(labelKey, u)
				synctest.Wait()
				time.Sleep(29 * time.Second)
				if job.Err() != nil || calls.Load() != 0 {
					t.Fatal("removed early")
				}
				switch mode {
				case "restored demand":
					u.Assigned = new(1)
					c.Update(labelKey, u)
				case "reconnected":
					c.Update(labelKey, github.Update{})
					u.Assigned = nil
					c.Update(labelKey, u)
				case "started":
					u.Started = []github.Job{{Runner: "mmc-prior"}}
					c.Update(labelKey, u)
				}
				time.Sleep(time.Second)
				synctest.Wait()
				if (job.Err() != nil) != (mode == "removed") {
					t.Fatal("local stop requires successful service removal")
				}
				switch mode {
				case "ambiguous":
					time.Sleep(30 * time.Second)
					synctest.Wait()
					if calls.Load() != 2 {
						t.Fatal("ambiguous state did not retry")
					}
				case "assigned", "removed":
					if calls.Load() != 1 {
						t.Fatal("missing service check")
					}
				default:
					if calls.Load() != 0 {
						t.Fatal("checked a needed runner or stale demand")
					}
				}
			})
		})
	}
}
