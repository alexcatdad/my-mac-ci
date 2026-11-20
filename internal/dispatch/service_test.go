package dispatch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/actions/scaleset"
	"github.com/alexcatdad/my-mac-ci/internal/config"
	"github.com/alexcatdad/my-mac-ci/internal/github"
	"github.com/alexcatdad/my-mac-ci/internal/isolation"
)

type testMonitor struct {
	status chan isolation.Status
	fresh  atomic.Bool
}

func (m *testMonitor) Current() bool { return m.fresh.Load() }

func (m *testMonitor) Run(ctx context.Context, publish func(isolation.Status)) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case status, open := <-m.status:
			if !open {
				return errors.New("monitor unavailable")
			}
			m.fresh.Store(status == isolation.Pass)
			publish(status)
		}
	}
}

// Only the GitHub transport and Apple container executable are substitutes.
type broker struct {
	messages chan *scaleset.RunnerScaleSetMessage
	acquired chan int64
	acked    chan int
}

func (b *broker) Session() scaleset.RunnerScaleSetSession {
	return scaleset.RunnerScaleSetSession{SessionID: [16]byte{1}, Statistics: &scaleset.RunnerScaleSetStatistic{}}
}

func (b *broker) GetMessage(ctx context.Context, _, _ int) (*scaleset.RunnerScaleSetMessage, error) {
	select {
	case m := <-b.messages:
		return m, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (b *broker) DeleteMessage(_ context.Context, id int) error {
	b.acked <- id
	return nil
}

func (b *broker) AcquireJobs(_ context.Context, ids []int64) ([]int64, error) {
	for _, id := range ids {
		b.acquired <- id
	}
	return ids, nil
}

func receive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a lifecycle event")
		var zero T
		return zero
	}
}

func TestOneJobLifecycle(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir+":/usr/bin:/bin")
	t.Setenv("MMC_TEST_DIR", dir)
	const runtime = `#!/bin/sh
set -eu
case "$1" in
image) test "$2" = inspect ;;
run)
    name=
    for arg do
        case "$arg" in
            mmc-*) name=$arg ;;
            com.alexcatdad.mmc.runner=*) owner=${arg#*=} ;;
            *synthetic-ticket*) exit 1 ;;
        esac
    done
    test -n "$name"
    IFS= read -r ticket
    test "$ticket" = synthetic-ticket
    printf '[{"id":"%s","configuration":{"id":"%s","labels":{"com.alexcatdad.mmc.runner":"%s"}},"status":{"state":"running","networks":[]}}]\n' "$name" "$name" "$owner" > "$MMC_TEST_DIR/inventory"
    printf '%s\n' "$name" >> "$MMC_TEST_DIR/starts"
    printf '%s\n' "$((6 * 7))" > "$MMC_TEST_DIR/result"
    while [ ! -f "$MMC_TEST_DIR/exit" ]; do sleep 0.01; done
    ;;
list)
    if [ ! -f "$MMC_TEST_DIR/inventory" ] || [ -f "$MMC_TEST_DIR/removed" ]; then
        printf '[]\n'
    else
        cat "$MMC_TEST_DIR/inventory"
        : > "$MMC_TEST_DIR/inspected"
    fi
    ;;
*) exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "container"), []byte(runtime), 0700); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{Registry: "registry.invalid", Cap: config.Resources{CPUs: 1, Memory: 1 << 30},
		Repos: map[string]map[string]config.Label{"example/project": {"linux": {
			Resources: config.Resources{CPUs: 1, Memory: 1 << 30}, Image: "runner@sha256:" + strings.Repeat("0", 64),
		}}}}
	b := broker{make(chan *scaleset.RunnerScaleSetMessage, 4), make(chan int64, 1), make(chan int, 4)}
	tickets := make(chan string, 4)
	allowTicket := true
	listen := func(ctx context.Context, cfg github.Config, update func(github.Update), _ func(error)) error {
		if cfg.MaxRunners != 1 || cfg.Repo != "example/project" || cfg.Label != "linux" {
			return errors.New("incorrect listener configuration")
		}
		return github.Listen(ctx, &b, 1, cfg.MaxRunners, func(ctx context.Context, name string) (string, error) {
			tickets <- name
			if allowTicket {
				allowTicket = false
				return "synthetic-ticket", nil
			}
			<-ctx.Done()
			return "", ctx.Err()
		}, update)
	}
	ctx, cancel := context.WithCancel(t.Context())
	m := &testMonitor{status: make(chan isolation.Status)}
	done := make(chan error, 1)
	go func() { done <- serve(ctx, cfg, github.Config{}, m, listen, "") }()
	t.Cleanup(func() {
		_ = os.WriteFile(filepath.Join(dir, "removed"), nil, 0600)
		_ = os.WriteFile(filepath.Join(dir, "exit"), nil, 0600)
		cancel()
		if err := receive(t, done); !errors.Is(err, context.Canceled) {
			t.Errorf("unexpected service exit: %v", err)
		}
	})
	send := func(id, assigned int, event *scaleset.RunnerScaleSetMessage) {
		t.Helper()
		event.MessageID = id
		event.Statistics = &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: assigned}
		b.messages <- event
		if receive(t, b.acked) != id {
			t.Fatal("wrong acknowledgement")
		}
	}
	noTicket := func() {
		t.Helper()
		select {
		case <-tickets:
			t.Fatal("requested a runner while starts should be blocked")
		case <-time.After(75 * time.Millisecond):
		}
	}
	m.status <- isolation.Inconclusive
	b.messages <- &scaleset.RunnerScaleSetMessage{MessageID: 1, Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 1}, JobAvailableMessages: []*scaleset.JobAvailable{
		{JobMessageBase: scaleset.JobMessageBase{RunnerRequestID: 7}},
	}}
	noTicket()
	select {
	case <-b.acquired:
		t.Fatal("job was acquired before the first isolation pass")
	default:
	}
	m.status <- isolation.Pass
	if receive(t, b.acked) != 1 || receive(t, b.acquired) != 7 {
		t.Fatal("job was not acquired")
	}
	name := receive(t, tickets)
	waitFile := func(file, want string) {
		t.Helper()
		until := time.Now().Add(5 * time.Second)
		for {
			b, err := os.ReadFile(filepath.Join(dir, file))
			if err == nil && string(b) == want {
				return
			}
			if time.Now().After(until) {
				t.Fatalf("timed out waiting for fake runtime event %s", file)
			}
			time.Sleep(time.Millisecond)
		}
	}
	waitFile("result", "42\n")
	send(2, 1, &scaleset.RunnerScaleSetMessage{JobStartedMessages: []*scaleset.JobStarted{{RunnerName: name}}})
	// The same poll reports completion and demand for a successor. The
	// completed container still owns the only CPU until the runtime exits.
	send(3, 1, &scaleset.RunnerScaleSetMessage{JobCompletedMessages: []*scaleset.JobCompleted{{RunnerName: name}}})
	noTicket()
	if err := os.WriteFile(filepath.Join(dir, "exit"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	waitFile("inspected", "")
	noTicket() // CLI exited, but its VM is still in the runtime inventory.
	if err := os.WriteFile(filepath.Join(dir, "removed"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if next := receive(t, tickets); next == name {
		t.Fatal("exit did not release capacity for a fresh reservation")
	}
	send(4, 0, &scaleset.RunnerScaleSetMessage{})
	starts, err := os.ReadFile(filepath.Join(dir, "starts"))
	if err != nil || string(starts) != name+"\n" {
		t.Fatal("expected exactly one container start")
	}
}

func TestRecoverBeforeListening(t *testing.T) {
	const inventory = `[{"id":"mmc-prior","configuration":{"id":"mmc-prior","labels":{"com.alexcatdad.mmc.runner":"synthetic","com.alexcatdad.mmc.repo":"example/project","com.alexcatdad.mmc.label":"linux"},"resources":{"cpus":1,"memoryInBytes":1073741824}},"status":{"state":"running"}}]`
	for _, outcome := range []string{"finishes", "isolation failure", "failure before monitor exit", "monitor unavailable", "incomplete inventory"} {
		t.Run(outcome, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("PATH", dir+":/usr/bin:/bin")
			t.Setenv("MMC_TEST_DIR", dir)
			const runtime = `#!/bin/sh
set -eu
case "$1" in
list) if [ -f "$MMC_TEST_DIR/gone" ]; then echo '[]'; else cat "$MMC_TEST_DIR/inventory"; fi ;;
rm) test "$2" = -f && test "$3" = mmc-prior
    touch "$MMC_TEST_DIR/stopped" "$MMC_TEST_DIR/gone" ;;
*) exit 1 ;;
esac
`
			if err := os.WriteFile(filepath.Join(dir, "container"), []byte(runtime), 0700); err != nil {
				t.Fatal(err)
			}
			snapshot := inventory
			if outcome == "incomplete inventory" {
				snapshot = strings.ReplaceAll(snapshot, "memoryInBytes", "missing")
			}
			if err := os.WriteFile(filepath.Join(dir, "inventory"), []byte(snapshot), 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			m := &testMonitor{status: make(chan isolation.Status)}
			connected := make(chan struct{}, 1)
			tickets := make(chan struct{}, 1)
			listen := func(ctx context.Context, _ github.Config, update func(github.Update), _ func(error)) error {
				update(github.Update{Assigned: new(1), Completed: []github.Job{{Runner: "mmc-prior"}}, JIT: func(ctx context.Context, _ string) (string, error) {
					tickets <- struct{}{}
					<-ctx.Done()
					return "", ctx.Err()
				}})
				connected <- struct{}{}
				<-ctx.Done()
				return ctx.Err()
			}
			if outcome == "failure before monitor exit" {
				m.status = make(chan isolation.Status, 1)
				m.status <- isolation.Fail
				close(m.status)
				if serve(ctx, dispatchConfig(), github.Config{}, m, listen, "") == nil {
					t.Fatal("service accepted a stopped monitor")
				}
				if _, err := os.Stat(filepath.Join(dir, "stopped")); err != nil || len(connected) != 0 {
					t.Fatal("monitor exit abandoned a confirmed isolation failure")
				}
				return
			}
			if outcome == "monitor unavailable" || outcome == "incomplete inventory" {
				close(m.status)
				if err := serve(ctx, dispatchConfig(), github.Config{}, m, listen, ""); err == nil {
					t.Fatal("service accepted incomplete startup")
				}
				if _, err := os.Stat(filepath.Join(dir, "stopped")); !errors.Is(err, os.ErrNotExist) || len(connected) != 0 {
					t.Fatal("incomplete startup changed the running job or opened a listener")
				}
				return
			}
			done := make(chan error, 1)
			go func() { done <- serve(ctx, dispatchConfig(), github.Config{}, m, listen, "") }()
			t.Cleanup(func() {
				cancel()
				if err := receive(t, done); !errors.Is(err, context.Canceled) {
					t.Errorf("service exit: %v", err)
				}
			})
			m.status <- isolation.Pass
			receive(t, connected)
			select {
			case <-tickets:
				t.Fatal("JobCompleted released the survivor's capacity before exit")
			case <-time.After(75 * time.Millisecond):
			}
			if outcome == "isolation failure" {
				m.status <- isolation.Inconclusive
				m.status <- isolation.Fail
				deadline := time.Now().Add(3 * time.Second)
				for {
					if _, err := os.Stat(filepath.Join(dir, "stopped")); err == nil {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("confirmed isolation failure did not stop the survivor")
					}
					time.Sleep(5 * time.Millisecond)
				}
				m.status <- isolation.Pass
			} else if err := os.WriteFile(filepath.Join(dir, "gone"), nil, 0600); err != nil {
				t.Fatal(err)
			}
			receive(t, tickets)
		})
	}
}
