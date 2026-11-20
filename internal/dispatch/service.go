package dispatch

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"github.com/alexcatdad/my-mac-ci/internal/config"
	"github.com/alexcatdad/my-mac-ci/internal/github"
	"github.com/alexcatdad/my-mac-ci/internal/isolation"
	"github.com/alexcatdad/my-mac-ci/internal/launcher"
	"github.com/alexcatdad/my-mac-ci/internal/usage"
)

// Serve recovers local runners and waits for a fresh isolation pass before
// connecting any GitHub listeners. The monitor also guards each actual spawn.
func Serve(ctx context.Context, cfg config.Config, auth github.Config, monitor *isolation.Monitor) error {
	if monitor == nil {
		return errors.New("isolation monitor is required")
	}
	dir, err := usage.DefaultDir()
	if err != nil {
		slog.Warn("job usage directory unavailable")
	}
	return serve(ctx, cfg, auth, monitor, github.Run, dir)
}

type listenFunc func(context.Context, github.Config, func(github.Update), func(error)) error

type isolationMonitor interface {
	Run(context.Context, func(isolation.Status)) error
	Current() bool
}

func serve(ctx context.Context, cfg config.Config, auth github.Config, monitor isolationMonitor, listen listenFunc, usageDir string) error {
	if cfg.Cap.CPUs <= 0 || cfg.Cap.Memory <= 0 {
		return errors.New("positive global limits are required")
	}
	ctx, cancel := context.WithCancel(ctx)
	controller := New(cfg, launcher.Start)
	if usageDir != "" {
		controller.measurements = &jobMeasurements{ctx: ctx, dir: usageDir, complete: usage.Complete,
			start: func(ctx context.Context, name string) measurement { return usage.Start(ctx, name) }}
	}
	var workers sync.WaitGroup
	defer func() {
		cancel()
		workers.Wait()
		controller.closeMeasurements()
	}()

	for _, labels := range cfg.Repos {
		for _, label := range labels {
			if label.CPUs <= 0 || label.Memory <= 0 || label.Max < 0 {
				return errors.New("invalid label limits")
			}
		}
	}

	survivors, err := launcher.Inventory(ctx)
	if err != nil {
		return err
	}
	if err := controller.recover(ctx, survivors, &workers); err != nil {
		return err
	}
	slog.Info("waiting for a passing isolation check before connecting to GitHub", "recovered_runners", len(survivors))
	controller.current = monitor.Current
	if detailed, ok := monitor.(interface {
		LastReport() (isolation.Report, int64)
	}); ok {
		controller.report = detailed.LastReport
	}
	if health, _ := ctx.Value(sessionKey{}).(*Health); health != nil {
		health.attach(controller)
		workers.Go(func() { controller.refreshVersions(ctx) })
	}
	workers.Go(func() { controller.Run(ctx) })
	stopped := make(chan error, 1)
	workers.Go(func() {
		var last isolation.Status
		err := monitor.Run(ctx, func(status isolation.Status) {
			controller.SetIsolation(status)
			if last != status {
				switch status {
				case isolation.Pass:
					slog.Info("isolation check passed")
				case isolation.Fail:
					slog.Warn("isolation check failed, stopping active jobs")
				default:
					slog.Warn("isolation check inconclusive, new runner starts paused")
				}
				last = status
			}
			controller.notifyListeners()
		})
		controller.SetIsolation(isolation.Inconclusive)
		select {
		case stopped <- err:
		case <-ctx.Done():
		}
	})
	return controller.serveListeners(ctx, cfg, auth, monitor, listen, &workers, stopped)
}
