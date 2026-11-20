package main

import (
	"context"
	"sync"

	"github.com/alexcatdad/my-mac-ci/internal/launcher"
)

type quitCheck struct {
	cancel context.CancelFunc
	stop   context.CancelFunc
	result chan string
	last   string
}

func (s *appSession) quitCommand(ctx context.Context, command string) string {
	if command == "quit-resume" {
		if s.quit != nil && s.quit.cancel != nil {
			s.quit.cancel()
		}
		s.quit = nil
		if s.health != nil {
			s.health.Resume()
		}
		return "ready"
	}
	if s.quit == nil {
		s.quit = &quitCheck{last: "pending"}
	}
	if s.done != nil {
		active, attached := s.health.PrepareQuit(command == "quit-stop")
		if !attached {
			return "pending"
		}
		if active {
			return "busy"
		}
		return "ready"
	}
	// A helper can be awaiting credentials or recovering from a failed
	// dispatch with surviving jobs. Inspect those without opening a listener.
	q := s.quit
	if q.result == nil {
		var watch, stop context.Context
		watch, q.cancel = context.WithCancel(ctx)
		stop, q.stop = context.WithCancel(watch)
		q.result = make(chan string, 2)
		go func() {
			survivors, err := launcher.Inventory(watch)
			if err != nil {
				q.result <- "unavailable"
				return
			}
			if len(survivors) > 0 {
				q.result <- "busy"
				var jobs sync.WaitGroup
				for _, job := range survivors {
					jobs.Go(func() { job.Wait(watch, stop) })
				}
				jobs.Wait()
			}
			q.result <- "ready"
		}()
	}
	if command == "quit-stop" {
		q.stop()
	}
	select {
	case q.last = <-q.result:
	default:
	}
	return q.last
}
