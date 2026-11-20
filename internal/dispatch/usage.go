package dispatch

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/alexcatdad/my-mac-ci/internal/config"
	"github.com/alexcatdad/my-mac-ci/internal/github"
	"github.com/alexcatdad/my-mac-ci/internal/scheduler"
	"github.com/alexcatdad/my-mac-ci/internal/usage"
)

type measurement interface {
	Finish(string, usage.Job) error
	Discard()
}

type jobMeasurements struct {
	ctx      context.Context
	dir      string
	start    func(context.Context, string) measurement
	complete func(string, string, usage.Job) error
	last     chan struct{}
}

func allocation(key scheduler.Key, resources config.Resources, image string) usage.Job {
	_, digest, _ := strings.Cut(image, "@")
	return usage.Job{Repo: key.Repo, Label: key.Label, CPUs: resources.CPUs, MemoryBytes: resources.Memory,
		ImageDigest: digest, Outcome: "unknown"}
}

// Queue writes in delivery order. Sampling and scheduling do not wait for
// disk I/O. An exit record is committed before a late completion enriches it.
func (m *jobMeasurements) write(save func() error) {
	previous := m.last
	next := make(chan struct{})
	m.last = next
	go func() {
		defer close(next)
		if previous != nil {
			<-previous
		}
		if save() != nil {
			slog.Warn("job usage could not be recorded")
		}
	}()
}

func mergeEvent(job *usage.Job, event github.Job) {
	if event.Workflow != "" {
		job.Workflow = event.Workflow
	}
	if event.Display != "" {
		job.Job = event.Display
	}
	if event.Event != "" {
		job.Event = event.Event
	}
	if !event.AssignedAt.IsZero() {
		job.StartedAt = event.AssignedAt
	}
	if !event.FinishedAt.IsZero() {
		job.FinishedAt = event.FinishedAt
	}
	if event.Outcome != "" && event.Outcome != "unknown" {
		job.Outcome = event.Outcome
	}
}

func (c *Controller) startMeasurement(r *runner, event github.Job) {
	m := c.measurements
	if m == nil || r.measured {
		return
	}
	if !r.sampleAdopted.IsZero() {
		// A survivor might have been waiting for a job at adoption. Avoid
		// including that idle interval in the newly observed workload.
		if event.AssignedAt.IsZero() || event.AssignedAt.After(r.sampleAdopted) {
			c.discardMeasurement(r)
		}
		r.sampleAdopted = time.Time{}
	}
	if r.sample == nil {
		ctx, cancel := context.WithCancel(m.ctx)
		r.stopSample, r.sample = cancel, m.start(ctx, event.Runner)
	}
	mergeEvent(&r.job, event)
}

func (c *Controller) discardMeasurement(r *runner) {
	if r.sample != nil {
		r.stopSample()
		sample := r.sample
		r.sample = nil
		c.measurements.write(func() error { sample.Discard(); return nil })
	}
}

func (c *Controller) finishMeasurement(r *runner) {
	if r.sample == nil {
		return
	}
	if r.retiring && !r.sampleAdopted.IsZero() {
		c.discardMeasurement(r)
		return
	}
	r.stopSample()
	m, sample, job := c.measurements, r.sample, r.job
	r.sample, r.measured = nil, true
	m.write(func() error { return sample.Finish(m.dir, job) })
}

func (c *Controller) completeMeasurement(key scheduler.Key, event github.Job) {
	m := c.measurements
	if m == nil {
		return
	}
	r := c.runners[event.Runner]
	if r != nil && r.key != key {
		return
	}
	if r != nil && !r.measured {
		c.startMeasurement(r, event)
		c.finishMeasurement(r)
		return
	}
	if r == nil && !strings.HasPrefix(event.Runner, "mmc-") {
		return
	}
	// A completion can arrive after confirmed VM exit or helper restart.
	// Complete only updates a previously recorded MMC-owned runner.
	job := usage.Job{Repo: key.Repo, Label: key.Label, Outcome: "unknown"}
	if r != nil {
		job = r.job
	}
	mergeEvent(&job, event)
	m.write(func() error { return m.complete(m.dir, event.Runner, job) })
}

// Called after joining listeners, VM monitors, and the dispatch loop.
func (c *Controller) closeMeasurements() {
	if m := c.measurements; m != nil {
		for _, r := range c.runners {
			c.finishMeasurement(r)
		}
		if m.last != nil {
			<-m.last
		}
	}
}
